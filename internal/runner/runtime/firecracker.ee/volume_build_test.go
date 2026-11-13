package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/n8n-io/sandbox-service/internal/daemon/volumebuild"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

var testBuildParams = volumebuild.Params{TarBytes: 3000, Entries: 4, ContentBytes: 1500, DecompressedBytes: 6000}

func testTar() []byte {
	return bytes.Repeat([]byte("tar!"), int(testBuildParams.TarBytes)/4)
}

// builderHarness is a runtime whose builder microVM is the guest func: it
// runs when the runner starts the VM, and the VM exits when it returns.
type builderHarness struct {
	t  *testing.T
	rt *Runtime

	mu       sync.Mutex
	commands []recordedCommand
	// cleanupCtxErrs is the state of the context each cleanup script ran with.
	cleanupCtxErrs []error
	jailerArgs     []string
	// commandsBeforeJailer counts the commands run before jailer started.
	commandsBeforeJailer int
	builder              *fakeProcess
	boot                 volumeBuilderBoot
	failCommand          string

	guest func(files volumeBuildFiles)
}

func newBuilderHarness(t *testing.T) *builderHarness {
	t.Helper()
	harness := &builderHarness{t: t, rt: testRuntimeT(t, 2)}
	harness.rt.config.VolumeBuildTimeout = time.Minute
	stubCreateDeps(harness.rt)

	var exit func(error)
	harness.rt.deps.run = func(ctx context.Context, name string, args ...string) error {
		harness.mu.Lock()
		defer harness.mu.Unlock()
		harness.commands = append(harness.commands, recordedCommand{name: name, args: args})
		script := args[len(args)-1]
		if strings.Contains(script, "umount -l") && !strings.Contains(script, "mount --bind") {
			harness.cleanupCtxErrs = append(harness.cleanupCtxErrs, ctx.Err())
		}
		if harness.failCommand != "" && strings.Contains(script, harness.failCommand) {
			return errors.New("injected command failure")
		}
		return nil
	}
	harness.rt.deps.cloneRootfs = func(_ context.Context, _, dest string) error {
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dest, []byte("rootfs"), 0o644)
	}
	harness.rt.deps.start = func(_ context.Context, onExit func(error), _ string, args ...string) (process, error) {
		if !slices.Contains(args, volumeBuildID) {
			return &fakeProcess{}, nil
		}
		harness.mu.Lock()
		defer harness.mu.Unlock()
		harness.jailerArgs, harness.builder, exit = args, &fakeProcess{}, onExit
		harness.commandsBeforeJailer = len(harness.commands)
		return harness.builder, nil
	}
	harness.rt.deps.bootVolumeBuilder = func(_ context.Context, _ string, boot volumeBuilderBoot) error {
		harness.mu.Lock()
		harness.boot = boot
		guest, exited := harness.guest, exit
		harness.mu.Unlock()
		if guest != nil {
			go func() {
				guest(harness.rt.volumeBuildFiles())
				exited(nil)
			}()
		}
		return nil
	}
	return harness
}

// scripts returns the shell scripts the runner ran that contain marker.
func (harness *builderHarness) scripts(marker string) []string {
	harness.mu.Lock()
	defer harness.mu.Unlock()
	var found []string
	for _, command := range harness.commands {
		if script := command.args[len(command.args)-1]; strings.Contains(script, marker) {
			found = append(found, script)
		}
	}
	return found
}

