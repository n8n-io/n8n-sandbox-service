package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/n8n-io/sandbox-service/internal/daemon/volumebuild"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
	"github.com/n8n-io/sandbox-service/internal/shellquote"
)

// A runner builds one volume image at a time, so the builder's host names are
// fixed: the next build and startup reconcile clear what a build left behind.
// No sandbox uses them: sandbox IDs are UUIDs and their jails "sandbox-<hash>".
const (
	volumeBuildID    = "volume-build"
	volumeBuildNetns = "fc-volume-build"
)

// volumeBuildKernelPath is where the builder's jail has the template kernel.
const volumeBuildKernelPath = "/kernel"

// volumeBuildReplayTimeout ends a journal replay of the rootfs clone that
// hangs. One takes about 0.1 s.
const volumeBuildReplayTimeout = time.Minute

// volumeBuildDrives are the builder's drives in the order the guest expects
// them. Firecracker cannot write the read-only ones: their files are also
// root-owned, mode 0444, and bound read-only into the jail.
var volumeBuildDrives = [...]struct {
	id       string
	jailPath string
	readOnly bool
}{
	volumebuild.DriveRootfs: {"rootfs", "/rootfs.ext4", true},
	volumebuild.DriveTar:    {"tar", "/tar", true},
	volumebuild.DriveOutput: {"output", "/image.ext4", false},
	volumebuild.DriveStatus: {"status", "/status", false},
}

// volumeBuildGuestReasons are the reasons a builder may report. Nothing else
// from its status record becomes a metric label or reaches the API.
var volumeBuildGuestReasons = []string{
	volumebuild.ReasonTar, volumebuild.ReasonTooLarge, volumebuild.ReasonNoSpace,
	volumebuild.ReasonFormat, volumebuild.ReasonMount, volumebuild.ReasonFsck, volumebuild.ReasonInternal,
}

// Steps of a volume build, as named in its log event.
const (
	stepReceiveTar   = "receive_tar"
	stepPrepareBuild = "prepare"
	stepStartBuilder = "start_builder"
	stepBuild        = "build"
	stepDeliverImage = "deliver"
)

var _ runnerruntime.VolumeBuilder = (*Runtime)(nil)

// volumeBuild is the runner's one running build.
type volumeBuild struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// volumeBuildFiles are the build's files on the data volume and its jail.
type volumeBuildFiles struct {
	dir      string
	drives   [len(volumeBuildDrives)]string
	jailDir  string
	jailRoot string
	socket   string
}

func (r *Runtime) volumeBuildFiles() volumeBuildFiles {
	dir := filepath.Join(r.runnerConfig.DataDir, volumeBuildID)
	jailDir := filepath.Join(r.config.JailerBaseDir, "firecracker", volumeBuildID)
	files := volumeBuildFiles{
		dir:      dir,
		jailDir:  jailDir,
		jailRoot: filepath.Join(jailDir, "root"),
		socket:   filepath.Join(jailDir, "root", "firecracker.socket"),
	}
	for drive, spec := range volumeBuildDrives {
		files.drives[drive] = filepath.Join(dir, strings.TrimPrefix(spec.jailPath, "/"))
	}
	return files
}

// volumeBuilderBoot is what the builder microVM boots with.
type volumeBuilderBoot struct {
	vcpuCount  int
	memSizeMiB int
	bootArgs   string
}

// volumeBuilderBootArgs is the golden snapshot's kernel command line, so the
// builder boots the daemon as sandboxes do, with two changes: no ip=, which
// stalls the boot of a guest without an interface, and the build parameters.
// Those go first, where no "--" in the rest can turn them into arguments.
func volumeBuilderBootArgs(golden string, params volumebuild.Params) string {
	args := []string{params.KernelArgs()}
	for arg := range strings.FieldsSeq(golden) {
		if !strings.HasPrefix(arg, "ip=") {
			args = append(args, arg)
		}
	}
	return strings.Join(args, " ")
}

