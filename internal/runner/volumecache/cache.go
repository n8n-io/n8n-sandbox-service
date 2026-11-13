// Package volumecache keeps the runner-local copies of the read-only volume
// objects sandboxes attach: ext4 images for Firecracker, tars for Docker. Each
// object is downloaded once, verified against the size and sha256 the API
// recorded, and kept while any sandbox on the runner holds it, running or
// stopped.
//
// Files are owned by the runner (root in production), mode 0444, in a 0700
// directory, so no other uid can write them or reach them by path.
//
// The budget is the cache's share of its disk. Sandbox creates leave the part
// not downloaded yet free (DataDirReservation), and a download starts only if
// it fits above the floor left for the sandboxes on the disk.
package volumecache

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/n8n-io/sandbox-service/internal/metrics"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
	"github.com/n8n-io/sandbox-service/internal/volume"
)

// ErrCacheFull is returned when pinned entries and downloads in progress leave
// no room for an object. It is a capacity refusal: another runner may have
// room, and the cache has changed nothing.
var ErrCacheFull = fmt.Errorf("volume cache full: %w", runnerruntime.ErrCapacityExhausted)

// errTooLarge is not a capacity refusal: the object fits no runner configured
// like this one.
var errTooLarge = errors.New("volume object is larger than the volume cache")

// Key identifies a cached object. Scope is volume.SharedScope or a tenant ID.
// The same digest in two scopes is two entries, so content is never shared
// across tenants by digest.
type Key struct {
	Scope  string
	SHA256 string
}

func (k Key) validate() error {
	if !volume.ValidScope(k.Scope) {
		return fmt.Errorf("invalid volume cache scope %q", k.Scope)
	}
	if !volume.ValidDigest(k.SHA256) {
		return fmt.Errorf("invalid volume object sha256 %q: want 64 lowercase hex characters", k.SHA256)
	}
	return nil
}

// Source is where to download an object and the size the API recorded for it.
// The URL's query carries a token, so it is never logged.
type Source struct {
	URL  string
	Size int64
}

// Config configures a Cache.
type Config struct {
	// Dir is emptied by New. It must be absolute and must not be /, DataDir,
	// or a directory that contains DataDir.
	Dir      string
	DataDir  string
	MaxBytes int64
	// FetchTimeout bounds one download, however many callers wait for it.
	FetchTimeout time.Duration
	Metrics      *metrics.RunnerRecorder
}

// Cache is safe for concurrent use.
type Cache struct {
	// config is what New was given, with Dir cleaned.
	config    Config
	transport http.RoundTripper
	diskSpace func(path string) (diskSpace, error)

	mu       sync.Mutex
	dirReady bool
	entries  map[Key]*entry
	fetches  map[Key]*fetch
	// The budget counts both: the bytes the entries take on disk, and the
	// declared size of the downloads in progress.
	allocatedBytes   int64
	downloadingBytes int64
	// lruClock is a logical clock that stamps an entry's lastUse; eviction
	// takes the lowest first.
	lruClock uint64
	// downloadSeq gives every download its own file name.
	downloadSeq uint64
}

// diskSpace is the free and total space, in bytes, of a filesystem. free is
// what an unprivileged writer can still use.
type diskSpace struct {
	free  int64
	total int64
}

func statDisk(path string) (diskSpace, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return diskSpace{}, fmt.Errorf("read free space of the volume cache dir: %w", err)
	}
	return diskSpace{free: int64(st.Bavail) * int64(st.Bsize), total: int64(st.Blocks) * int64(st.Bsize)}, nil
}

type entry struct {
	path string
	// allocatedBytes is what the file takes on disk, less than its size when
	// it has holes.
	allocatedBytes int64
	// holders is the set of holder names that pin the entry, as given to
	// Acquire.
	holders map[string]struct{}
	// lastUse is the lruClock value from when the entry was downloaded, last
	// acquired, or released by its last holder.
	lastUse uint64
}

// fetch is one download in progress. waitersByHolder counts the Acquires
// waiting per holder; the holders still waiting when it succeeds are pinned
// with the new entry, so it cannot be evicted before they see it.
type fetch struct {
	done            chan struct{}
	waitersByHolder map[string]int
	finished        bool
	path            string
	err             error
}

