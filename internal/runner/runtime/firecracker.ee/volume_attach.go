package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/n8n-io/sandbox-service/internal/daemon/volumedrives"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
	"github.com/n8n-io/sandbox-service/internal/shellquote"
	"github.com/n8n-io/sandbox-service/internal/volume"
)

var _ runnerruntime.VolumeAttacher = (*Runtime)(nil)

// SetVolumeCache hands the runtime the cache its volumes come from. Call it
// before Prepare. Without it, a create that names volumes fails.
func (r *Runtime) SetVolumeCache(cache runnerruntime.VolumeCache) {
	r.volumeCache = cache
}

// attachedVolume is a volume a sandbox has: its cached image, bound read-only
// at its drive's jail path on every activation and pinned for the sandbox's
// lifetime. Its drive is its position in the create options.
type attachedVolume struct {
	drive     int
	sha256    string
	image     string
	sectors   uint64
	mountPath string
	optional  bool
	// leftOut is why the guest did not mount the volume at create, "" if it
	// did. A cold boot mounts only what the create mounted.
	leftOut string
}

const volumeSectorBytes = 512

// leftOutVolume is an optional volume a create left out. err is the cache's
// error, for one left out before the guest started.
type leftOutVolume struct {
	drive  int
	sha256 string
	reason string
	err    error
}

// volumeAcquireReserve is the part of a create's budget that waiting for
// volume downloads leaves to the rest of the create. A var so tests can
// shrink it.
var volumeAcquireReserve = time.Minute

// acquireVolumes gets the image of every volume from the cache, all at once,
// pinned for the sandbox, before anything exists on the host. It returns the
// optional volumes it left out. A required volume that fails fails the
// create; a capacity refusal stays one, so the API tries another runner.
// Downloads that outlast the wait go on in the cache.
func (r *Runtime) acquireVolumes(ctx context.Context, state *sandboxState, volumes []runnerruntime.Volume) ([]leftOutVolume, error) {
	if r.volumeCache == nil {
		return nil, fmt.Errorf("this runner has no volume cache: %w", runnerruntime.ErrVolumesUnsupported)
	}
	if drives := len(state.bootParams.VolumeDrives); len(volumes) > drives {
		return nil, fmt.Errorf("the golden snapshot has %d volume drives, the create asks for %d volumes: %w", drives, len(volumes), runnerruntime.ErrVolumesUnsupported)
	}
	for index, vol := range volumes {
		if vol.Kind != volume.KindExt4 {
			return nil, fmt.Errorf("volume %d: Firecracker attaches %s images, not %s: %w", index, volume.KindExt4, vol.Kind, runnerruntime.ErrVolumesUnsupported)
		}
	}
	r.mu.Lock()
	state.pinsVolumes = true
	r.mu.Unlock()

	waitCtx, cancelWait := context.WithTimeout(ctx, acquireWait(ctx))
	defer cancelWait()
	paths := make([]string, len(volumes))
	errs := make([]error, len(volumes))
	var failedMu sync.Mutex
	var failed error
	fail := func(index int, vol runnerruntime.Volume, err error) {
		failedMu.Lock()
		if failed == nil {
			failed = fmt.Errorf("volume %d (sha256 %s): %w", index, vol.SHA256, err)
		}
		failedMu.Unlock()
		// The create fails: stop waiting for the others.
		cancelWait()
	}
	var wg sync.WaitGroup
	for index, vol := range volumes {
		wg.Go(func() {
			path, err := r.volumeCache.Acquire(waitCtx, vol, state.id)
			if err != nil {
				errs[index] = err
				if !vol.Optional {
					fail(index, vol, err)
				}
				return
			}
			paths[index] = path
			// The host's fault, not the volume's: fails even an optional one.
			if err := r.deps.checkVolumeImage(path); err != nil {
				fail(index, vol, err)
			}
		})
	}
	wg.Wait()

	var leftOut []leftOutVolume
	attached := make([]attachedVolume, 0, len(volumes))
	for index, vol := range volumes {
		if err := errs[index]; err != nil {
			leftOut = append(leftOut, leftOutVolume{drive: index, sha256: vol.SHA256, reason: acquireLeftOutReason(err), err: err})
			continue
		}
		attached = append(attached, attachedVolume{
			drive:     index,
			sha256:    vol.SHA256,
			image:     paths[index],
			sectors:   uint64(vol.Size) / volumeSectorBytes,
			mountPath: vol.MountPath,
			optional:  vol.Optional,
		})
	}

	// Shutdown can take the sandbox while acquires still wait, which then pin
	// after it released the sandbox's pins. Once it has untracked the sandbox,
	// nothing else would release them.
	r.mu.Lock()
	defer r.mu.Unlock()
	current, tracked := r.sandboxes[state.id]
	if !tracked {
		r.releaseVolumesLocked(state)
	}
	if current != state || state.deleting() {
		return nil, errors.New("sandbox was torn down while its volumes were acquired")
	}
	if failed != nil {
		return nil, failed
	}
	state.volumes = attached
	return leftOut, nil
}

