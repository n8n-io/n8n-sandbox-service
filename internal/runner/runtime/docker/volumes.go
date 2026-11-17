package docker

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
	"github.com/n8n-io/sandbox-service/internal/safetar"
	"github.com/n8n-io/sandbox-service/internal/volume"
)

var _ runnerruntime.VolumeAttacher = (*Runtime)(nil)

// volumeMount is a cached tree bound read-only into a container.
type volumeMount struct {
	source string
	target string
}

// leftOutVolume is an optional volume a create left out, with the cache's
// error.
type leftOutVolume struct {
	index  int
	sha256 string
	reason string
	err    error
}

// volumeAcquireReserve is the part of a create's time that waiting for the
// volume cache leaves to the rest of the create. A var so tests can shrink it.
var volumeAcquireReserve = time.Minute

// SetVolumeCache hands the runtime the cache its volumes come from. Call it
// before Prepare. Without it, a create that names volumes fails.
func (m *Runtime) SetVolumeCache(cache runnerruntime.VolumeCache) {
	m.volumeCache = cache
	if err := m.prepareVolumeCacheDir(); err != nil {
		slog.Warn("docker volumes: the volume cache dir is not nosuid,nodev yet; creates with volumes try again", "err", err)
	}
}

// prepareVolumeCacheDir makes the mount the cache dir is on nosuid,nodev, which
// Docker's bind mounts of the trees in it copy: Docker can't set either flag
// on a bind mount itself.
func (m *Runtime) prepareVolumeCacheDir() error {
	m.volumeDirMu.Lock()
	defer m.volumeDirMu.Unlock()
	return m.setNoSuidNoDev(m.runnerConfig.VolumeCacheDir)
}

// createWithVolumes acquires every volume before the container exists, then
// creates it with one read-only bind mount per tree it got. The cache pins by
// sandbox ID, so it holds the ID's lock throughout, and refuses while a
// container of the ID exists, whose pins a failure here would release. Every
// pin is released if the create fails and its container is gone; delete
// releases them otherwise.
func (m *Runtime) createWithVolumes(ctx context.Context, sandboxID string, opts *CreateOptions) (*ContainerInfo, error) {
	if err := m.checkCreate(sandboxID); err != nil {
		return nil, err
	}
	if m.volumeCache == nil {
		return nil, fmt.Errorf("this runner has no volume cache: %w", runnerruntime.ErrVolumesUnsupported)
	}
	for index, vol := range opts.Volumes {
		if vol.Kind != volume.KindTar {
			return nil, fmt.Errorf("volume %d: Docker attaches %s volumes, not %s: %w", index, volume.KindTar, vol.Kind, runnerruntime.ErrVolumesUnsupported)
		}
	}
	unlock, err := m.lockSandbox(ctx, sandboxID, false)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if _, err := m.FindContainerIDByLabel(ctx, sandboxID); !errors.Is(err, ErrSandboxNotFound) {
		if err == nil {
			err = fmt.Errorf("sandbox %s has a container already", sandboxID)
		}
		return nil, err
	}

	mounts, leftOut, err := m.acquireVolumes(ctx, sandboxID, opts.Volumes)
	if err != nil {
		m.volumeCache.ReleaseAll(sandboxID)
		return nil, err
	}
	info, leftBehind, err := m.createContainer(ctx, sandboxID, opts, mounts)
	if err != nil {
		if leftBehind {
			// Unpinned, its trees could be evicted under it. A delete of the
			// sandbox removes it and releases them.
			slog.Warn("docker volumes stay pinned for a container a failed create left behind", "sandbox_id", sandboxID)
		} else {
			m.volumeCache.ReleaseAll(sandboxID)
		}
		slog.Warn("docker create with volumes failed", "sandbox_id", sandboxID, "err", err)
		return nil, withoutCachePaths{err: err, dir: m.runnerConfig.VolumeCacheDir}
	}
	info.VolumesLeftOut = m.reportVolumes(sandboxID, opts.Volumes, leftOut)
	return info, nil
}