// New checks cfg and empties the cache dir, creating it if needed: it only
// ever holds what this process downloaded.
func New(cfg Config) (*Cache, error) {
	dir, err := checkDir(cfg.Dir, cfg.DataDir)
	if err != nil {
		return nil, err
	}
	if cfg.MaxBytes <= 0 {
		return nil, fmt.Errorf("volume cache max bytes must be positive, got %d", cfg.MaxBytes)
	}
	if cfg.FetchTimeout <= 0 {
		return nil, fmt.Errorf("volume fetch timeout must be positive, got %s", cfg.FetchTimeout)
	}
	cfg.Dir = dir
	cache := &Cache{
		config:    cfg,
		transport: newTransport(),
		diskSpace: statDisk,
		entries:   make(map[Key]*entry),
		fetches:   make(map[Key]*fetch),
	}
	if err := cache.prepareDirLocked(); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", err, dir)
		}
		slog.Warn("volume cache: parent of the cache dir does not exist; the first download after it does creates the dir", "dir", dir)
	}
	cfg.Metrics.SetVolumeCache(
		func() float64 { used, _, _ := cache.stats(); return float64(used) },
		func() float64 { _, entries, _ := cache.stats(); return float64(entries) },
		func() float64 { _, _, pinned := cache.stats(); return float64(pinned) },
	)
	return cache, nil
}

// checkDir refuses a cache dir whose wipe would take the sandbox data with it.
// Symlinks are resolved in the parent only: the wipe replaces a symlink at dir
// itself without following it.
func checkDir(dir, dataDir string) (string, error) {
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("volume cache dir %q must be an absolute path", dir)
	}
	dir = filepath.Clean(dir)
	resolvedDir := filepath.Join(resolve(filepath.Dir(dir)), filepath.Base(dir))
	if dir == "/" || resolvedDir == "/" {
		return "", errors.New("volume cache dir must not be /")
	}
	absData, err := filepath.Abs(dataDir)
	if err != nil {
		return "", fmt.Errorf("resolve data dir: %w", err)
	}
	if within(absData, dir) || within(resolve(absData), resolvedDir) {
		return "", fmt.Errorf("volume cache dir %q must not be the data dir %q or contain it", dir, dataDir)
	}
	return dir, nil
}