// acquireWait is how long acquireVolumes may wait: ctx's time less
// volumeAcquireReserve, or half of it if that is more.
func acquireWait(ctx context.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(createBudget)
	}
	remaining := time.Until(deadline)
	return max(remaining-volumeAcquireReserve, remaining/2)
}

func acquireLeftOutReason(err error) string {
	switch {
	case errors.Is(err, runnerruntime.ErrCapacityExhausted):
		return runnerruntime.VolumeLeftOutCacheFull
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return runnerruntime.VolumeLeftOutTimeout
	default:
		return runnerruntime.VolumeLeftOutDownload
	}
}

// checkVolumeImage refuses a cached image that is not root-owned and readable
// but writable by no one. The cache writes it 0444 as the runner, which runs
// as root; every jail that attaches it binds the same inode. The error names
// no host path: it can reach the client whose create failed.
func checkVolumeImage(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New("stat cached volume image failed")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() {
		return errors.New("cached volume image is not a regular file")
	}
	if perm := info.Mode().Perm(); stat.Uid != 0 || perm&0o222 != 0 || perm&0o004 == 0 {
		return fmt.Errorf("cached volume image is owned by uid %d with mode %04o, want root and readable by all but writable by no one; the runner must run as root", stat.Uid, perm)
	}
	return nil
}

// volumeTarget is the jail path of a volume drive.
func volumeTarget(jailRoot string, drives []volumeDrive, drive int) string {
	return filepath.Join(jailRoot, strings.TrimPrefix(drives[drive].Path, "/"))
}

// volumeBinds bind-mounts each image read-only at its drive's jail path, over
// the placeholder volumeDrivePlaceholders has just renamed there, so the path
// is a regular file the runner made. It runs before jailer starts: jailer
// copies the jail's mounts into its namespace with their flags, and a
// read-only bind made after it would arrive there read-write. The image is
// never chowned: the bind shares the inode with the cache's file.
func volumeBinds(jailRoot string, drives []volumeDrive, volumes []attachedVolume) string {
	var script strings.Builder
	for _, vol := range volumes {
		fmt.Fprintf(&script, "mount --bind -o ro %s %s\n",
			shellquote.Quote(vol.image), shellquote.Quote(volumeTarget(jailRoot, drives, vol.drive)))
	}
	return script.String()
}

// volumeUnmounts undoes volumeBinds before the jail is removed. A bound path
// is a mount point, which Firecracker can neither rename nor remove, so a
// symlink there was never bound and is skipped, not followed.
func volumeUnmounts(jailRoot string, drives []volumeDrive, volumes []attachedVolume) string {
	var script strings.Builder
	for _, vol := range volumes {
		fmt.Fprintf(&script, "[ -L %[1]s ] || umount -l %[1]s 2>/dev/null || true\n",
			shellquote.Quote(volumeTarget(jailRoot, drives, vol.drive)))
	}
	return script.String()
}

type patchDriveRequest struct {
	DriveID    string `json:"drive_id"`
	PathOnHost string `json:"path_on_host"`
}

// patchVolumeDrive points a drive at its own path again, which makes
// Firecracker read the size of the image bound there and tell the guest. A
// restored guest keeps the drive size of its snapshot until then.
func patchVolumeDrive(ctx context.Context, socketPath string, drive volumeDrive) error {
	body, err := json.Marshal(patchDriveRequest{DriveID: drive.DriveID, PathOnHost: drive.Path})
	if err != nil {
		return err
	}
	return newFirecrackerAPIClient(socketPath).patchJSON(ctx, "/drives/"+drive.DriveID, body)
}

// volumesOf returns the sandbox's volumes as they are now.
func (r *Runtime) volumesOf(state *sandboxState) []attachedVolume {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]attachedVolume(nil), state.volumes...)
}