// bootVolumeBuilder configures and starts the builder microVM: the golden
// snapshot's machine without a network interface, and the four build drives.
func bootVolumeBuilder(ctx context.Context, socketPath string, boot volumeBuilderBoot) error {
	client := newFirecrackerAPIClient(socketPath)
	put := func(path string, body any) error {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode firecracker %s request: %w", path, err)
		}
		if err := client.putJSON(ctx, path, raw); err != nil {
			return fmt.Errorf("firecracker %s: %w", path, err)
		}
		return nil
	}

	if err := put("/machine-config", machineConfigRequest{VCPUCount: boot.vcpuCount, MemSizeMiB: boot.memSizeMiB, SMT: false}); err != nil {
		return err
	}
	if err := put("/boot-source", bootSourceRequest{KernelImagePath: volumeBuildKernelPath, BootArgs: boot.bootArgs}); err != nil {
		return err
	}
	for drive, spec := range volumeBuildDrives {
		if err := put("/drives/"+spec.id, driveRequest{
			DriveID:      spec.id,
			PathOnHost:   spec.jailPath,
			IsRootDevice: drive == volumebuild.DriveRootfs,
			IsReadOnly:   spec.readOnly,
		}); err != nil {
			return err
		}
	}
	return put("/actions", actionRequest{ActionType: "InstanceStart"})
}

// BuildVolume turns the tar into an ext4 image in a builder microVM: a cold
// boot of the template rootfs with no network, whose init is the daemon in
// build mode. The host never unpacks the tar and never mounts or parses the
// image. A build takes no sandbox slot.
func (r *Runtime) BuildVolume(ctx context.Context, params volumebuild.Params, tar io.Reader, deliver func(image io.Reader, size int64) error) (err error) {
	if err := params.Validate(); err != nil {
		return fmt.Errorf("volume build parameters: %w", err)
	}
	imageBytes := volumebuild.ImageBytes(params)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	build := &volumeBuild{cancel: cancel, done: make(chan struct{})}
	if err := r.claimVolumeBuild(build, params, imageBytes); err != nil {
		return err
	}
	defer r.releaseVolumeBuild(build)

	files := r.volumeBuildFiles()
	timer := newStepTimer("volume_build", nil)
	sizes := []any{"tar_bytes", params.TarBytes, "entries", params.Entries, "content_bytes", params.ContentBytes,
		"decompressed_bytes", params.DecompressedBytes, "image_bytes", imageBytes}
	slog.InfoContext(ctx, "firecracker volume build started", sizes...)

	var builder process
	var status volumebuild.Status
	defer func() {
		r.cleanupVolumeBuild(ctx, builder, files)
		attrs := append(sizes, timer.attrsFor(ctx)...)
		if err != nil {
			slog.WarnContext(ctx, "firecracker volume build failed", append(attrs, "err", err)...)
			return
		}
		// Numbers only: nothing else in the status record is trusted.
		slog.InfoContext(ctx, "firecracker volume build finished", append(attrs,
			"files", status.Files, "bytes", status.Bytes, "used_bytes", status.UsedBytes,
			"guest_format_ms", status.FormatMs, "guest_extract_ms", status.ExtractMs,
			"guest_sync_ms", status.SyncMs, "guest_fsck_ms", status.FsckMs)...)
	}()

	golden, err := r.deps.loadBootParams(bootParamsPath(r.config))
	if err != nil {
		return err
	}
	// Admission checked the kernel, but the builder boots the file now, as a
	// sandbox cold boot does.
	if err := r.deps.checkTemplateKernel(templateKernelPath(r.config)); err != nil {
		return err
	}
	boot := volumeBuilderBoot{
		vcpuCount:  golden.VCPUCount,
		memSizeMiB: golden.MemSizeMiB,
		bootArgs:   volumeBuilderBootArgs(golden.BootArgs, params),
	}

	if err := timer.step(stepReceiveTar, func() error { return receiveVolumeTar(ctx, files, params, tar) }); err != nil {
		return fmt.Errorf("receive tar: %w", err)
	}
	if err := timer.step(stepPrepareBuild, func() error { return r.prepareVolumeBuild(ctx, files, imageBytes) }); err != nil {
		return fmt.Errorf("prepare volume build: %w", err)
	}

	// One time limit from the jailer's start to the builder's exit: the
	// Firecracker API calls have none of their own.
	builderCtx, cancelBuilder := context.WithTimeout(ctx, r.config.VolumeBuildTimeout)
	defer cancelBuilder()
	exited := make(chan struct{})
	err = timer.step(stepStartBuilder, func() error {
		jail := &sandboxState{vmID: volumeBuildID, netnsName: volumeBuildNetns}
		var startErr error
		if builder, startErr = r.startJailer(builderCtx, jail, func(error) { close(exited) }); startErr != nil {
			return fmt.Errorf("start firecracker jailer: %w", startErr)
		}
		if err := r.waitForSocket(builderCtx, files.socket); err != nil {
			return fmt.Errorf("wait for firecracker socket: %w", err)
		}
		return r.deps.bootVolumeBuilder(builderCtx, files.socket, boot)
	})
	if err != nil {
		if r.volumeBuilderTimedOut(ctx, builderCtx) {
			return r.volumeBuildTimeout(fmt.Errorf("start volume builder: %w", err))
		}
		return fmt.Errorf("start volume builder: %w", err)
	}

	// The guest reboots when it is done, which ends Firecracker. Its exit
	// status says nothing: the status record does.
	if err := timer.step(stepBuild, func() error { return r.waitForVolumeBuilder(ctx, builderCtx, exited) }); err != nil {
		return err
	}
	if status, err = readVolumeBuildStatus(files.drives[volumebuild.DriveStatus]); err != nil {
		return &runnerruntime.VolumeBuildError{Reason: volumebuild.ReasonInternal, Err: fmt.Errorf("the builder left no usable status record: %w", err)}
	}
	if status.Result != volumebuild.ResultOK {
		reason := status.Reason
		if !slices.Contains(volumeBuildGuestReasons, reason) {
			reason = volumebuild.ReasonInternal
		}
		return &runnerruntime.VolumeBuildError{Reason: reason, Err: errors.New("reported by the builder")}
	}

	return timer.step(stepDeliverImage, func() error {
		return deliverVolumeImage(files.drives[volumebuild.DriveOutput], imageBytes, deliver)
	})
}