// requireCleanedUp checks what every way out of a build must leave: no
// builder, no jail, binds or namespace, no files, and the next build free.
func (harness *builderHarness) requireCleanedUp(t *testing.T) {
	t.Helper()
	files := harness.rt.volumeBuildFiles()
	if _, err := os.Stat(files.dir); !os.IsNotExist(err) {
		t.Errorf("the build's files are still there: %v", err)
	}
	harness.mu.Lock()
	builder, ctxErrs := harness.builder, slices.Clone(harness.cleanupCtxErrs)
	harness.mu.Unlock()
	if builder != nil && !builder.killed {
		t.Error("the builder microVM was not killed")
	}
	if len(ctxErrs) == 0 {
		t.Fatal("no cleanup script ran")
	}
	if last := ctxErrs[len(ctxErrs)-1]; last != nil {
		t.Errorf("the cleanup ran with a finished context: %v", last)
	}
	cleanup := harness.scripts("umount -l")
	last := cleanup[len(cleanup)-1]
	for _, want := range []string{
		"umount -l '/srv/jailer/firecracker/volume-build/root/kernel'",
		"umount -l '/srv/jailer/firecracker/volume-build/root/rootfs.ext4'",
		"umount -l '/srv/jailer/firecracker/volume-build/root/tar'",
		"umount -l '/srv/jailer/firecracker/volume-build/root/image.ext4'",
		"umount -l '/srv/jailer/firecracker/volume-build/root/status'",
		"rm -rf '/srv/jailer/firecracker/volume-build'\n",
		"ip netns delete 'fc-volume-build'",
	} {
		if !strings.Contains(last, want) {
			t.Errorf("the cleanup script lacks %q:\n%s", want, last)
		}
	}
	harness.rt.volumeBuildMu.Lock()
	defer harness.rt.volumeBuildMu.Unlock()
	if harness.rt.volumeBuild != nil {
		t.Error("the runner still counts a build as running")
	}
}

func statusRecord(t *testing.T, status volumebuild.Status) []byte {
	t.Helper()
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	record := make([]byte, volumebuild.StatusBytes)
	copy(record, raw)
	return record
}

// writeInPlace writes into a drive file as a guest does: without changing
// its size.
func writeInPlace(t *testing.T, path string, data []byte) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Error(err)
		return
	}
	defer file.Close()
	if _, err := file.WriteAt(data, 0); err != nil {
		t.Error(err)
	}
}

func guestBuilds(t *testing.T, status volumebuild.Status, image string) func(volumeBuildFiles) {
	return func(files volumeBuildFiles) {
		writeInPlace(t, files.drives[volumebuild.DriveOutput], []byte(image))
		writeInPlace(t, files.drives[volumebuild.DriveStatus], statusRecord(t, status))
	}
}

var okStatus = volumebuild.Status{Version: 1, Result: volumebuild.ResultOK, Files: 2, Bytes: 1500, UsedBytes: 8192}

func collectImage(got *[]byte, gotSize *int64) func(io.Reader, int64) error {
	return func(image io.Reader, size int64) error {
		data, err := io.ReadAll(image)
		*got, *gotSize = data, size
		return err
	}
}

func discardImage(io.Reader, int64) error { return nil }

func TestBuildVolumeBootsTheBuilderAndDeliversTheImage(t *testing.T) {
	harness := newBuilderHarness(t)
	imageBytes := volumebuild.ImageBytes(testBuildParams)

	var tarDrive, statusDrive []byte
	var outputSize int64
	var slotsUsed int32
	harness.guest = func(files volumeBuildFiles) {
		tarDrive, _ = os.ReadFile(files.drives[volumebuild.DriveTar])
		statusDrive, _ = os.ReadFile(files.drives[volumebuild.DriveStatus])
		if info, err := os.Stat(files.drives[volumebuild.DriveOutput]); err == nil {
			outputSize = info.Size()
		}
		capacity, _ := harness.rt.Capacity(context.Background())
		slotsUsed = capacity.Used
		guestBuilds(t, okStatus, "the image")(files)
	}

	var image []byte
	var size int64
	if err := harness.rt.BuildVolume(context.Background(), testBuildParams, bytes.NewReader(testTar()), collectImage(&image, &size)); err != nil {
		t.Fatalf("BuildVolume() failed: %v", err)
	}

	if size != imageBytes || int64(len(image)) != imageBytes || !bytes.HasPrefix(image, []byte("the image")) {
		t.Errorf("delivered %d bytes as size %d, want the %d bytes of the output drive", len(image), size, imageBytes)
	}
	wantTarDrive := append(testTar(), make([]byte, volumebuild.DriveBytes(testBuildParams.TarBytes)-testBuildParams.TarBytes)...)
	if !bytes.Equal(tarDrive, wantTarDrive) {
		t.Errorf("the tar drive holds %d bytes, want the tar padded to %d", len(tarDrive), len(wantTarDrive))
	}
	if outputSize != imageBytes {
		t.Errorf("the output drive has %d bytes, want %d", outputSize, imageBytes)
	}
	if !bytes.Equal(statusDrive, make([]byte, volumebuild.StatusBytes)) {
		t.Errorf("the status drive does not start as %d zeros", volumebuild.StatusBytes)
	}
	if slotsUsed != 0 {
		t.Errorf("the build took %d sandbox slots", slotsUsed)
	}

	golden := testBootParams(harness.rt.config)
	if harness.boot.vcpuCount != golden.VCPUCount || harness.boot.memSizeMiB != golden.MemSizeMiB {
		t.Errorf("builder machine = %+v, want the golden snapshot's %d vCPU and %d MiB", harness.boot, golden.VCPUCount, golden.MemSizeMiB)
	}
	wantArgs := "SANDBOX_VOLUME_BUILD_TAR_BYTES=3000 SANDBOX_VOLUME_BUILD_ENTRIES=4 SANDBOX_VOLUME_BUILD_CONTENT_BYTES=1500 " +
		"SANDBOX_VOLUME_BUILD_DECOMPRESSED_BYTES=6000 " +
		"console=ttyS0 reboot=k panic=1 pci=off ipv6.disable=1 clocksource=kvm-clock init=/sandbox-daemon"
	if harness.boot.bootArgs != wantArgs {
		t.Errorf("boot args:\n got %s\nwant %s", harness.boot.bootArgs, wantArgs)
	}

	for _, want := range [][]string{
		{"--id", "volume-build"},
		{"--netns", "/run/netns/fc-volume-build"},
		{"--uid", "1000"},
		{"--gid", "1000"},
	} {
		index := slices.Index(harness.jailerArgs, want[0])
		if index < 0 || harness.jailerArgs[index+1] != want[1] {
			t.Errorf("jailer args %q lack %q", harness.jailerArgs, want)
		}
	}
	harness.requireCleanedUp(t)
}