// patchVolumeDrives tells a guest restored from the golden snapshot the size
// of each image bound at its drives. A failure is the host's, not a volume's,
// so it fails the activation.
func (r *Runtime) patchVolumeDrives(ctx context.Context, state *sandboxState, volumes []attachedVolume) error {
	for _, vol := range volumes {
		drive := state.bootParams.VolumeDrives[vol.drive]
		if err := r.deps.patchVolumeDrive(ctx, state.socketPath, drive); err != nil {
			return fmt.Errorf("patch volume drive %s: %w", drive.DriveID, err)
		}
	}
	return nil
}

// mountGuestVolumes sends the guest daemon's one volume mount request: on
// create every volume, on a cold boot those the create mounted. A required
// volume the guest did not mount fails the activation. An optional one the
// create did not get is left out: its image stays bound and pinned, as the
// drive's size is in the guest and a wake restores it from the same path. The
// create reports it once it has succeeded (reportVolumes).
func (r *Runtime) mountGuestVolumes(ctx context.Context, state *sandboxState, creating bool, t *stepTimer) error {
	volumes := r.volumesOf(state)
	var drives []volumedrives.Volume
	var mounting []attachedVolume
	for _, vol := range volumes {
		if !creating && vol.leftOut != "" {
			continue
		}
		drives = append(drives, volumedrives.Volume{Drive: vol.drive, Sectors: vol.sectors, Path: vol.mountPath})
		mounting = append(mounting, vol)
	}
	var failed map[int]string
	mountErr := t.step(stepMountVolumes, func() error {
		var err error
		failed, err = r.deps.mountVolumeDrives(ctx, state.daemonURL, drives)
		return err
	})
	r.metrics.ObserveVolumeMount(volumeMountResult(drives, failed, mountErr))
	if mountErr != nil {
		return fmt.Errorf("mount guest volume drives: %w", mountErr)
	}
	for _, vol := range mounting {
		guestReason, ok := failed[vol.drive]
		if !ok {
			continue
		}
		reason := guestLeftOutReason(guestReason)
		if !vol.optional {
			return fmt.Errorf("guest did not mount required volume %d (sha256 %s): %s", vol.drive, vol.sha256, reason)
		}
		if !creating {
			slog.Warn("firecracker volume not mounted after cold boot",
				"sandbox_id", state.id, "drive", vol.drive, "sha256", vol.sha256, "reason", reason)
			continue
		}
		r.mu.Lock()
		for index := range state.volumes {
			if state.volumes[index].drive == vol.drive {
				state.volumes[index].leftOut = reason
			}
		}
		r.mu.Unlock()
	}
	return nil
}

// reportVolumes logs and counts what a create that succeeded attached and
// left out, and returns the echo's left-out reasons by drive.
func (r *Runtime) reportVolumes(sandboxID string, acquireLeftOut []leftOutVolume, volumes []attachedVolume) map[int]string {
	leftOut := slices.Clone(acquireLeftOut)
	for _, vol := range volumes {
		if vol.leftOut != "" {
			leftOut = append(leftOut, leftOutVolume{drive: vol.drive, sha256: vol.sha256, reason: vol.leftOut})
			continue
		}
		slog.Info("firecracker volume attached", "sandbox_id", sandboxID, "drive", vol.drive, "sha256", vol.sha256)
	}
	reasons := make(map[int]string, len(leftOut))
	for _, vol := range leftOut {
		attrs := []any{"sandbox_id", sandboxID, "drive", vol.drive, "sha256", vol.sha256, "reason", vol.reason}
		if vol.err != nil {
			attrs = append(attrs, "err", vol.err)
		}
		slog.Warn("firecracker volume left out", attrs...)
		r.metrics.ObserveVolumeLeftOut(vol.reason)
		reasons[vol.drive] = vol.reason
	}
	return reasons
}

// guestLeftOutReason maps a reason the daemon gave, already checked against
// volumedrives.KnownReason, to the echo's.
func guestLeftOutReason(reason string) string {
	switch reason {
	case volumedrives.ReasonDrive:
		return runnerruntime.VolumeLeftOutDrive
	case volumedrives.ReasonMountPoint:
		return runnerruntime.VolumeLeftOutMountPoint
	default:
		return runnerruntime.VolumeLeftOutMount
	}
}

// releaseVolumesLocked unpins the sandbox's images. Called under r.mu when the
// sandbox stops being tracked, so a new sandbox with the same ID can't have
// pinned anything yet.
func (r *Runtime) releaseVolumesLocked(state *sandboxState) {
	if state.pinsVolumes && r.volumeCache != nil {
		r.volumeCache.ReleaseAll(state.id)
	}
}