// claimVolumeBuild makes build the runner's one build, or refuses with an
// error wrapping ErrCapacityExhausted. Nothing has been read or created yet,
// so the API can send the tar to another runner.
func (r *Runtime) claimVolumeBuild(build *volumeBuild, params volumebuild.Params, imageBytes int64) error {
	r.volumeBuildMu.Lock()
	if r.volumeBuild != nil {
		r.volumeBuildMu.Unlock()
		return runnerruntime.ErrVolumeBuildBusy
	}
	r.volumeBuild = build
	r.volumeBuildMu.Unlock()

	if err := r.checkVolumeBuildDiskSpace(volumebuild.DriveBytes(params.TarBytes) + imageBytes); err != nil {
		r.releaseVolumeBuild(build)
		return err
	}
	return nil
}

func (r *Runtime) releaseVolumeBuild(build *volumeBuild) {
	r.volumeBuildMu.Lock()
	defer r.volumeBuildMu.Unlock()
	if r.volumeBuild == build {
		r.volumeBuild = nil
		close(build.done)
	}
}

// stopVolumeBuild ends a running build, so no builder microVM outlives the
// runner. The wait it returns blocks until the build has cleaned up or ctx
// is done.
func (r *Runtime) stopVolumeBuild() (wait func(ctx context.Context)) {
	r.volumeBuildMu.Lock()
	build := r.volumeBuild
	r.volumeBuildMu.Unlock()
	if build == nil {
		return func(context.Context) {}
	}
	build.cancel()
	return func(ctx context.Context) {
		select {
		case <-build.done:
		case <-ctx.Done():
			// Both can be ready, and select then picks either.
			select {
			case <-build.done:
			default:
				slog.Warn("firecracker volume build still cleaning up at shutdown; startup reconcile removes the rest")
			}
		}
	}
}