func TestBuildVolumeKeepsFirecrackerFromWritingTheTarAndRootfs(t *testing.T) {
	harness := newBuilderHarness(t)
	harness.guest = guestBuilds(t, okStatus, "")
	if err := harness.rt.BuildVolume(context.Background(), testBuildParams, bytes.NewReader(testTar()), discardImage); err != nil {
		t.Fatalf("BuildVolume() failed: %v", err)
	}

	dir := filepath.Join(harness.rt.runnerConfig.DataDir, "volume-build")
	quoted := func(name string) string { return "'" + filepath.Join(dir, name) + "'" }
	const jail = "/srv/jailer/firecracker/volume-build/root"

	replays := harness.scripts("e2fsck")
	if len(replays) != 1 || !strings.Contains(replays[0], "e2fsck -E journal_only -y -- "+quoted("rootfs.ext4")) {
		t.Fatalf("journal replay of the rootfs clone: %q", replays)
	}
	scripts := harness.scripts("mount --bind")
	if len(scripts) != 1 {
		t.Fatalf("got %d jail scripts, want 1", len(scripts))
	}
	script := scripts[0]
	for _, want := range []string{
		"chown root:root " + quoted("rootfs.ext4") + " " + quoted("tar") + "\n",
		"chmod 0444 " + quoted("rootfs.ext4") + " " + quoted("tar") + "\n",
		"chmod 0660 " + quoted("image.ext4") + " " + quoted("status") + "\n",
		"mount --bind -o ro '/srv/firecracker/template/vmlinux' '" + jail + "/kernel'\n",
		"mount --bind -o ro " + quoted("rootfs.ext4") + " '" + jail + "/rootfs.ext4'\n",
		"mount --bind -o ro " + quoted("tar") + " '" + jail + "/tar'\n",
		"mount --bind " + quoted("image.ext4") + " '" + jail + "/image.ext4'\n",
		"mount --bind " + quoted("status") + " '" + jail + "/status'\n",
		"ip netns add 'fc-volume-build'\n",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the jail script lacks %q:\n%s", want, script)
		}
	}
	// A bind target shares its inode with the source: chowning one in the jail
	// would chown the template kernel every sandbox shares.
	for line := range strings.SplitSeq(script, "\n") {
		if (strings.HasPrefix(line, "chown ") || strings.HasPrefix(line, "chmod ")) && (strings.Contains(line, jail) || strings.Contains(line, "template")) {
			t.Errorf("the jail script changes a bind target or the template: %s", line)
		}
	}
	// Read-only only counts if it is in place before jailer copies the mounts.
	harness.mu.Lock()
	defer harness.mu.Unlock()
	if len(harness.jailerArgs) == 0 {
		t.Fatal("jailer was not started")
	}
	bound := slices.IndexFunc(harness.commands, func(command recordedCommand) bool {
		return strings.Contains(command.args[len(command.args)-1], "mount --bind")
	})
	if bound < 0 || bound >= harness.commandsBeforeJailer {
		t.Errorf("the jail script ran as command %d, and jailer started after %d commands", bound+1, harness.commandsBeforeJailer)
	}
	if last := harness.commands[len(harness.commands)-1].args; !strings.Contains(last[len(last)-1], "umount -l") {
		t.Errorf("the last command is not the cleanup: %q", last)
	}
}