// sandboxLock is held by the create with volumes or the delete of one
// sandbox ID that runs; users counts those holding or waiting for it.
type sandboxLock struct {
	held  chan struct{}
	users int
}

// lockSandbox takes the lock of sandboxID: waiting for it until ctx ends, or
// refusing at once if it is held.
func (m *Runtime) lockSandbox(ctx context.Context, sandboxID string, wait bool) (func(), error) {
	m.mu.Lock()
	lock := m.sandboxLocks[sandboxID]
	if lock == nil {
		lock = &sandboxLock{held: make(chan struct{}, 1)}
		m.sandboxLocks[sandboxID] = lock
	}
	lock.users++
	m.mu.Unlock()
	leave := func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if lock.users--; lock.users == 0 {
			delete(m.sandboxLocks, sandboxID)
		}
	}
	if wait {
		select {
		case lock.held <- struct{}{}:
		case <-ctx.Done():
			leave()
			return nil, ctx.Err()
		}
	} else {
		select {
		case lock.held <- struct{}{}:
		default:
			leave()
			return nil, fmt.Errorf("sandbox %s is being created or deleted", sandboxID)
		}
	}
	return func() {
		<-lock.held
		leave()
	}, nil
}

// withoutCachePaths is an error from Docker, which quotes its command line and
// the paths it could not bind, with the volume cache dir left out: it can
// reach the client whose create failed. The runner logs it whole.
type withoutCachePaths struct {
	err error
	dir string
}

// Error replaces the dir as the cache names its entries: cleaned.
func (e withoutCachePaths) Error() string {
	if e.dir == "" {
		return e.err.Error()
	}
	return strings.ReplaceAll(e.err.Error(), filepath.Clean(e.dir), "<volume cache>")
}

func (e withoutCachePaths) Unwrap() error { return e.err }

// acquireVolumes gets the tree of every volume from the cache, all at once,
// pinned for the sandbox, and readies their bind mounts. It returns the
// optional volumes it left out. A required volume that fails fails the
// create; a capacity refusal stays one, so the API tries another runner.
// Fetches that outlast the wait go on in the cache.
func (m *Runtime) acquireVolumes(ctx context.Context, sandboxID string, volumes []runnerruntime.Volume) ([]volumeMount, []leftOutVolume, error) {
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
			path, err := m.volumeCache.Acquire(waitCtx, vol, sandboxID)
			if err != nil {
				errs[index] = err
				if !vol.Optional {
					fail(index, vol, err)
				}
				return
			}
			paths[index] = path
		})
	}
	wg.Wait()
	if failed != nil {
		return nil, nil, failed
	}

	var mounts []volumeMount
	var leftOut []leftOutVolume
	for index, vol := range volumes {
		if err := errs[index]; err != nil {
			leftOut = append(leftOut, leftOutVolume{index: index, sha256: vol.SHA256, reason: leftOutReason(err), err: err})
			continue
		}
		mounts = append(mounts, volumeMount{source: paths[index], target: vol.MountPath})
	}
	if len(mounts) == 0 {
		return nil, leftOut, nil
	}
	// After the acquires: the cache creates its dir on the first download if
	// it was missing at startup.
	if err := m.prepareVolumeCacheDir(); err != nil {
		return nil, nil, fmt.Errorf("make the volume cache dir nosuid,nodev: %w: %w", err, runnerruntime.ErrVolumesUnsupported)
	}
	// The host's fault, not a volume's: fails even an optional one.
	for _, mount := range mounts {
		if err := m.checkVolumeTree(mount.source); err != nil {
			return nil, nil, fmt.Errorf("volume at %s: %w", mount.target, err)
		}
	}
	return mounts, leftOut, nil
}

// acquireWait is how long acquireVolumes may wait: ctx's time less
// volumeAcquireReserve, or half of it if that is more.
func acquireWait(ctx context.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(runnerruntime.CreateBudget)
	}
	remaining := time.Until(deadline)
	return max(remaining-volumeAcquireReserve, remaining/2)
}

