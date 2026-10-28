package firecracker

import (
	"fmt"
	"log/slog"
	"os"
	"syscall"

	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// stoppedSnapshotHeadroomBytes is extra free-space reserved on top of the
// current per-sandbox snapshot_mem size before StopSandbox writes a new full
// snapshot. snapshot/create rewrites mem/state in place and may briefly need
// more disk than the prior file size (metadata, partial writes, filesystem
// rounding). The headroom avoids failing stop on a full volume when free space
// only barely equals the existing snapshot size.
const stoppedSnapshotHeadroomBytes = 64 << 20 // 64 MiB

// createFloorDivisor sets the free space a create needs on the data volume: one
// twentieth, 5%. A create takes almost nothing up front, because the rootfs and
// snapshot are reflinked, and grows as its guest writes. The floor stops new
// sandboxes from arriving on a volume that is filling up, and leaves the rest
// as room for the sandboxes already there to grow into.
const createFloorDivisor = 20

// volumeSpace is the space on the filesystem holding the sandbox data dir, in
// bytes. free is what an unprivileged writer can still allocate, which is what
// the guests' writes through Firecracker and the runner's clones draw on.
type volumeSpace struct {
	free  int64
	total int64
}

// statVolume leaves the path out of its error, which can reach the client whose
// create failed; the path is the runner's SANDBOX_RUNNER_DATA_DIR.
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

// checkCreateDiskSpace refuses a create while the data volume is below its
// floor. The refusal wraps ErrCapacityExhausted, so the API places the sandbox
// on another runner; nothing has been created yet at this point.
func (r *Runtime) checkCreateDiskSpace() error {
	space, err := r.deps.volumeSpace(r.runnerConfig.DataDir)
	if err != nil {
		return err
	}
	floor := space.total / createFloorDivisor
	if space.free < floor {
		return fmt.Errorf("firecracker data volume has %d MiB free, below its %d MiB floor: %w",
			space.free>>20, floor>>20, runnerruntime.ErrCapacityExhausted)
	}
	return nil
}

// ensureDiskSpaceForSnapshot fails a stop that has no room to write its
// snapshot. It runs before the guest is paused, so a refused stop leaves the
// sandbox running and the idle sweeper tries again later. Concurrent stops and
// running guests can still take the space before the snapshot is written; that
// stop fails too, and StopSandbox resumes the guest.
//
// It never makes room by deleting another sandbox. The runner does not know who
// owns its sandboxes, so whatever it deleted could belong to another tenant, and
// the API would not be told.
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