func TestBootVolumeBuilderSendsTheBuildersBootRequests(t *testing.T) {
	type request struct{ method, path, body string }
	var mu sync.Mutex
	var got []request
	socket := listenUnixHTTP(t, http.HandlerFunc(func(writer http.ResponseWriter, sent *http.Request) {
		body, _ := io.ReadAll(sent.Body)
		mu.Lock()
		got = append(got, request{sent.Method, sent.URL.Path, string(body)})
		mu.Unlock()
		writer.WriteHeader(http.StatusNoContent)
	}))

	boot := volumeBuilderBoot{vcpuCount: 1, memSizeMiB: 512, bootArgs: "A=1 console=ttyS0"}
	if err := bootVolumeBuilder(context.Background(), socket, boot); err != nil {
		t.Fatalf("bootVolumeBuilder() failed: %v", err)
	}

	// The guest tells its drives apart by this order alone; no network
	// interface is attached.
	want := []request{
		{http.MethodPut, "/machine-config", `{"vcpu_count":1,"mem_size_mib":512,"smt":false}`},
		{http.MethodPut, "/boot-source", `{"kernel_image_path":"/kernel","boot_args":"A=1 console=ttyS0"}`},
		{http.MethodPut, "/drives/rootfs", `{"drive_id":"rootfs","path_on_host":"/rootfs.ext4","is_root_device":true,"is_read_only":true}`},
		{http.MethodPut, "/drives/tar", `{"drive_id":"tar","path_on_host":"/tar","is_root_device":false,"is_read_only":true}`},
		{http.MethodPut, "/drives/output", `{"drive_id":"output","path_on_host":"/image.ext4","is_root_device":false,"is_read_only":false}`},
		{http.MethodPut, "/drives/status", `{"drive_id":"status","path_on_host":"/status","is_root_device":false,"is_read_only":false}`},
		{http.MethodPut, "/actions", `{"action_type":"InstanceStart"}`},
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(got, want) {
		t.Errorf("requests:\n got %+v\nwant %+v", got, want)
	}
}

func TestVolumeBuilderBootArgs(t *testing.T) {
	params := volumebuild.Params{TarBytes: 1, Entries: 2, ContentBytes: 3, DecompressedBytes: 4}
	const build = "SANDBOX_VOLUME_BUILD_TAR_BYTES=1 SANDBOX_VOLUME_BUILD_ENTRIES=2 SANDBOX_VOLUME_BUILD_CONTENT_BYTES=3 SANDBOX_VOLUME_BUILD_DECOMPRESSED_BYTES=4"
	for golden, want := range map[string]string{
		"console=ttyS0 init=/sandbox-daemon ip=172.16.0.10::172.16.0.1:255.255.255.0::eth0:off": build + " console=ttyS0 init=/sandbox-daemon",
		"ip=dhcp   console=ttyS0  reboot=k":                                                     build + " console=ttyS0 reboot=k",
		"console=ttyS0 -- argument":                                                             build + " console=ttyS0 -- argument",
	} {
		if got := volumeBuilderBootArgs(golden, params); got != want {
			t.Errorf("volumeBuilderBootArgs(%q):\n got %s\nwant %s", golden, got, want)
		}
	}
}

// neverRead fails the test if the runner reads a tar it should have refused.
type neverRead struct{ t *testing.T }

func (r neverRead) Read([]byte) (int, error) {
	r.t.Error("the tar was read")
	return 0, io.EOF
}

func TestBuildVolumeRefusesASecondBuild(t *testing.T) {
	harness := newBuilderHarness(t)
	running, finish := make(chan struct{}), make(chan struct{})
	harness.guest = func(files volumeBuildFiles) {
		close(running)
		<-finish
		guestBuilds(t, okStatus, "")(files)
	}
	first := make(chan error, 1)
	go func() {
		first <- harness.rt.BuildVolume(context.Background(), testBuildParams, bytes.NewReader(testTar()), discardImage)
	}()
	<-running

	harness.mu.Lock()
	commandsBefore := len(harness.commands)
	harness.mu.Unlock()
	err := harness.rt.BuildVolume(context.Background(), testBuildParams, neverRead{t}, discardImage)
	if !errors.Is(err, runnerruntime.ErrVolumeBuildBusy) || !errors.Is(err, runnerruntime.ErrCapacityExhausted) {
		t.Fatalf("second BuildVolume() = %v, want a capacity refusal for a busy runner", err)
	}
	harness.mu.Lock()
	if len(harness.commands) != commandsBefore {
		t.Errorf("the refused build ran commands: %v", harness.commands[commandsBefore:])
	}
	harness.mu.Unlock()
	if _, err := os.Stat(harness.rt.volumeBuildFiles().drives[volumebuild.DriveTar]); err != nil {
		t.Errorf("the refused build disturbed the running build's files: %v", err)
	}

	close(finish)
	if err := <-first; err != nil {
		t.Fatalf("first BuildVolume() failed: %v", err)
	}
	harness.guest = guestBuilds(t, okStatus, "")
	if err := harness.rt.BuildVolume(context.Background(), testBuildParams, bytes.NewReader(testTar()), discardImage); err != nil {
		t.Fatalf("BuildVolume() after the first finished: %v", err)
	}
}

func TestBuildVolumeLeavesSandboxCreatesAlone(t *testing.T) {
	harness := newBuilderHarness(t)
	var createErr error
	harness.guest = func(files volumeBuildFiles) {
		_, createErr = harness.rt.CreateSandbox(context.Background(), "sandbox-id-123456", nil)
		guestBuilds(t, okStatus, "")(files)
	}
	if err := harness.rt.BuildVolume(context.Background(), testBuildParams, bytes.NewReader(testTar()), discardImage); err != nil {
		t.Fatalf("BuildVolume() failed: %v", err)
	}
	if createErr != nil {
		t.Fatalf("CreateSandbox() during a build failed: %v", createErr)
	}
	if capacity, _ := harness.rt.Capacity(context.Background()); capacity.Used != 1 {
		t.Errorf("slots used = %d, want the sandbox's 1", capacity.Used)
	}
}

func TestBuildVolumeRefusesBelowTheDiskFloor(t *testing.T) {
	harness := newBuilderHarness(t)
	needed := volumebuild.DriveBytes(testBuildParams.TarBytes) + volumebuild.ImageBytes(testBuildParams)
	const total = 100 << 30
	floor := int64(total * createFloorPercent / 100)

	harness.rt.deps.volumeSpace = func(string) (volumeSpace, error) { return volumeSpace{free: floor + needed - 1, total: total}, nil }
	err := harness.rt.BuildVolume(context.Background(), testBuildParams, neverRead{t}, discardImage)
	if !errors.Is(err, runnerruntime.ErrCapacityExhausted) || errors.Is(err, runnerruntime.ErrVolumeBuildBusy) {
		t.Fatalf("BuildVolume() = %v, want a capacity refusal that is not busy", err)
	}
	if len(harness.commands) != 0 {
		t.Errorf("the refused build ran commands: %v", harness.commands)
	}
	if _, err := os.Stat(harness.rt.volumeBuildFiles().dir); !os.IsNotExist(err) {
		t.Errorf("the refused build created files: %v", err)
	}

	// With exactly enough, and the refusal did not leave the runner busy.
	harness.rt.deps.volumeSpace = func(string) (volumeSpace, error) { return volumeSpace{free: floor + needed, total: total}, nil }
	harness.guest = guestBuilds(t, okStatus, "")
	if err := harness.rt.BuildVolume(context.Background(), testBuildParams, bytes.NewReader(testTar()), discardImage); err != nil {
		t.Fatalf("BuildVolume() with enough disk: %v", err)
	}
}

func TestBuildVolumeRefusesInvalidParams(t *testing.T) {
	harness := newBuilderHarness(t)
	err := harness.rt.BuildVolume(context.Background(), volumebuild.Params{TarBytes: 10, Entries: 0}, neverRead{t}, discardImage)
	if err == nil || errors.Is(err, runnerruntime.ErrCapacityExhausted) {
		t.Fatalf("BuildVolume() = %v, want an error that is not a capacity refusal", err)
	}
	if len(harness.commands) != 0 {
		t.Errorf("ran commands: %v", harness.commands)
	}
}

func TestBuildVolumeReportsWhatTheBuilderRecorded(t *testing.T) {
	// rawStatus leaves text on the status drive, padded with zeros to size.
	rawStatus := func(text string, size int) func(volumeBuildFiles) {
		return func(files volumeBuildFiles) {
			record := make([]byte, size)
			copy(record, text)
			_ = os.WriteFile(files.drives[volumebuild.DriveStatus], record, 0o600)
		}
	}
	type guest func(t *testing.T) func(volumeBuildFiles)
	tests := map[string]struct {
		guest guest
		want  string
	}{
		// Everything here is a builder that died or lies: a runner fault.
		"a builder that died before its record": {func(*testing.T) func(volumeBuildFiles) { return func(volumeBuildFiles) {} }, volumebuild.ReasonInternal},
		"a record with a reason of its own": {func(*testing.T) func(volumeBuildFiles) {
			return rawStatus(`{"version":1,"result":"failed","reason":"tar\",result=\"success"}`, volumebuild.StatusBytes)
		}, volumebuild.ReasonInternal},
		"a record that is not JSON": {func(*testing.T) func(volumeBuildFiles) {
			return rawStatus(strings.Repeat("A", volumebuild.StatusBytes), volumebuild.StatusBytes)
		}, volumebuild.ReasonInternal},
		"a truncated status drive": {func(*testing.T) func(volumeBuildFiles) {
			return rawStatus(`{"version":1,"result":"ok"}`, 100)
		}, volumebuild.ReasonInternal},
		"an ok record on a resized image": {func(t *testing.T) func(volumeBuildFiles) {
			return func(files volumeBuildFiles) {
				guestBuilds(t, okStatus, "")(files)
				_ = os.Truncate(files.drives[volumebuild.DriveOutput], 1<<30)
			}
		}, volumebuild.ReasonInternal},
		"an ok record on an image that is now a link": {func(t *testing.T) func(volumeBuildFiles) {
			return func(files volumeBuildFiles) {
				guestBuilds(t, okStatus, "")(files)
				_ = os.Remove(files.drives[volumebuild.DriveOutput])
				_ = os.Symlink("/etc/hosts", files.drives[volumebuild.DriveOutput])
			}
		}, ""},
	}
	for _, reason := range volumeBuildGuestReasons {
		failed := volumebuild.Status{Version: 1, Result: volumebuild.ResultFailed, Reason: reason}
		tests["the builder reports "+reason] = struct {
			guest guest
			want  string
		}{func(t *testing.T) func(volumeBuildFiles) { return guestBuilds(t, failed, "") }, reason}
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			harness := newBuilderHarness(t)
			harness.guest = tt.guest(t)
			delivered := false
			err := harness.rt.BuildVolume(context.Background(), testBuildParams, bytes.NewReader(testTar()), func(io.Reader, int64) error {
				delivered = true
				return nil
			})

			var failure *runnerruntime.VolumeBuildError
			switch {
			case err == nil:
				t.Fatal("BuildVolume() succeeded")
			case tt.want == "" && errors.As(err, &failure):
				t.Errorf("BuildVolume() = %v, want a plain runner error", err)
			case tt.want != "" && (!errors.As(err, &failure) || failure.Reason != tt.want):
				t.Errorf("BuildVolume() = %v, want reason %q", err, tt.want)
			}
			if delivered {
				t.Error("an image was delivered")
			}
			// Nothing the guest wrote may travel in the error.
			if strings.Contains(err.Error(), "success") || strings.Contains(err.Error(), "AAAA") {
				t.Errorf("the error quotes the status record: %v", err)
			}
			harness.requireCleanedUp(t)
		})
	}
}