// checkVolumeBuildDiskSpace refuses a build whose tar and image would take
// the data volume below the floor creates keep, with what the volume cache
// sets aside counted as taken, as for a create. The image is sparse, but a
// build may fill it. The rootfs clone is not counted: like a sandbox's, it
// shares the template's blocks where the volume can.
func (r *Runtime) checkVolumeBuildDiskSpace(needed int64) error {
	space, err := r.deps.volumeSpace(r.runnerConfig.DataDir)
	if err != nil {
		return err
	}
	var reserved int64
	if r.diskReserved != nil {
		reserved = r.diskReserved()
	}
	floor := space.total * runnerruntime.DiskFloorPercent / 100
	// The reservation is compared, not subtracted, so none overflows; free,
	// floor and needed are far below the int64 range.
	switch {
	case space.free-floor-needed >= reserved:
		return nil
	case reserved > 0:
		return fmt.Errorf("firecracker data volume has %d MiB free; less the %d MiB set aside for the volume cache (SANDBOX_RUNNER_VOLUME_CACHE_MAX_BYTES), that is too little for a volume build of %d MiB above its %d MiB floor: %w",
			space.free>>20, reserved>>20, needed>>20, floor>>20, runnerruntime.ErrCapacityExhausted)
	default:
		return fmt.Errorf("firecracker data volume has %d MiB free, too little for a volume build of %d MiB above its %d MiB floor: %w",
			space.free>>20, needed>>20, floor>>20, runnerruntime.ErrCapacityExhausted)
	}
}