func resolve(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

// within reports whether path is parent or inside it.
func within(path, parent string) bool {
	rel, err := filepath.Rel(parent, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// prepareDirLocked empties the cache dir and makes it the runner's, mode 0700,
// or creates it. An existing dir is kept, so it can be a mount point; a symlink
// in its place is removed, not followed. Not MkdirAll: a missing parent can be
// a data volume that is not mounted yet, and creating it would put the cache on
// the disk underneath. Errors leave out the path, like download errors.
func (c *Cache) prepareDirLocked() error {
	info, err := os.Lstat(c.config.Dir)
	if err == nil && !info.IsDir() {
		if err := os.Remove(c.config.Dir); err != nil {
			return fmt.Errorf("remove volume cache dir: %w", withoutPath(err))
		}
		err = fs.ErrNotExist
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.Mkdir(c.config.Dir, 0o700); err != nil {
			return fmt.Errorf("create volume cache dir: %w", withoutPath(err))
		}
	case err != nil:
		return fmt.Errorf("stat volume cache dir: %w", withoutPath(err))
	default:
		if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
			if err := os.Lchown(c.config.Dir, os.Geteuid(), os.Getegid()); err != nil {
				return fmt.Errorf("own volume cache dir: %w", withoutPath(err))
			}
		}
		if err := os.Chmod(c.config.Dir, 0o700); err != nil {
			return fmt.Errorf("chmod volume cache dir: %w", withoutPath(err))
		}
		entries, err := os.ReadDir(c.config.Dir)
		if err != nil {
			return fmt.Errorf("read volume cache dir: %w", withoutPath(err))
		}
		for _, dirEntry := range entries {
			if err := os.RemoveAll(filepath.Join(c.config.Dir, dirEntry.Name())); err != nil {
				return fmt.Errorf("wipe volume cache dir: %w", withoutPath(err))
			}
		}
	}
	c.dirReady = true
	return nil
}

// Acquire returns the path of a verified copy of key's object and pins it for
// holder until Release or ReleaseAll; the file stays in place while pinned.
// Pins are per holder, so a retried Acquire pins once. Concurrent Acquires of
// one key share one download, which runs under the cache's fetch timeout: ctx
// only bounds this caller's wait.
func (c *Cache) Acquire(ctx context.Context, key Key, src Source, holder string) (string, error) {
	if err := key.validate(); err != nil {
		return "", err
	}
	if holder == "" {
		return "", errors.New("volume cache holder is empty")
	}
	sourceURL, err := parseSource(src)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	c.mu.Lock()
	if cached, ok := c.entries[key]; ok {
		cached.holders[holder] = struct{}{}
		cached.lastUse = c.tickLocked()
		c.mu.Unlock()
		c.config.Metrics.ObserveVolumeCacheFetch(metrics.VolumeFetchHit)
		return cached.path, nil
	}
	pending, started := c.fetches[key], false
	var evicted []string
	if pending == nil {
		pending, evicted, err = c.startLocked(ctx, key, src, sourceURL, holder)
		if err != nil {
			c.mu.Unlock()
			result := metrics.VolumeFetchIO
			if errors.Is(err, ErrCacheFull) || errors.Is(err, errTooLarge) {
				result = metrics.VolumeFetchFull
			}
			c.config.Metrics.ObserveVolumeCacheFetch(result)
			return "", err
		}
		started = true
	}
	pending.waitersByHolder[holder]++
	c.mu.Unlock()

	// Their entries are gone from the index, and every download gets a new
	// file name, so nothing can reach these paths any more.
	for _, path := range evicted {
		if err := os.Remove(path); err != nil {
			slog.Warn("volume cache: remove evicted object", "path", path, "err", err)
		}
	}

	path, err := c.wait(ctx, pending, holder)
	if err == nil && !started {
		c.config.Metrics.ObserveVolumeCacheFetch(metrics.VolumeFetchHit)
	}
	return path, err
}

// startLocked makes room for src and starts its download. On error it has
// evicted nothing and reserved nothing.
func (c *Cache) startLocked(ctx context.Context, key Key, src Source, sourceURL *url.URL, holder string) (*fetch, []string, error) {
	if src.Size > c.config.MaxBytes {
		return nil, nil, fmt.Errorf("%w: %d bytes, cache holds %d", errTooLarge, src.Size, c.config.MaxBytes)
	}
	if !c.dirReady {
		// Again: symlinks in a parent that did not exist at startup were not
		// resolved then.
		if _, err := checkDir(c.config.Dir, c.config.DataDir); err != nil {
			return nil, nil, err
		}
		if err := c.prepareDirLocked(); err != nil {
			return nil, nil, err
		}
	}
	evicted, err := c.makeRoomLocked(src.Size)
	if err != nil {
		return nil, nil, err
	}
	c.downloadingBytes += src.Size
	c.downloadSeq++
	dest := filepath.Join(c.config.Dir, fmt.Sprintf("%s.%s.%d", key.Scope, key.SHA256, c.downloadSeq))
	pending := &fetch{done: make(chan struct{}), waitersByHolder: make(map[string]int)}
	c.fetches[key] = pending
	slog.InfoContext(ctx, "volume cache download started",
		"scope", key.Scope, "sha256", key.SHA256, "size", src.Size, "source", redact(sourceURL), "holder", holder)
	go c.run(context.WithoutCancel(ctx), key, src, sourceURL, dest, pending)
	return pending, evicted, nil
}

// makeRoomLocked evicts unpinned entries, least recently used first, until need
// more bytes fit both the budget and the disk, which keeps its floor free for
// the sandboxes on it. It evicts nothing if they cannot fit.
func (c *Cache) makeRoomLocked(need int64) ([]string, error) {
	space, err := c.diskSpace(c.config.Dir)
	if err != nil {
		return nil, err
	}
	budgetFree := c.config.MaxBytes - c.allocatedBytes - c.downloadingBytes
	// Downloads in progress count as if they had written nothing yet.
	diskFree := space.free - c.downloadingBytes - space.total*runnerruntime.DiskFloorPercent/100
	fits := func(freed int64) bool { return need <= budgetFree+freed && need <= diskFree+freed }
	if fits(0) {
		return nil, nil
	}
	type candidate struct {
		key    Key
		cached *entry
	}
	var candidates []candidate
	for key, cached := range c.entries {
		if len(cached.holders) == 0 {
			candidates = append(candidates, candidate{key, cached})
		}
	}
	sort.Slice(candidates, func(left, right int) bool {
		return candidates[left].cached.lastUse < candidates[right].cached.lastUse
	})
	var freed int64
	evictCount := 0
	for ; evictCount < len(candidates) && !fits(freed); evictCount++ {
		freed += candidates[evictCount].cached.allocatedBytes
	}
	if need > budgetFree+freed {
		return nil, fmt.Errorf("%w: objects in use fill its %d MiB budget", ErrCacheFull, c.config.MaxBytes>>20)
	}
	if need > diskFree+freed {
		return nil, fmt.Errorf("%w: the disk has %d MiB free above its floor, %d MiB needed", ErrCacheFull, max(0, diskFree+freed)>>20, need>>20)
	}
	paths := make([]string, 0, evictCount)
	for _, cand := range candidates[:evictCount] {
		delete(c.entries, cand.key)
		c.allocatedBytes -= cand.cached.allocatedBytes
		paths = append(paths, cand.cached.path)
		c.config.Metrics.ObserveVolumeCacheEviction()
		slog.Info("volume cache evicted", "scope", cand.key.Scope, "sha256", cand.key.SHA256, "allocated_bytes", cand.cached.allocatedBytes)
	}
	return paths, nil
}

func (c *Cache) run(ctx context.Context, key Key, src Source, sourceURL *url.URL, dest string, pending *fetch) {
	ctx, cancel := context.WithTimeout(ctx, c.config.FetchTimeout)
	defer cancel()
	start := time.Now()
	alloc, err := c.download(ctx, sourceURL, src.Size, key.SHA256, dest)
	dur := time.Since(start)

	// Recorded before the waiters are released, so whoever sees the result
	// finds it counted and logged.
	if err != nil {
		result := resultOf(err)
		c.config.Metrics.ObserveVolumeCacheFetch(result)
		attrs := []any{"scope", key.Scope, "sha256", key.SHA256, "source", redact(sourceURL), "result", result, "duration_ms", dur.Milliseconds(), "err", err}
		if cause := causeOf(err); cause != nil {
			attrs = append(attrs, "cause", cause)
		}
		slog.WarnContext(ctx, "volume cache download failed", attrs...)
	} else {
		c.config.Metrics.ObserveVolumeCacheFetch(metrics.VolumeFetchDownloaded)
		c.config.Metrics.ObserveVolumeCacheDownload(dur)
		slog.InfoContext(ctx, "volume cache download finished",
			"scope", key.Scope, "sha256", key.SHA256, "size", src.Size, "allocated_bytes", alloc, "duration_ms", dur.Milliseconds())
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.fetches, key)
	c.downloadingBytes -= src.Size
	if err == nil {
		holders := make(map[string]struct{}, len(pending.waitersByHolder))
		for holder := range pending.waitersByHolder {
			holders[holder] = struct{}{}
		}
		c.entries[key] = &entry{path: dest, allocatedBytes: alloc, holders: holders, lastUse: c.tickLocked()}
		c.allocatedBytes += alloc
		pending.path = dest
	}
	pending.err = err
	pending.finished = true
	close(pending.done)
}

// wait returns pending's result, or ctx's error if ctx ends first, in which
// case holder is not pinned when the download completes.
func (c *Cache) wait(ctx context.Context, pending *fetch, holder string) (string, error) {
	select {
	case <-pending.done:
		return pending.path, pending.err
	case <-ctx.Done():
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if pending.finished {
		// The download won the race and has pinned holder already.
		return pending.path, pending.err
	}
	if pending.waitersByHolder[holder]--; pending.waitersByHolder[holder] == 0 {
		delete(pending.waitersByHolder, holder)
	}
	return "", ctx.Err()
}

// Release unpins key for holder. It does nothing if holder does not hold key.
func (c *Cache) Release(key Key, holder string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cached, ok := c.entries[key]; ok {
		c.unpinLocked(cached, holder)
	}
}

// ReleaseAll unpins every entry holder holds. Like Release, it does not reach
// an Acquire that is still waiting.
func (c *Cache) ReleaseAll(holder string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, cached := range c.entries {
		c.unpinLocked(cached, holder)
	}
}

func (c *Cache) unpinLocked(cached *entry, holder string) {
	if _, ok := cached.holders[holder]; !ok {
		return
	}
	delete(cached.holders, holder)
	if len(cached.holders) == 0 {
		cached.lastUse = c.tickLocked()
	}
}

func (c *Cache) tickLocked() uint64 {
	c.lruClock++
	return c.lruClock
}

func (c *Cache) stats() (bytes int64, entries, pinned int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, cached := range c.entries {
		if len(cached.holders) > 0 {
			pinned++
		}
	}
	return c.allocatedBytes + c.downloadingBytes, len(c.entries), pinned
}

// DataDirReservation is the part of the budget not on disk yet, which sandbox
// creates leave free, while the cache shares the data dir's filesystem; 0 when
// it does not. Downloads in progress count as not on disk at all.
func (c *Cache) DataDirReservation() int64 {
	if !sameFilesystem(c.config.Dir, c.config.DataDir) {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return max(0, c.config.MaxBytes-c.allocatedBytes)
}

// sameFilesystem reports whether a and b are on one filesystem, or a missing
// path's parent is. When either cannot be read it says yes, so the space stays
// set aside.
func sameFilesystem(pathA, pathB string) bool {
	devA, okA := deviceOf(pathA)
	devB, okB := deviceOf(pathB)
	return !okA || !okB || devA == devB
}

func deviceOf(path string) (uint64, bool) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		info, err = os.Stat(filepath.Dir(path))
	}
	if err != nil {
		return 0, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Dev), true
}