func TestBuildVolumeEndsABuilderAtItsTimeLimit(t *testing.T) {
	harness := newBuilderHarness(t)
	harness.rt.config.VolumeBuildTimeout = 20 * time.Millisecond

	err := harness.rt.BuildVolume(context.Background(), testBuildParams, bytes.NewReader(testTar()), discardImage)

	var failure *runnerruntime.VolumeBuildError
	if !errors.As(err, &failure) || failure.Reason != runnerruntime.VolumeBuildTimeout {
		t.Fatalf("BuildVolume() = %v, want reason %q", err, runnerruntime.VolumeBuildTimeout)
	}
	harness.requireCleanedUp(t)
}

func TestBuildVolumeEndsABuilderWhenTheCallerGoesAway(t *testing.T) {
	harness := newBuilderHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	harness.guest = func(volumeBuildFiles) {
		cancel()
		// A builder that would outlive the caller, were it not killed.
		time.Sleep(50 * time.Millisecond)
	}

	err := harness.rt.BuildVolume(ctx, testBuildParams, bytes.NewReader(testTar()), discardImage)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("BuildVolume() = %v, want %v", err, context.Canceled)
	}
	harness.requireCleanedUp(t)
}

func TestBuildVolumeCleansUpOnEveryWayOut(t *testing.T) {
	tests := map[string]func(harness *builderHarness) (tar io.Reader, deliver func(io.Reader, int64) error){
		"the tar cannot be read": func(*builderHarness) (io.Reader, func(io.Reader, int64) error) {
			return io.MultiReader(bytes.NewReader(testTar()[:100]), iotest.ErrReader(errors.New("stream broke"))), discardImage
		},
		"the tar is short": func(*builderHarness) (io.Reader, func(io.Reader, int64) error) {
			return bytes.NewReader(testTar()[:2999]), discardImage
		},
		"the tar is long": func(*builderHarness) (io.Reader, func(io.Reader, int64) error) {
			return bytes.NewReader(append(testTar(), 'x')), discardImage
		},
		"the boot parameters are missing": func(harness *builderHarness) (io.Reader, func(io.Reader, int64) error) {
			harness.rt.deps.loadBootParams = func(string) (*bootParams, error) { return nil, errors.New("no boot.json") }
			return bytes.NewReader(testTar()), discardImage
		},
		"the rootfs cannot be cloned": func(harness *builderHarness) (io.Reader, func(io.Reader, int64) error) {
			harness.rt.deps.cloneRootfs = func(context.Context, string, string) error { return errors.New("no space") }
			return bytes.NewReader(testTar()), discardImage
		},
		"the journal replay fails": func(harness *builderHarness) (io.Reader, func(io.Reader, int64) error) {
			harness.failCommand = "e2fsck"
			return bytes.NewReader(testTar()), discardImage
		},
		"the jail cannot be prepared": func(harness *builderHarness) (io.Reader, func(io.Reader, int64) error) {
			harness.failCommand = "mount --bind"
			return bytes.NewReader(testTar()), discardImage
		},
		"jailer does not start": func(harness *builderHarness) (io.Reader, func(io.Reader, int64) error) {
			harness.rt.deps.start = func(context.Context, func(error), string, ...string) (process, error) {
				return nil, errors.New("no jailer")
			}
			return bytes.NewReader(testTar()), discardImage
		},
		"the API socket never appears": func(harness *builderHarness) (io.Reader, func(io.Reader, int64) error) {
			harness.rt.deps.pathExists = func(string) bool { return false }
			return bytes.NewReader(testTar()), discardImage
		},
		"the builder does not boot": func(harness *builderHarness) (io.Reader, func(io.Reader, int64) error) {
			harness.rt.deps.bootVolumeBuilder = func(context.Context, string, volumeBuilderBoot) error { return errors.New("bad drive") }
			return bytes.NewReader(testTar()), discardImage
		},
		"the image cannot be delivered": func(harness *builderHarness) (io.Reader, func(io.Reader, int64) error) {
			harness.guest = guestBuilds(harness.t, okStatus, "")
			return bytes.NewReader(testTar()), func(io.Reader, int64) error { return errors.New("caller hung up") }
		},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			harness := newBuilderHarness(t)
			tar, deliver := setup(harness)

			err := harness.rt.BuildVolume(context.Background(), testBuildParams, tar, deliver)

			if err == nil {
				t.Fatal("BuildVolume() succeeded")
			}
			if errors.Is(err, runnerruntime.ErrCapacityExhausted) {
				t.Errorf("BuildVolume() = %v: a refusal after work began would have the API retry elsewhere", err)
			}
			harness.requireCleanedUp(t)
		})
	}
}

