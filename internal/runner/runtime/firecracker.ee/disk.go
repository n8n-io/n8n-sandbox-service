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

// createFloorDivisor sets the free space a create needs: 1/20 of the volume, 5%.
// New sandboxes take almost no space at first and grow as they write, so the
// floor is room for the sandboxes already there to grow into.
const createFloorDivisor = 20

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

// checkCreateDiskSpace refuses a create while the data volume is below its
// floor. The refusal wraps ErrCapacityExhausted, so the API tries another
// runner; nothing has been created yet.
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

// ensureDiskSpaceForSnapshot fails a stop with no room for its snapshot, before
// the guest is paused, so the sandbox keeps running and the idle sweeper retries.
// It never deletes another sandbox to make room: the runner does not know who
// owns them.
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