// receiveVolumeTar writes the tar into the build's fresh directory, padded
// with zeros to the size of its drive.
func receiveVolumeTar(ctx context.Context, files volumeBuildFiles, params volumebuild.Params, tar io.Reader) error {
	if err := removeSandboxDataDir(ctx, files.dir); err != nil {
		return fmt.Errorf("remove an earlier build's files: %w", err)
	}
	// Not MkdirAll: a missing data dir can mean its volume is not mounted.
	if err := os.Mkdir(files.dir, 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(files.drives[volumebuild.DriveTar], os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	// One byte more than the tar may have, to notice a longer one.
	written, err := io.Copy(file, io.LimitReader(tar, params.TarBytes+1))
	if err != nil {
		return err
	}
	if written > params.TarBytes {
		return fmt.Errorf("the tar is longer than its %d bytes", params.TarBytes)
	}
	if written < params.TarBytes {
		return fmt.Errorf("the tar ended after %d of %d bytes", written, params.TarBytes)
	}
	if err := file.Truncate(volumebuild.DriveBytes(params.TarBytes)); err != nil {
		return err
	}
	return file.Close()
}

// prepareVolumeBuild creates the other drives and the builder's jail.
func (r *Runtime) prepareVolumeBuild(ctx context.Context, files volumeBuildFiles, imageBytes int64) error {
	for drive, size := range map[int]int64{volumebuild.DriveOutput: imageBytes, volumebuild.DriveStatus: volumebuild.StatusBytes} {
		if err := createSparseFile(files.drives[drive], size); err != nil {
			return err
		}
	}

	rootfs := files.drives[volumebuild.DriveRootfs]
	if err := r.deps.cloneRootfs(ctx, filepath.Join(r.config.TemplateDir, "rootfs.ext4"), rootfs); err != nil {
		return fmt.Errorf("clone template rootfs: %w", err)
	}
	// The golden snapshot's microVM had the template mounted read-write when
	// it was stopped, so its journal asks for a replay, and the kernel refuses
	// to mount such a file system from a read-only drive. Replaying it here,
	// on this build's own clone, lets the builder boot it read-only. The
	// template itself must stay as the golden snapshot's memory knows it.
	replay := `PATH="$PATH:/usr/sbin:/sbin" exec e2fsck -E journal_only -y -- ` + shellquote.Quote(rootfs)
	replayCtx, cancel := context.WithTimeout(ctx, volumeBuildReplayTimeout)
	defer cancel()
	if err := r.deps.run(replayCtx, "/bin/sh", "-c", replay); err != nil {
		if errors.Is(replayCtx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("ended after %s: %w", volumeBuildReplayTimeout, err)
		}
		return fmt.Errorf("replay the journal of the rootfs clone: %w", err)
	}

	return r.deps.run(ctx, "sudo", "/bin/sh", "-c", volumeBuildJailScript(r.config, files, os.Getgid()))
}

func createSparseFile(path string, size int64) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Truncate(size); err != nil {
		return err
	}
	return file.Close()
}

// volumeBuildJailScript sets up the builder's jail and its empty network
// namespace, after clearing what an earlier build left of them.
//
// The tar and the rootfs clone become root-owned and read-only before they are
// bound, so Firecracker's uid can write neither whatever a guest does to the
// VMM; the kernel is bound read-only too. chown and chmod name the files in
// the build directory, never a bind target, and the kernel is not touched:
// every sandbox shares it. The binds exist before jailer starts, which is
// what makes it carry their read-only flag into the jail. The output and
// status files go to Firecracker's uid and the runner's group, so the runner
// reads them back without their being world-readable.
func volumeBuildJailScript(cfg Config, files volumeBuildFiles, runnerGID int) string {
	quote := shellquote.Quote
	var script strings.Builder
	script.WriteString(volumeBuildCleanupScript(files))
	fmt.Fprintf(&script, "chown root:root %s %s\n", quote(files.drives[volumebuild.DriveRootfs]), quote(files.drives[volumebuild.DriveTar]))
	fmt.Fprintf(&script, "chmod 0444 %s %s\n", quote(files.drives[volumebuild.DriveRootfs]), quote(files.drives[volumebuild.DriveTar]))
	fmt.Fprintf(&script, "chown 1000:%d %s %s\n", runnerGID, quote(files.drives[volumebuild.DriveOutput]), quote(files.drives[volumebuild.DriveStatus]))
	fmt.Fprintf(&script, "chmod 0660 %s %s\n", quote(files.drives[volumebuild.DriveOutput]), quote(files.drives[volumebuild.DriveStatus]))
	fmt.Fprintf(&script, "mkdir -p %s\n", quote(files.jailRoot))

	kernelTarget := filepath.Join(files.jailRoot, volumeBuildKernelPath)
	fmt.Fprintf(&script, ": > %s\nmount --bind -o ro %s %s\n", quote(kernelTarget), quote(templateKernelPath(cfg)), quote(kernelTarget))
	for drive, spec := range volumeBuildDrives {
		target := filepath.Join(files.jailRoot, spec.jailPath)
		options := ""
		if spec.readOnly {
			options = "-o ro "
		}
		fmt.Fprintf(&script, ": > %s\nmount --bind %s%s %s\n", quote(target), options, quote(files.drives[drive]), quote(target))
	}
	fmt.Fprintf(&script, "ip netns add %s\n", quote(volumeBuildNetns))
	return script.String()
}

// volumeBuildCleanupScript removes the builder's jail and network namespace.
// It is safe to repeat, and unmounts every bind first: rm cannot delete a
// file something is mounted on.
func volumeBuildCleanupScript(files volumeBuildFiles) string {
	quote := shellquote.Quote
	var script strings.Builder
	script.WriteString("set -eu\n")
	fmt.Fprintf(&script, "umount -l %s 2>/dev/null || true\n", quote(filepath.Join(files.jailRoot, volumeBuildKernelPath)))
	for _, spec := range volumeBuildDrives {
		fmt.Fprintf(&script, "umount -l %s 2>/dev/null || true\n", quote(filepath.Join(files.jailRoot, spec.jailPath)))
	}
	fmt.Fprintf(&script, "rm -rf %s\n", quote(files.jailDir))
	fmt.Fprintf(&script, "ip netns delete %s 2>/dev/null || true\n", quote(volumeBuildNetns))
	return script.String()
}

// waitForVolumeBuilder waits for the builder microVM to end by itself, or
// for builderCtx, the build's time limit within ctx, to end.
func (r *Runtime) waitForVolumeBuilder(ctx, builderCtx context.Context, exited <-chan struct{}) error {
	select {
	case <-exited:
		return nil
	case <-builderCtx.Done():
		if r.volumeBuilderTimedOut(ctx, builderCtx) {
			return r.volumeBuildTimeout(nil)
		}
		return ctx.Err()
	}
}

// volumeBuilderTimedOut reports whether builderCtx ended at the build's time
// limit rather than with the caller's ctx.
func (r *Runtime) volumeBuilderTimedOut(ctx, builderCtx context.Context) bool {
	return ctx.Err() == nil && errors.Is(builderCtx.Err(), context.DeadlineExceeded)
}

// volumeBuildTimeout is the build's time limit ending, during the step that
// failed if there is one.
func (r *Runtime) volumeBuildTimeout(during error) error {
	err := fmt.Errorf("the builder did not finish within %s of its start", r.config.VolumeBuildTimeout)
	if during != nil {
		err = fmt.Errorf("%w: %w", err, during)
	}
	return &runnerruntime.VolumeBuildError{Reason: runnerruntime.VolumeBuildTimeout, Err: err}
}

// readVolumeBuildStatus reads the record the guest left: a bounded read and a
// strict parse of bytes the runner does not trust.
func readVolumeBuildStatus(path string) (volumebuild.Status, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return volumebuild.Status{}, err
	}
	defer file.Close()
	record := make([]byte, volumebuild.StatusBytes)
	if _, err := io.ReadFull(file, record); err != nil {
		return volumebuild.Status{}, err
	}
	return volumebuild.ParseStatus(record)
}