func TestShutdownEndsARunningVolumeBuild(t *testing.T) {
	harness := newBuilderHarness(t)
	running := make(chan struct{})
	harness.guest = func(volumeBuildFiles) {
		close(running)
		time.Sleep(50 * time.Millisecond)
	}
	result := make(chan error, 1)
	go func() {
		result <- harness.rt.BuildVolume(context.Background(), testBuildParams, bytes.NewReader(testTar()), discardImage)
	}()
	<-running

	harness.rt.Shutdown(context.Background())

	// Shutdown waited for the build's cleanup.
	harness.requireCleanedUp(t)
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("BuildVolume() = %v, want %v", err, context.Canceled)
	}
}

// The build cleans up next to the sandboxes: its cleanup takes nothing from
// the time Shutdown has for them.
func TestShutdownDeletesSandboxesWhileAVolumeBuildCleansUp(t *testing.T) {
	harness := newBuilderHarness(t)
	if _, err := harness.rt.CreateSandbox(context.Background(), "sandbox-id-123456", nil); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	running, release := make(chan struct{}), make(chan struct{})
	released := sync.OnceFunc(func() { close(release) })
	defer released()
	harness.guest = func(volumeBuildFiles) {
		close(running)
		<-release
	}
	run := harness.rt.deps.run
	harness.rt.deps.run = func(ctx context.Context, name string, args ...string) error {
		script := args[len(args)-1]
		if strings.Contains(script, "rm -rf '/srv/jailer/firecracker/volume-build'") && !strings.Contains(script, "mount --bind") {
			<-release
		}
		return run(ctx, name, args...)
	}
	result := make(chan error, 1)
	go func() {
		result <- harness.rt.BuildVolume(context.Background(), testBuildParams, bytes.NewReader(testTar()), discardImage)
	}()
	<-running

	shutdown := make(chan struct{})
	go func() {
		harness.rt.Shutdown(context.Background())
		close(shutdown)
	}()

	deadline := time.After(10 * time.Second)
	for {
		if capacity, _ := harness.rt.Capacity(context.Background()); capacity.Used == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Shutdown kept the sandbox while the volume build cleaned up")
		case <-time.After(5 * time.Millisecond):
		}
	}
	select {
	case <-shutdown:
		t.Fatal("Shutdown did not wait for the build's cleanup")
	default:
	}

	released()
	<-shutdown
	harness.requireCleanedUp(t)
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("BuildVolume() = %v, want %v", err, context.Canceled)
	}
}