func leftOutReason(err error) string {
	switch {
	case errors.Is(err, runnerruntime.ErrCapacityExhausted):
		return runnerruntime.VolumeLeftOutCacheFull
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return runnerruntime.VolumeLeftOutTimeout
	case errors.Is(err, safetar.ErrInvalid), errors.Is(err, safetar.ErrTooLarge):
		return runnerruntime.VolumeLeftOutUnpack
	default:
		return runnerruntime.VolumeLeftOutDownload
	}
}

// reportVolumes logs and counts what a create that succeeded attached and left
// out, and returns the echo's left-out reasons by index.
func (m *Runtime) reportVolumes(sandboxID string, volumes []runnerruntime.Volume, leftOut []leftOutVolume) map[int]string {
	reasons := make(map[int]string, len(leftOut))
	for _, vol := range leftOut {
		slog.Warn("docker volume left out",
			"sandbox_id", sandboxID, "index", vol.index, "sha256", vol.sha256, "reason", vol.reason, "err", vol.err)
		m.metrics.ObserveVolumeLeftOut(vol.reason)
		reasons[vol.index] = vol.reason
	}
	for index, vol := range volumes {
		if _, isLeftOut := reasons[index]; !isLeftOut {
			slog.Info("docker volume attached", "sandbox_id", sandboxID, "index", index, "sha256", vol.SHA256)
		}
	}
	return reasons
}

// releaseVolumes unpins the sandbox's trees once a delete, holding the ID's
// lock, found its container gone.
func (m *Runtime) releaseVolumes(sandboxID string) {
	if m.volumeCache != nil {
		m.volumeCache.ReleaseAll(sandboxID)
	}
}

// checkVolumeTree refuses a cached tree a sandbox could write, or whose bind
// mount would not be nosuid,nodev: its root must be a directory owned by the
// runner, mode 0755, on a mount with both flags. The cache unpacks it so. The
// error names no host path: it can reach the client whose create failed.
func checkVolumeTree(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New("stat cached volume tree failed")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() {
		return errors.New("cached volume tree is not a directory")
	}
	if int(stat.Uid) != os.Geteuid() || info.Mode() != fs.ModeDir|0o755 {
		return fmt.Errorf("cached volume tree is owned by uid %d with mode %v, want the runner's uid %d and 0755", stat.Uid, info.Mode().Perm(), os.Geteuid())
	}
	return checkNoSuidNoDev(path)
}

// dockerVolumeArgs binds each tree read-only at its mount path, and labels a
// container that has any, so a runner restart can tell it apart. Each --mount
// value is a CSV record, as Docker parses it: a mount path may hold a comma or
// a quote, which would otherwise end its field and could start another, such
// as source=.
func dockerVolumeArgs(mounts []volumeMount) []string {
	if len(mounts) == 0 {
		return nil
	}
	args := []string{"--label", containerLabelVolumes + "=" + containerLabelVolumesVal}
	for _, mount := range mounts {
		args = append(args, "--mount", csvRecord("type=bind", "source="+mount.source, "target="+mount.target, "readonly"))
	}
	return args
}

func csvRecord(fields ...string) string {
	var record strings.Builder
	writer := csv.NewWriter(&record)
	// Writing to a strings.Builder can't fail.
	_ = writer.Write(fields)
	writer.Flush()
	return strings.TrimSuffix(record.String(), "\n")
}

// removeVolumeContainersLeft tries once more to remove the containers with
// volumes that startup reconcile could not, and fails if any is left: the
// volume cache's startup wipe deletes the trees bound into it, and the runner
// would serve it without them.
func (m *Runtime) removeVolumeContainersLeft(ctx context.Context) error {
	ids, err := m.docker.findContainerByLabels(ctx, managedLabelFilter, volumesLabelFilter)
	if err != nil {
		return err
	}
	for _, id := range ids {
		_ = m.removeContainerAndTeardownRules(ctx, id)
	}
	ids, err = m.docker.findContainerByLabels(ctx, managedLabelFilter, volumesLabelFilter)
	if err != nil {
		return err
	}
	if len(ids) > 0 {
		return fmt.Errorf("%d sandbox containers with volumes could not be removed; the runner would serve them without their volumes", len(ids))
	}
	return nil
}