// deliverVolumeImage hands the image over as opaque bytes: exactly the size
// the drive was created with, whatever the file is now.
func deliverVolumeImage(path string, imageBytes int64, deliver func(image io.Reader, size int64) error) error {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != imageBytes {
		return &runnerruntime.VolumeBuildError{
			Reason: volumebuild.ReasonInternal,
			Err:    fmt.Errorf("the image file has %d bytes, created with %d", info.Size(), imageBytes),
		}
	}
	return deliver(io.LimitReader(file, imageBytes), imageBytes)
}

// cleanupVolumeBuild ends the builder microVM if it still runs and removes
// the jail, its binds and namespace, and the build's files. It runs on every
// way out of a build, with a budget of its own: the build's context may be
// the reason it ended.
func (r *Runtime) cleanupVolumeBuild(ctx context.Context, builder process, files volumeBuildFiles) {
	cleanupCtx, cancel := withCleanupBudget(ctx)
	defer cancel()
	if builder != nil {
		if err := builder.Kill(); err != nil && !containsProcessFinished(err) {
			slog.WarnContext(ctx, "firecracker volume build cleanup: kill the builder", "err", err)
		}
	}
	if err := r.deps.run(cleanupCtx, "sudo", "/bin/sh", "-c", volumeBuildCleanupScript(files)); err != nil {
		slog.WarnContext(ctx, "firecracker volume build cleanup: jail", "err", err)
	}
	if err := removeSandboxDataDir(cleanupCtx, files.dir); err != nil {
		slog.WarnContext(ctx, "firecracker volume build cleanup: files", "path", files.dir, "err", err)
	}
}

// reconcileVolumeBuild clears the jail and namespace of a build a runner
// crash interrupted. Its files are a directory in the data dir, which
// reconcileSandboxDataDirs removes.
func (r *Runtime) reconcileVolumeBuild(ctx context.Context) error {
	return r.deps.run(ctx, "sudo", "/bin/sh", "-c", volumeBuildCleanupScript(r.volumeBuildFiles()))
}