func TestReconcileOnStartupClearsAnInterruptedVolumeBuild(t *testing.T) {
	rt := testRuntimeT(t, 1)
	files := rt.volumeBuildFiles()
	if err := os.Mkdir(files.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, drive := range files.drives {
		if err := os.WriteFile(drive, []byte("left behind"), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	var scripts []string
	rt.deps.run = func(_ context.Context, _ string, args ...string) error {
		scripts = append(scripts, args[len(args)-1])
		return nil
	}
	rt.deps.pathExists = func(string) bool { return true }

	rt.reconcileOnStartup(context.Background())

	if _, err := os.Stat(files.dir); !os.IsNotExist(err) {
		t.Errorf("the interrupted build's files are still there: %v", err)
	}
	unmount := slices.IndexFunc(scripts, func(script string) bool {
		return strings.Contains(script, "umount -l '/srv/jailer/firecracker/volume-build/root/tar'") &&
			strings.Contains(script, "ip netns delete 'fc-volume-build'")
	})
	removeJails := slices.IndexFunc(scripts, func(script string) bool {
		return strings.Contains(script, "rm -rf '/srv/jailer/firecracker'\n")
	})
	if unmount < 0 || removeJails < 0 || unmount > removeJails {
		t.Errorf("want the build's binds unmounted before the jails are removed, got scripts:\n%s", strings.Join(scripts, "\n---\n"))
	}
}
