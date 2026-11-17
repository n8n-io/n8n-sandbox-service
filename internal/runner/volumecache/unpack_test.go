package volumecache

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/metrics"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
	"github.com/n8n-io/sandbox-service/internal/safetar"
	"github.com/n8n-io/sandbox-service/internal/volume"
)

// tarEntry is one entry of a test tar: a directory if its name ends in /, a
// link if it has a target, a file otherwise.
type tarEntry struct {
	name    string
	content string
	mode    int64
	target  string
}

func buildTar(t *testing.T, gzipped bool, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	var writer *tar.Writer
	var compressor *gzip.Writer
	if gzipped {
		compressor = gzip.NewWriter(&buf)
		writer = tar.NewWriter(compressor)
	} else {
		writer = tar.NewWriter(&buf)
	}
	for _, tarred := range entries {
		hdr := &tar.Header{Name: tarred.name, Mode: tarred.mode}
		switch {
		case strings.HasSuffix(tarred.name, "/"):
			hdr.Typeflag = tar.TypeDir
		case tarred.target != "":
			hdr.Typeflag, hdr.Linkname = tar.TypeSymlink, tarred.target
		default:
			hdr.Typeflag, hdr.Size = tar.TypeReg, int64(len(tarred.content))
		}
		if hdr.Mode == 0 {
			hdr.Mode = 0o644
		}
		if err := writer.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(tarred.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if compressor != nil {
		if err := compressor.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

// countTar counts a tar as the API does.
func countTar(t *testing.T, data []byte) volume.TarCounts {
	t.Helper()
	stats, err := safetar.Validate(bytes.NewReader(data), volume.TarPolicy(math.MaxInt32, 0, 0))
	if err != nil {
		t.Fatalf("count tar: %v", err)
	}
	return volume.TarCounts{Entries: int64(stats.Entries), ContentBytes: stats.Bytes, DecompressedBytes: stats.DecompressedBytes}
}

func tarKey(data []byte) Key {
	key := keyFor(data)
	key.Kind = volume.KindTar
	return key
}

func (s *server) tarSource(t *testing.T) Source {
	src := s.source()
	src.Tar = countTar(t, s.data)
	return src
}

func (s *server) acquireTar(t *testing.T, cache *Cache, holder string) string {
	t.Helper()
	path, err := s.fetchTar(t, cache, holder)
	if err != nil {
		t.Fatalf("Acquire() of a tar for %s failed: %v", holder, err)
	}
	return path
}

func (s *server) fetchTar(t *testing.T, cache *Cache, holder string) (string, error) {
	return cache.Acquire(context.Background(), tarKey(s.data), s.tarSource(t), holder)
}

func wantUnpacks(t *testing.T, rec *metrics.RunnerRecorder, result string, want float64) {
	t.Helper()
	if got := rec.VolumeCacheUnpackCount(result); got != want {
		t.Errorf("unpacks{result=%q} = %v, want %v", result, got, want)
	}
}

// treeSize walks a tree the way settleTree does, as a check on what the
// cache counts.
func treeSize(t *testing.T, dir string) room {
	t.Helper()
	var tree room
	err := filepath.WalkDir(dir, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		tree.bytes += int64(info.Sys().(*syscall.Stat_t).Blocks) * 512
		tree.inodes++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func TestAcquireUnpacksATar(t *testing.T) {
	for name, gzipped := range map[string]bool{"plain": false, "gzip": true} {
		t.Run(name, func(t *testing.T) {
			cache, rec := newTestCache(t, 1<<30)
			srv := serve(t, buildTar(t, gzipped,
				tarEntry{name: "data/hello.txt", content: "hello", mode: 0o600},
				tarEntry{name: "bin/run.sh", content: "#!/bin/sh\necho ok\n", mode: 0o4750},
				tarEntry{name: "empty/", mode: 0o700},
				tarEntry{name: ".bin/run", target: "../bin/run.sh"},
				tarEntry{name: "abs", target: "/mnt/volume/data/hello.txt"},
			), false)

			path := srv.acquireTar(t, cache, "sandbox-a")
			if filepath.Dir(path) != cache.config.Dir {
				t.Errorf("tree %s is not in the cache dir", path)
			}
			got, err := os.ReadFile(filepath.Join(path, "data/hello.txt"))
			if err != nil || string(got) != "hello" {
				t.Fatalf("data/hello.txt = %q, %v", got, err)
			}
			for name, want := range map[string]fs.FileMode{
				".":              fs.ModeDir | 0o755,
				"data":           fs.ModeDir | 0o755,
				"empty":          fs.ModeDir | 0o755,
				"data/hello.txt": 0o644,
				"bin/run.sh":     0o755,
			} {
				info, err := os.Lstat(filepath.Join(path, name))
				if err != nil || info.Mode() != want {
					t.Errorf("mode of %s = %v (%v), want %v", name, info, err, want)
					continue
				}
				if uid := int(info.Sys().(*syscall.Stat_t).Uid); uid != os.Geteuid() {
					t.Errorf("%s is owned by uid %d, want the runner's %d", name, uid, os.Geteuid())
				}
			}
			for name, want := range map[string]string{".bin/run": "../bin/run.sh", "abs": "/mnt/volume/data/hello.txt"} {
				if target, err := os.Readlink(filepath.Join(path, name)); err != nil || target != want {
					t.Errorf("link %s = %q, %v; want %q", name, target, err, want)
				}
			}

			// Only the tree is left, and the budget counts what it takes.
			if names := dirNames(t, cache.config.Dir); len(names) != 1 || names[0] != filepath.Base(path) {
				t.Errorf("cache dir holds %v, want only the tree", names)
			}
			tree := treeSize(t, path)
			cache.mu.Lock()
			cached := *cache.entries[tarKey(srv.data)]
			cache.mu.Unlock()
			if cached.allocatedBytes != tree.bytes || cached.inodes != tree.inodes || cache.usedBytes() != tree.bytes {
				t.Errorf("entry counts %d bytes and %d inodes, budget %d; the tree takes %d and %d",
					cached.allocatedBytes, cached.inodes, cache.usedBytes(), tree.bytes, tree.inodes)
			}
			if limit := treeRoom(countTar(t, srv.data)); tree.bytes > limit.bytes || tree.inodes > limit.inodes {
				t.Errorf("tree takes %+v, more than the %+v reserved for it", tree, limit)
			}
			wantFetches(t, rec, metrics.VolumeFetchDownloaded, 1)
			wantUnpacks(t, rec, metrics.VolumeUnpackSuccess, 1)
		})
	}
}

// Two sandboxes share one tree, and Acquires that wait together download and
// unpack once.
func TestConcurrentAcquiresUnpackOnce(t *testing.T) {
	cache, rec := newTestCache(t, 1<<30)
	srv := serve(t, buildTar(t, true, tarEntry{name: "a", content: "shared"}), true)
	key := tarKey(srv.data)
	src := srv.tarSource(t)

	const acquirers = 6
	paths := make([]string, acquirers)
	errs := make([]error, acquirers)
	var wg sync.WaitGroup
	for worker := range acquirers {
		wg.Go(func() {
			paths[worker], errs[worker] = cache.Acquire(context.Background(), key, src, "sandbox-"+strconv.Itoa(worker))
		})
	}
	waitFor(t, "every Acquire to wait on the fetch", func() bool { return cache.waiting(key) == acquirers })
	srv.open()
	wg.Wait()
	for worker := range acquirers {
		if errs[worker] != nil || paths[worker] != paths[0] {
			t.Errorf("Acquire %d = %q, %v; want %q", worker, paths[worker], errs[worker], paths[0])
		}
	}
	if holders := cache.holders(key); len(holders) != acquirers {
		t.Errorf("holders = %v, want all %d", holders, acquirers)
	}
	if again := srv.acquireTar(t, cache, "sandbox-late"); again != paths[0] {
		t.Errorf("a later Acquire got %q, want the shared tree %q", again, paths[0])
	}
	wantUnpacks(t, rec, metrics.VolumeUnpackSuccess, 1)
	if hits := srv.hits.Load(); hits != 1 {
		t.Errorf("server hits = %d, want 1", hits)
	}
}

// The same bytes as an image and as a tar are two entries.
func TestAcquireKeepsKindsApart(t *testing.T) {
	cache, _ := newTestCache(t, 1<<30)
	srv := serve(t, buildTar(t, false, tarEntry{name: "a", content: "x"}), false)
	image := srv.acquire(t, cache, "sandbox-a")
	tree := srv.acquireTar(t, cache, "sandbox-a")
	if info, err := os.Lstat(image); err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o444 {
		t.Errorf("image = %v, %v; want a 0444 file", info, err)
	}
	if info, err := os.Lstat(tree); err != nil || !info.IsDir() {
		t.Errorf("tree = %v, %v; want a directory", info, err)
	}
}

// A tar that breaks a rule, or holds more than the API counted, fails as that
// object: no capacity refusal, nothing left behind, no tar name in the error.
func TestUnpackFailureLeavesNothing(t *testing.T) {
	const secretName = "secret-entry-name"
	fixed := func(counts volume.TarCounts) func(*testing.T, []byte) volume.TarCounts {
		return func(*testing.T, []byte) volume.TarCounts { return counts }
	}
	counted := func(change func(*volume.TarCounts)) func(*testing.T, []byte) volume.TarCounts {
		return func(t *testing.T, data []byte) volume.TarCounts {
			counts := countTar(t, data)
			change(&counts)
			return counts
		}
	}
	for name, tt := range map[string]struct {
		data   []byte
		counts func(*testing.T, []byte) volume.TarCounts
		want   error
	}{
		"hard link": {
			data: func() []byte {
				var buf bytes.Buffer
				writer := tar.NewWriter(&buf)
				_ = writer.WriteHeader(&tar.Header{Name: secretName, Typeflag: tar.TypeLink, Linkname: "elsewhere"})
				_ = writer.Close()
				return buf.Bytes()
			}(),
			counts: fixed(volume.TarCounts{Entries: 1, DecompressedBytes: 2048}),
			want:   safetar.ErrInvalid,
		},
		"path through a link": {
			data:   buildTar(t, false, tarEntry{name: "link", target: "/etc"}, tarEntry{name: "link/" + secretName, content: "x"}),
			counts: fixed(volume.TarCounts{Entries: 2, ContentBytes: 1, DecompressedBytes: 4096}),
			want:   safetar.ErrInvalid,
		},
		"more entries than counted": {
			data:   buildTar(t, false, tarEntry{name: "a", content: "x"}, tarEntry{name: secretName, content: "y"}),
			counts: counted(func(counts *volume.TarCounts) { counts.Entries-- }),
			want:   safetar.ErrTooLarge,
		},
		"more content than counted": {
			data:   buildTar(t, false, tarEntry{name: "dir/" + secretName, content: "content"}),
			counts: counted(func(counts *volume.TarCounts) { counts.ContentBytes-- }),
			want:   safetar.ErrTooLarge,
		},
		"more decompressed than counted": {
			data:   buildTar(t, true, tarEntry{name: secretName, content: strings.Repeat("z", 64<<10)}),
			counts: counted(func(counts *volume.TarCounts) { counts.DecompressedBytes = counts.ContentBytes }),
			want:   safetar.ErrTooLarge,
		},
	} {
		t.Run(name, func(t *testing.T) {
			cache, rec := newTestCache(t, 1<<30)
			srv := serve(t, tt.data, false)
			src := srv.source()
			src.Tar = tt.counts(t, tt.data)

			_, err := cache.Acquire(context.Background(), tarKey(tt.data), src, "sandbox-a")
			if !errors.Is(err, tt.want) || errors.Is(err, runnerruntime.ErrCapacityExhausted) {
				t.Fatalf("Acquire() error = %v, want %v and no capacity refusal", err, tt.want)
			}
			if strings.Contains(err.Error(), secretName) || strings.Contains(err.Error(), cache.config.Dir) {
				t.Errorf("error names a path: %v", err)
			}
			wantUnpacks(t, rec, metrics.VolumeUnpackInvalid, 1)
			if names := dirNames(t, cache.config.Dir); len(names) != 0 {
				t.Errorf("cache dir holds %v after a failed unpack, want nothing", names)
			}
			if used := cache.usedBytes(); used != 0 {
				t.Errorf("budget counts %d bytes after a failed unpack, want 0", used)
			}
			if _, entries, _ := cache.stats(); entries != 0 {
				t.Errorf("entries = %d after a failed unpack, want 0", entries)
			}
		})
	}
}

// An unpack that can't get a slot within the fetch timeout times out and
// leaves nothing; once a slot frees, the next fetch unpacks.
func TestUnpackWaitsForASlotWithinTheFetchTimeout(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(filepath.Join(root, "cache"), filepath.Join(root, "data"))
	cfg.FetchTimeout = 300 * time.Millisecond
	cache, rec := openCache(t, cfg)
	for range maxConcurrentUnpacks {
		cache.unpackSlots <- struct{}{}
	}
	srv := serve(t, buildTar(t, false, tarEntry{name: "a", content: "x"}), false)

	_, err := srv.fetchTar(t, cache, "sandbox-a")
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, safetar.ErrInvalid) {
		t.Fatalf("Acquire() error = %v, want a timeout", err)
	}
	wantUnpacks(t, rec, metrics.VolumeUnpackTimeout, 1)
	if names := dirNames(t, cache.config.Dir); len(names) != 0 {
		t.Errorf("cache dir holds %v, want nothing", names)
	}

	<-cache.unpackSlots
	srv.acquireTar(t, cache, "sandbox-a")
	if len(cache.unpackSlots) != maxConcurrentUnpacks-1 {
		t.Errorf("%d slots taken after the unpack, want the %d the test holds", len(cache.unpackSlots), maxConcurrentUnpacks-1)
	}
}

// The room a tar fetch sets aside covers the tar and its tree at once, and
// a fetch that can't have it is refused before it downloads.
func TestTarFetchHoldsRoomForTheTarAndItsTree(t *testing.T) {
	cache, rec := newTestCache(t, 1<<30)
	srv := serve(t, buildTar(t, false, tarEntry{name: "a", content: "x"}), true)
	src := srv.tarSource(t)
	need := roomFor(tarKey(srv.data), src)
	if tree := treeRoom(src.Tar); need.bytes != src.Size+tree.bytes || need.inodes != 1+tree.inodes {
		t.Fatalf("room for the fetch = %+v, want the tar and the tree %+v", need, tree)
	}

	cache.setMaxBytes(need.bytes - 1)
	if _, err := srv.fetchTar(t, cache, "sandbox-a"); err == nil || errors.Is(err, runnerruntime.ErrCapacityExhausted) {
		t.Fatalf("Acquire() of a tar whose unpack can't fit the budget = %v, want a refusal no other runner would lift", err)
	}
	cache.setMaxBytes(1 << 30)
	done := make(chan error, 1)
	go func() { _, err := srv.fetchTar(t, cache, "sandbox-a"); done <- err }()
	waitFor(t, "the download to start", func() bool { return srv.hits.Load() == 1 })
	if used := cache.usedBytes(); used != need.bytes {
		t.Errorf("budget counts %d bytes during the fetch, want %d", used, need.bytes)
	}
	srv.open()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	wantFetches(t, rec, metrics.VolumeFetchFull, 1)
}

// A fetch leaves the disk's inode floor alone: it evicts unused entries for
// inodes as for bytes, and is refused like a full runner when that is not
// enough. A filesystem that reports no inodes is not checked.
func TestFetchKeepsTheInodeFloor(t *testing.T) {
	cache, rec := newTestCache(t, 1<<30)
	older := serve(t, buildTar(t, false, tarEntry{name: "a/b/c", content: "x"}), false)
	olderPath := older.acquireTar(t, cache, "sandbox-a")
	fresh := serve(t, buildTar(t, false, tarEntry{name: "d", content: "y"}), false)
	need := roomFor(tarKey(fresh.data), fresh.tarSource(t))

	const totalInodes = 1000
	floor := int64(totalInodes * runnerruntime.DiskFloorPercent / 100)
	setInodes := func(free int64) {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		cache.diskSpace = func(string) (diskSpace, error) {
			return diskSpace{free: 1 << 40, total: 2 << 40, freeInodes: free, totalInodes: totalInodes}, nil
		}
	}
	setInodes(floor + need.inodes - 1)
	_, err := fresh.fetchTar(t, cache, "sandbox-b")
	if !errors.Is(err, runnerruntime.ErrCapacityExhausted) || !strings.Contains(err.Error(), "inodes") {
		t.Fatalf("Acquire() into the inode floor = %v, want a capacity refusal naming inodes", err)
	}
	wantFetches(t, rec, metrics.VolumeFetchFull, 1)

	cache.Release(tarKey(older.data), "sandbox-a")
	fresh.acquireTar(t, cache, "sandbox-b")
	wantGone(t, olderPath)
	wantEvictions(t, rec, 1)

	cache.setDisk(1<<40, 2<<40)
	serve(t, buildTar(t, false, tarEntry{name: "e", content: "z"}), false).acquireTar(t, cache, "sandbox-c")
}

// An evicted entry counts in the budget until its fetch has removed it, so
// another fetch can't take the same room while it is still on disk.
func TestEvictedBytesCountUntilRemoved(t *testing.T) {
	cache, _ := newTestCache(t, 1<<30)
	srv := serve(t, randomBytes(t, 64<<10), false)
	path := srv.acquire(t, cache, "sandbox-a")
	cache.Release(keyFor(srv.data), "sandbox-a")
	used := cache.usedBytes()
	cache.setMaxBytes(used)

	cache.mu.Lock()
	evicted, err := cache.makeRoomLocked(room{bytes: used, inodes: 1})
	if err != nil || len(evicted) != 1 || evicted[0].path != path {
		cache.mu.Unlock()
		t.Fatalf("makeRoomLocked() = %+v, %v; want the entry evicted", evicted, err)
	}
	_, again := cache.makeRoomLocked(room{bytes: 1, inodes: 1})
	cache.mu.Unlock()
	if !errors.Is(again, ErrCacheFull) {
		t.Errorf("room taken by an entry still being removed was offered again: %v", again)
	}
	if got := cache.usedBytes(); got != used {
		t.Errorf("budget counts %d bytes, want the %d still on disk", got, used)
	}
}

// outsideTarget is a file and a directory outside the cache that links in a
// tree point at.
func outsideTarget(t *testing.T) (dir, file string) {
	t.Helper()
	dir = t.TempDir()
	file = filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(file, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, file
}

func linkingTar(t *testing.T, dir, file string) []byte {
	t.Helper()
	return buildTar(t, false,
		tarEntry{name: "to-dir", target: dir},
		tarEntry{name: "to-file", target: file},
		tarEntry{name: "nested/up", target: "../../.."},
		tarEntry{name: "nested/data", content: "x"},
	)
}

func wantKept(t *testing.T, dir, file string) {
	t.Helper()
	if got, err := os.ReadFile(file); err != nil || string(got) != "keep" {
		t.Errorf("link target %s = %q, %v; want it untouched", file, got, err)
	}
	if names := dirNames(t, dir); len(names) != 1 {
		t.Errorf("link target dir holds %v, want only keep.txt", names)
	}
}

// Eviction removes a tree's links, not what they point at.
func TestEvictionRemovesTreesWithoutFollowingLinks(t *testing.T) {
	dir, file := outsideTarget(t)
	cache, rec := newTestCache(t, 1<<30)
	linking := serve(t, linkingTar(t, dir, file), false)
	treePath := linking.acquireTar(t, cache, "sandbox-a")
	cache.Release(tarKey(linking.data), "sandbox-a")

	next := serve(t, buildTar(t, false, tarEntry{name: "a", content: "x"}), false)
	cache.setMaxBytes(cache.usedBytes() + roomFor(tarKey(next.data), next.tarSource(t)).bytes - 1)
	next.acquireTar(t, cache, "sandbox-b")

	wantGone(t, treePath)
	wantKept(t, dir, file)
	wantEvictions(t, rec, 1)
}

// The startup wipe removes trees an earlier process left, not what their
// links point at.
func TestStartupWipeRemovesTreesWithoutFollowingLinks(t *testing.T) {
	dir, file := outsideTarget(t)
	root := t.TempDir()
	cfg := testConfig(filepath.Join(root, "cache"), filepath.Join(root, "data"))
	cache, _ := openCache(t, cfg)
	serve(t, linkingTar(t, dir, file), false).acquireTar(t, cache, "sandbox-a")

	if _, err := New(cfg); err != nil {
		t.Fatalf("New() over a cache with a tree failed: %v", err)
	}
	if names := dirNames(t, cfg.Dir); len(names) != 0 {
		t.Errorf("cache dir holds %v after the wipe, want nothing", names)
	}
	wantKept(t, dir, file)
}

func settleIn(t *testing.T, dir string) (room, error) {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	return settleTree(context.Background(), root)
}

// A path as long as the tar rules allow unpacks, although it is longer than a
// system call takes once the cache dir is in front of it.
func TestUnpackTakesTheLongestPaths(t *testing.T) {
	// Long names rather than many: safetar's own cost grows with the depth.
	long := strings.Repeat(strings.Repeat("d", 250)+"/", 16) + strings.Repeat("f", 74)
	if len(long) != 4090 {
		t.Fatalf("test path is %d bytes, want 4090", len(long))
	}
	cache, rec := newTestCache(t, 1<<30)
	srv := serve(t, buildTar(t, false, tarEntry{name: long, content: "deep", mode: 0o600}), false)

	path := srv.acquireTar(t, cache, "sandbox-a")
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if got, err := root.ReadFile(long); err != nil || string(got) != "deep" {
		t.Fatalf("deep file = %q, %v", got, err)
	}
	if info, err := root.Lstat(long); err != nil || info.Mode() != 0o644 {
		t.Errorf("deep file mode = %v (%v), want 0644", info, err)
	}
	wantUnpacks(t, rec, metrics.VolumeUnpackSuccess, 1)
}

// settleTree stops when its context ends.
func TestSettleTreeStopsWithItsContext(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := settleTree(ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("settleTree() = %v, want %v", err, context.Canceled)
	}
}

// An object larger than the budget is refused before its room is summed,
// which could overflow.
func TestAcquireRefusesAHugeTarWithoutOverflow(t *testing.T) {
	cache, rec := newTestCache(t, 1<<30)
	srv := serve(t, buildTar(t, false, tarEntry{name: "a", content: "x"}), false)
	src := srv.tarSource(t)
	src.Size = math.MaxInt64
	_, err := cache.Acquire(context.Background(), tarKey(srv.data), src, "sandbox-a")
	if err == nil || errors.Is(err, runnerruntime.ErrCapacityExhausted) {
		t.Fatalf("Acquire() = %v, want a refusal no other runner would lift", err)
	}
	if used := cache.usedBytes(); used != 0 {
		t.Errorf("budget counts %d bytes after the refusal, want 0", used)
	}
	wantFetches(t, rec, metrics.VolumeFetchFull, 1)
}

// Whatever modes the tree was made with, settleTree leaves a sandbox read and
// execute, never write, and leaves links alone.
func TestSettleTreeSetsModes(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, mode := range map[string]fs.FileMode{"plain": 0o600, "exec": 0o700, "open": 0o666, "setuid": 0o755 | fs.ModeSetuid} {
		path := filepath.Join(dir, "sub", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(dir, "sub", "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	tree, err := settleIn(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]fs.FileMode{
		".": fs.ModeDir | 0o755, "sub": fs.ModeDir | 0o755,
		"sub/plain": 0o644, "sub/exec": 0o755, "sub/open": 0o644, "sub/setuid": 0o755,
	} {
		if info, err := os.Lstat(filepath.Join(dir, name)); err != nil || info.Mode() != want {
			t.Errorf("mode of %s = %v (%v), want %v", name, info, err, want)
		}
	}
	if info, err := os.Stat(outside); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("link target mode = %v (%v), want it left at 600", info, err)
	}
	if want := treeSize(t, dir); tree != want {
		t.Errorf("settleTree() = %+v, want %+v", tree, want)
	}
}

func TestSettleTreeRefusesOtherFileTypes(t *testing.T) {
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo-with-a-secret-name"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	_, err := settleIn(t, dir)
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("settleTree() = %v, want an error that names no path", err)
	}
}

// A disk that fills during an unpack is a full runner, and nothing the tar
// names reaches the error.
func TestUnpackFailureClassifies(t *testing.T) {
	entry := func(err error) error { return &safetar.EntryError{Name: "tar/secret", Err: err} }
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	for name, tt := range map[string]struct {
		ctx    context.Context
		err    error
		result string
		is     error
	}{
		"no space":  {context.Background(), entry(&fs.PathError{Op: "write", Path: "tar/secret", Err: syscall.ENOSPC}), metrics.VolumeUnpackFull, runnerruntime.ErrCapacityExhausted},
		"io":        {context.Background(), entry(&fs.PathError{Op: "open", Path: "tar/secret", Err: syscall.EIO}), metrics.VolumeUnpackIO, nil},
		"invalid":   {context.Background(), entry(safetar.ErrInvalid), metrics.VolumeUnpackInvalid, safetar.ErrInvalid},
		"too large": {context.Background(), entry(safetar.ErrTooLarge), metrics.VolumeUnpackInvalid, safetar.ErrTooLarge},
		// The reader's context error comes back wrapped as an invalid tar.
		"timeout": {ended, entry(errors.Join(safetar.ErrInvalid, context.Canceled)), metrics.VolumeUnpackTimeout, context.Canceled},
	} {
		t.Run(name, func(t *testing.T) {
			err := unpackFailure(tt.ctx, tt.err)
			if result := unpackResultOf(err); result != tt.result {
				t.Errorf("result = %s, want %s", result, tt.result)
			}
			if tt.is != nil && !errors.Is(err, tt.is) {
				t.Errorf("error %v is not %v", err, tt.is)
			}
			if tt.result == metrics.VolumeUnpackTimeout && errors.Is(err, safetar.ErrInvalid) {
				t.Errorf("a timeout counts as an invalid tar: %v", err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Errorf("error names the tar's path: %v", err)
			}
		})
	}
}
