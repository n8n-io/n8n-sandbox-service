package firecracker

import (
	"fmt"
	"log/slog"
	"os"
	"syscall"

	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// stoppedSnapshotHeadroomBytes is the free space a stop needs on top of the
// snapshot's size: writing a snapshot can briefly take a little more than the
// file itself.
const stoppedSnapshotHeadroomBytes = 64 << 20 // 64 MiB

// volumeSpace is the free and total space, in bytes, of the filesystem holding
// the sandbox data dir. free is what an unprivileged writer can still use.
type volumeSpace struct {
	free  int64
	total int64
}

// statVolume's error leaves out the path: it can reach the client whose create
// failed.
func statVolume(path string) (volumeSpace, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return volumeSpace{}, fmt.Errorf("read free space of the sandbox data dir: %w", err)
	}
	return volumeSpace{
		free:  int64(stat.Bavail) * int64(stat.Bsize),
		total: int64(stat.Blocks) * int64(stat.Bsize),
	}, nil
}

// SetDiskReservation tells creates and volume builds how much of the data
// volume is set aside and not written yet: the volume cache's unused budget.
// Call it before Prepare.
func (r *Runtime) SetDiskReservation(reserved func() int64) {
	r.diskReserved = reserved
}

// checkCreateDiskSpace refuses a create while the data volume, less what is
// set aside, is below its floor. The refusal wraps ErrCapacityExhausted, so the
// API tries another runner; nothing has been created yet.
func (r *Runtime) checkCreateDiskSpace() error {
	space, err := r.deps.volumeSpace(r.runnerConfig.DataDir)
	if err != nil {
		return err
	}
	var reserved int64
	if r.diskReserved != nil {
		reserved = r.diskReserved()
	}
	floor := space.total * runnerruntime.DiskFloorPercent / 100
	switch {
	case space.free-reserved >= floor:
		return nil
	case reserved > 0:
		// Named because on a small disk the cache's default budget alone can keep
		// the runner from admitting anything, the admission canary included.
		return fmt.Errorf("firecracker data volume has %d MiB free; less the %d MiB set aside for the volume cache (SANDBOX_RUNNER_VOLUME_CACHE_MAX_BYTES), that is below its %d MiB floor: %w",
			space.free>>20, reserved>>20, floor>>20, runnerruntime.ErrCapacityExhausted)
	default:
		return fmt.Errorf("firecracker data volume has %d MiB free, below its %d MiB floor: %w",
			space.free>>20, floor>>20, runnerruntime.ErrCapacityExhausted)
	}
}

// ensureDiskSpaceForSnapshot fails a stop with no room for its snapshot, before
// the guest is paused, so the sandbox keeps running and the idle sweeper retries.
// It never deletes another sandbox to make room: the runner does not know who
// owns them. Unlike a create, a stop may use the space the volume cache sets
// aside: the sandbox is already on the volume, and those may grow into that
// space as into the floor.
func (r *Runtime) ensureDiskSpaceForSnapshot(state *sandboxState) error {
	needed, err := snapshotWriteBytes(state.snapshotMemPath)
	if err != nil {
		return err
	}
	needed += stoppedSnapshotHeadroomBytes

	space, err := r.deps.volumeSpace(r.runnerConfig.DataDir)
	if err != nil {
		return err
	}
	if space.free < needed {
		slog.Warn("firecracker stop refused: not enough disk space for the snapshot",
			"sandbox_id", state.id, "free_bytes", space.free, "needed_bytes", needed)
		return fmt.Errorf("insufficient disk space for firecracker snapshot: %d bytes free, %d needed", space.free, needed)
	}
	return nil
}

func snapshotWriteBytes(memPath string) (int64, error) {
	info, err := os.Stat(memPath)
	if err != nil {
		return 0, fmt.Errorf("stat snapshot mem: %w", err)
	}
	return info.Size(), nil
}
