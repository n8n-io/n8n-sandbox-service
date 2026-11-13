package volumecache

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/metrics"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
	"github.com/n8n-io/sandbox-service/internal/volume"
)

const token = "sig=secret-token"

func testConfig(dir, dataDir string) Config {
	return Config{Dir: dir, DataDir: dataDir, MaxBytes: 1 << 30, FetchTimeout: 10 * time.Second}
}

// newCache builds a cache with its own metrics on a disk with room to spare,
// its transports as New made them.
func newCache(t *testing.T, cfg Config) (*Cache, *metrics.RunnerRecorder) {
	t.Helper()
	rec := metrics.NewRunnerRecorder(true)
	cfg.Metrics = rec
	cache, err := New(cfg)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	cache.diskSpace = roomyDisk
	return cache, rec
}

// openCache builds a cache that reaches the test storage servers.
func openCache(t *testing.T, cfg Config) (*Cache, *metrics.RunnerRecorder) {
	t.Helper()
	cache, rec := newCache(t, cfg)
	storageCA, _ := storagePKI(t)
	cache.trust(storageCA.pool())
	return cache, rec
}

// trust makes the plain transport accept the certificates of pool instead of
// the system's CAs, and keeps whatever else New set on it.
func (c *Cache) trust(pool *x509.CertPool) {
	if c.transport.TLSClientConfig == nil {
		c.transport.TLSClientConfig = &tls.Config{}
	}
	c.transport.TLSClientConfig.RootCAs = pool
}

func newTestCache(t *testing.T, maxBytes int64) (*Cache, *metrics.RunnerRecorder) {
	t.Helper()
	root := t.TempDir()
	cfg := testConfig(filepath.Join(root, "cache"), filepath.Join(root, "data"))
	cfg.MaxBytes = maxBytes
	return openCache(t, cfg)
}

// roomyDisk stands in for the disk under test, so tests do not depend on how
// full the machine's disk is.
func roomyDisk(string) (diskSpace, error) { return diskSpace{free: 1 << 40, total: 2 << 40}, nil }

func (c *Cache) setDisk(free, total int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.diskSpace = func(string) (diskSpace, error) { return diskSpace{free: free, total: total}, nil }
}

func randomBytes(t *testing.T, size int) []byte {
	t.Helper()
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	return data
}

func keyFor(data []byte) Key {
	sum := sha256.Sum256(data)
	return Key{Scope: "11111111-2222-4333-8444-555555555555", SHA256: hex.EncodeToString(sum[:])}
}

// server serves data with a Content-Length, counting requests. With a gate,
// each request waits for it to close.
type server struct {
	*httptest.Server
	data []byte
	hits atomic.Int32
	gate chan struct{}
}

func serve(t *testing.T, data []byte, gated bool) *server {
	t.Helper()
	srv := &server{data: data}
	if gated {
		srv.gate = make(chan struct{})
	}
	srv.Server = serveTLS(t, func(writer http.ResponseWriter, req *http.Request) {
		srv.hits.Add(1)
		if srv.gate != nil {
			select {
			case <-srv.gate:
			case <-req.Context().Done():
				return
			}
		}
		writer.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = writer.Write(data)
	})
	return srv
}

// serveTLS starts a server that stands in for object storage: https, with a
// certificate of the storage test CA, open to any client.
func serveTLS(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	_, cert := storagePKI(t)
	srv := httptest.NewUnstartedServer(handler)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func (s *server) source() Source {
	return Source{URL: s.URL + "/objects/a?" + token, Size: int64(len(s.data))}
}

func (s *server) open() { close(s.gate) }

func (s *server) fetch(cache *Cache, holder string) (string, error) {
	return cache.Acquire(context.Background(), keyFor(s.data), s.source(), holder)
}

func (s *server) acquire(t *testing.T, cache *Cache, holder string) string {
	t.Helper()
	path, err := s.fetch(cache, holder)
	if err != nil {
		t.Fatalf("Acquire() for %s failed: %v", holder, err)
	}
	return path
}

// fetchAsync starts a fetch and reports its error once it ends.
func (s *server) fetchAsync(cache *Cache, holder string) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := s.fetch(cache, holder)
		done <- err
	}()
	return done
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func wantFetches(t *testing.T, rec *metrics.RunnerRecorder, result string, want float64) {
	t.Helper()
	if got := rec.VolumeCacheFetchCount(result); got != want {
		t.Errorf("fetches{result=%q} = %v, want %v", result, got, want)
	}
}

func wantEvictions(t *testing.T, rec *metrics.RunnerRecorder, want float64) {
	t.Helper()
	if got := rec.VolumeCacheEvictionCount(); got != want {
		t.Errorf("evictions = %v, want %v", got, want)
	}
}

func wantGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s still on disk: %v", path, err)
	}
}

func (c *Cache) waiting(key Key) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	if pending := c.fetches[key]; pending != nil {
		for _, count := range pending.waitersByHolder {
			total += count
		}
	}
	return total
}

func (c *Cache) holders(key Key) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	cached := c.entries[key]
	if cached == nil {
		return nil
	}
	var holders []string
	for holder := range cached.holders {
		holders = append(holders, holder)
	}
	return holders
}

func (c *Cache) setMaxBytes(maxBytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.config.MaxBytes = maxBytes
}

func (c *Cache) usedBytes() int64 {
	used, _, _ := c.stats()
	return used
}

// dirNames lists the cache dir, so tests can check no download file is left.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, dirEntry := range entries {
		names = append(names, dirEntry.Name())
	}
	return names
}

func TestAcquireDownloadsVerifiesAndPins(t *testing.T) {
	cache, rec := newTestCache(t, 1<<30)
	srv := serve(t, randomBytes(t, 3*copyBuffer+123), false)

	path := srv.acquire(t, cache, "sandbox-a")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, srv.data) {
		t.Fatal("cached content differs from the served object")
	}
	if filepath.Dir(path) != cache.config.Dir {
		t.Errorf("path %s is not in the cache dir %s", path, cache.config.Dir)
	}
	for name, want := range map[string]fs.FileMode{path: 0o444, cache.config.Dir: 0o700} {
		if info, err := os.Stat(name); err != nil || info.Mode().Perm() != want {
			t.Errorf("mode of %s = %v (%v), want %o", name, info, err, want)
		}
	}

	if again := srv.acquire(t, cache, "sandbox-b"); again != path {
		t.Fatalf("second Acquire() = %q, want %q", again, path)
	}
	if hits := srv.hits.Load(); hits != 1 {
		t.Errorf("server hits = %d, want 1", hits)
	}
	if holders := cache.holders(keyFor(srv.data)); len(holders) != 2 {
		t.Errorf("holders = %v, want sandbox-a and sandbox-b", holders)
	}
	wantFetches(t, rec, metrics.VolumeFetchDownloaded, 1)
	wantFetches(t, rec, metrics.VolumeFetchHit, 1)
	if names := dirNames(t, cache.config.Dir); len(names) != 1 {
		t.Errorf("cache dir holds %v, want only the object", names)
	}
}

// The same digest in another scope is another object, downloaded on its own.
func TestAcquireKeepsScopesApart(t *testing.T) {
	cache, _ := newTestCache(t, 1<<30)
	srv := serve(t, randomBytes(t, 4096), false)
	shared := Key{Scope: volume.SharedScope, SHA256: keyFor(srv.data).SHA256}

	tenantPath := srv.acquire(t, cache, "sandbox-a")
	sharedPath, err := cache.Acquire(context.Background(), shared, srv.source(), "sandbox-a")
	if err != nil {
		t.Fatal(err)
	}
	if tenantPath == sharedPath || srv.hits.Load() != 2 {
		t.Fatalf("paths %s and %s after %d downloads; want two files and two downloads", tenantPath, sharedPath, srv.hits.Load())
	}
}

func TestAcquireRejectsDigestMismatch(t *testing.T) {
	cache, rec := newTestCache(t, 1<<30)
	srv := serve(t, randomBytes(t, 8192), false)

	_, err := cache.Acquire(context.Background(), keyFor([]byte("something else")), srv.source(), "sandbox-a")
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("Acquire() error = %v, want a digest mismatch", err)
	}
	wantFetches(t, rec, metrics.VolumeFetchDigest, 1)
	if names := dirNames(t, cache.config.Dir); len(names) != 0 {
		t.Errorf("cache dir holds %v after a failed download, want nothing", names)
	}
	if used := cache.usedBytes(); used != 0 {
		t.Errorf("budget counts %d bytes after a failed download, want 0", used)
	}

	// A failure is not remembered.
	srv.acquire(t, cache, "sandbox-a")
}

func TestAcquireRejectsWrongSize(t *testing.T) {
	data := randomBytes(t, 3*sparseBlock)
	chunked := func(writer http.ResponseWriter, _ *http.Request) {
		writer.(http.Flusher).Flush()
		_, _ = writer.Write(data)
	}
	withLength := func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = writer.Write(data)
	}
	cutShort := func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = writer.Write(data[:len(data)/2])
	}
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		size    int
		want    string
	}{
		{"content length above size", withLength, len(data) - 1, "content length"},
		{"content length below size", withLength, len(data) + 1, "content length"},
		{"body longer than size", chunked, len(data) - 10, "longer"},
		{"body shorter than size", chunked, len(data) + 10, "shorter"},
		{"body cut off", cutShort, len(data), "shorter"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache, rec := newTestCache(t, 1<<30)
			srv := serveTLS(t, tc.handler)

			_, err := cache.Acquire(context.Background(), keyFor(data), Source{URL: srv.URL, Size: int64(tc.size)}, "sandbox-a")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Acquire() error = %v, want %q", err, tc.want)
			}
			wantFetches(t, rec, metrics.VolumeFetchSize, 1)
			if names := dirNames(t, cache.config.Dir); len(names) != 0 {
				t.Errorf("cache dir holds %v, want nothing", names)
			}
		})
	}
}

// Errors, which can reach clients, carry neither the token nor the storage
// address; logs name the source without its token. Redirects are refused.
func TestFailuresKeepSourceSecrets(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	respond := func(handler http.HandlerFunc) string {
		return serveTLS(t, handler).URL
	}
	redirectTo := func(location string) string {
		return respond(func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Location", location)
			writer.WriteHeader(http.StatusFound)
		})
	}
	closed := httptest.NewTLSServer(http.NotFoundHandler())
	closed.Close()
	target := serve(t, []byte("x"), false)

	for _, tc := range []struct{ name, base, want string }{
		{"forbidden", respond(func(writer http.ResponseWriter, _ *http.Request) {
			http.Error(writer, "forbidden", http.StatusForbidden)
		}), "status 403"},
		{"connection refused", closed.URL, "request failed"},
		{"redirect", redirectTo(target.URL + "/elsewhere?" + token), "status 302"},
		// An http.Client would quote this whole header in its parse error.
		{"unparsable redirect", redirectTo("/elsewhere/%zz?" + token), "status 302"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache, rec := newTestCache(t, 1<<30)
			logs.Reset()
			_, err := cache.Acquire(context.Background(), keyFor(target.data), Source{URL: tc.base + "/a?" + token, Size: 1}, "sandbox-a")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Acquire() error = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), strings.TrimPrefix(tc.base, "https://")) {
				t.Errorf("error names the source: %v", err)
			}
			if out := logs.String(); !strings.Contains(out, tc.base+"/a") || strings.Contains(out, "secret-token") {
				t.Errorf("logs do not name the source without its token:\n%s", out)
			}
			wantFetches(t, rec, metrics.VolumeFetchHTTP, 1)
		})
	}
	if hits := target.hits.Load(); hits != 0 {
		t.Errorf("redirect target served %d requests, want 0", hits)
	}
}

func TestAcquireRefusesSourceAndKey(t *testing.T) {
	cache, rec := newTestCache(t, 1<<30)
	srv := serve(t, randomBytes(t, 4096), false)
	good := keyFor(srv.data)
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		key  Key
		src  Source
	}{
		{"file scheme", good, Source{URL: "file:///etc/passwd", Size: 4096}},
		{"http scheme", good, Source{URL: "http://" + srv.Listener.Addr().String() + "/a", Size: 4096}},
		{"ftp scheme", good, Source{URL: "ftp://" + srv.Listener.Addr().String() + "/a", Size: 4096}},
		{"no scheme", good, Source{URL: "//" + srv.Listener.Addr().String() + "/a", Size: 4096}},
		{"no host", good, Source{URL: "https:///a", Size: 4096}},
		{"port without a host", good, Source{URL: "https://:" + port + "/a", Size: 4096}},
		{"unparsable", good, Source{URL: "https://h/%zz?" + token, Size: 4096}},
		{"zero size", good, Source{URL: srv.URL, Size: 0}},
		{"scope traversal", Key{Scope: "../../etc", SHA256: good.SHA256}, srv.source()},
		{"scope that is no tenant ID", Key{Scope: "__admin__", SHA256: good.SHA256}, srv.source()},
		{"uppercase digest", Key{Scope: volume.SharedScope, SHA256: strings.ToUpper(good.SHA256)}, srv.source()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := cache.Acquire(context.Background(), tc.key, tc.src, "sandbox-a")
			if err == nil {
				t.Fatal("Acquire() succeeded, want it refused")
			}
			if strings.Contains(err.Error(), "secret-token") {
				t.Errorf("error leaks the token: %v", err)
			}
		})
	}
	if _, err := srv.fetch(cache, ""); err == nil {
		t.Error("Acquire() with no holder succeeded")
	}
	if hits := srv.hits.Load(); hits != 0 {
		t.Errorf("server hits = %d, want 0", hits)
	}
	if names := dirNames(t, cache.config.Dir); len(names) != 0 {
		t.Errorf("cache dir holds %v, want nothing", names)
	}
	wantFetches(t, rec, metrics.VolumeFetchHTTP, 0)
}

// A disk that fills during a download is a full runner; other disk errors are
// not. Neither names the path.
func TestDiskFailure(t *testing.T) {
	full := diskFailure("write file", &fs.PathError{Op: "write", Path: "/cache/.download-1", Err: syscall.ENOSPC})
	if !errors.Is(full, runnerruntime.ErrCapacityExhausted) || resultOf(full) != metrics.VolumeFetchFull {
		t.Errorf("ENOSPC = %v (%s), want a capacity refusal counted as full", full, resultOf(full))
	}
	other := diskFailure("write file", &fs.PathError{Op: "write", Path: "/cache/.download-1", Err: syscall.EIO})
	if errors.Is(other, runnerruntime.ErrCapacityExhausted) || resultOf(other) != metrics.VolumeFetchIO {
		t.Errorf("EIO = %v (%s), want an io failure", other, resultOf(other))
	}
	for _, err := range []error{full, other} {
		if strings.Contains(err.Error(), "/cache") {
			t.Errorf("error names the path: %v", err)
		}
	}
}

func TestDownloadTimesOut(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(filepath.Join(root, "cache"), filepath.Join(root, "data"))
	cfg.FetchTimeout = 100 * time.Millisecond
	cache, rec := openCache(t, cfg)
	srv := serve(t, []byte("x"), true)
	defer srv.open()

	if _, err := srv.fetch(cache, "sandbox-a"); err == nil {
		t.Fatal("Acquire() succeeded, want a timeout")
	}
	wantFetches(t, rec, metrics.VolumeFetchTimeout, 1)
	if used := cache.usedBytes(); used != 0 {
		t.Errorf("budget still counts %d bytes of the failed download", used)
	}
}

func TestConcurrentAcquireDownloadsOnce(t *testing.T) {
	cache, rec := newTestCache(t, 1<<30)
	srv := serve(t, randomBytes(t, 64<<10), true)
	key := keyFor(srv.data)

	const acquirers = 8
	paths := make([]string, acquirers)
	errs := make([]error, acquirers)
	var wg sync.WaitGroup
	for worker := range acquirers {
		wg.Go(func() { paths[worker], errs[worker] = srv.fetch(cache, "sandbox-"+strconv.Itoa(worker)) })
	}
	waitFor(t, "every Acquire to wait on the download", func() bool { return cache.waiting(key) == acquirers })
	srv.open()
	wg.Wait()

	for worker := range acquirers {
		if errs[worker] != nil || paths[worker] != paths[0] {
			t.Errorf("Acquire %d = %q, %v; want %q", worker, paths[worker], errs[worker], paths[0])
		}
	}
	if hits := srv.hits.Load(); hits != 1 {
		t.Errorf("server hits = %d, want 1", hits)
	}
	if holders := cache.holders(key); len(holders) != acquirers {
		t.Errorf("holders = %v, want all %d", holders, acquirers)
	}
	wantFetches(t, rec, metrics.VolumeFetchHit, acquirers-1)
}

// The caller that started a download gives up; the download, and the caller
// still waiting for it, carry on.
func TestWaiterCancelDoesNotAbortDownload(t *testing.T) {
	cache, rec := newTestCache(t, 1<<30)
	srv := serve(t, randomBytes(t, 64<<10), true)
	key := keyFor(srv.data)

	ctx, cancel := context.WithCancel(context.Background())
	starter := make(chan error, 1)
	go func() {
		_, err := cache.Acquire(ctx, key, srv.source(), "sandbox-starter")
		starter <- err
	}()
	waitFor(t, "the download to start", func() bool { return srv.hits.Load() == 1 })
	other := srv.fetchAsync(cache, "sandbox-other")
	waitFor(t, "both Acquires to wait", func() bool { return cache.waiting(key) == 2 })

	cancel()
	if err := <-starter; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Acquire returned %v, want context.Canceled", err)
	}
	srv.open()
	if err := <-other; err != nil {
		t.Fatalf("the other Acquire failed: %v", err)
	}
	if holders := cache.holders(key); len(holders) != 1 || holders[0] != "sandbox-other" {
		t.Errorf("holders = %v, want only sandbox-other", holders)
	}
	wantFetches(t, rec, metrics.VolumeFetchDownloaded, 1)
}

// A full budget evicts unused objects, least recently used first; pinned ones
// never go, and when only they are left the fetch is refused like a full
// runner.
func TestBudgetEvictsLeastRecentlyUsed(t *testing.T) {
	cache, rec := newTestCache(t, 1<<30)
	servers := make([]*server, 4)
	paths := make([]string, 3)
	for index := range servers {
		servers[index] = serve(t, randomBytes(t, 64<<10), false)
	}
	for index := range paths {
		paths[index] = servers[index].acquire(t, cache, "sandbox-"+strconv.Itoa(index))
	}
	cache.setMaxBytes(cache.usedBytes())
	extra := servers[3]

	_, err := extra.fetch(cache, "sandbox-3")
	if !errors.Is(err, ErrCacheFull) || !errors.Is(err, runnerruntime.ErrCapacityExhausted) {
		t.Fatalf("Acquire() with every object pinned = %v, want ErrCacheFull as a capacity refusal", err)
	}
	if hits := extra.hits.Load(); hits != 0 {
		t.Errorf("a refused fetch downloaded anyway (%d requests)", hits)
	}
	wantFetches(t, rec, metrics.VolumeFetchFull, 1)
	wantEvictions(t, rec, 0)

	for index := range paths {
		cache.Release(keyFor(servers[index].data), "sandbox-"+strconv.Itoa(index))
	}
	// Object 0 becomes the most recently used; object 1 is now the least.
	servers[0].acquire(t, cache, "sandbox-x")
	cache.Release(keyFor(servers[0].data), "sandbox-x")
	extra.acquire(t, cache, "sandbox-3")

	wantGone(t, paths[1])
	for _, index := range []int{0, 2} {
		if _, err := os.Stat(paths[index]); err != nil {
			t.Errorf("object %d evicted: %v", index, err)
		}
	}
	wantEvictions(t, rec, 1)
	if _, entryCount, _ := cache.stats(); entryCount != 3 {
		t.Errorf("entries = %d, want 3", entryCount)
	}
}

// Room a download in progress has yet to fill is not offered again, in the
// budget or on the disk. A download that exactly fits goes ahead.
func TestDownloadsInProgressHoldTheirRoom(t *testing.T) {
	const size, total = 64 << 10, 100 << 20
	for name, limit := range map[string]func(*Cache){
		"budget": func(cache *Cache) { cache.setMaxBytes(size) },
		"disk":   func(cache *Cache) { cache.setDisk(total*runnerruntime.DiskFloorPercent/100+size, total) },
	} {
		t.Run(name, func(t *testing.T) {
			cache, rec := newTestCache(t, 1<<30)
			limit(cache)
			first, second := serve(t, randomBytes(t, size), true), serve(t, randomBytes(t, size), false)
			done := first.fetchAsync(cache, "sandbox-a")
			waitFor(t, "the first download to start", func() bool { return first.hits.Load() == 1 })
			if used := cache.usedBytes(); used != size {
				t.Errorf("budget counts %d bytes for a download in progress, want %d", used, size)
			}

			if _, err := second.fetch(cache, "sandbox-b"); !errors.Is(err, ErrCacheFull) {
				t.Errorf("Acquire() of room a download in progress holds = %v, want ErrCacheFull", err)
			}
			first.open()
			if err := <-done; err != nil {
				t.Fatalf("first Acquire() failed: %v", err)
			}
			wantFetches(t, rec, metrics.VolumeFetchFull, 1)
		})
	}
}

// A download leaves the disk floor to the sandboxes on the disk: it evicts
// unused objects, least recently used first and only as many as it needs, and
// is refused like a full runner when that is not enough.
func TestDownloadKeepsTheDiskFloor(t *testing.T) {
	const size, total = 64 << 10, 100 << 20
	cache, rec := newTestCache(t, 1<<30)
	older, newer, fresh := serve(t, randomBytes(t, size), false), serve(t, randomBytes(t, size), false), serve(t, randomBytes(t, size), false)
	olderPath := older.acquire(t, cache, "sandbox-a")
	newerPath := newer.acquire(t, cache, "sandbox-b")
	cache.setDisk(total*runnerruntime.DiskFloorPercent/100+size-1, total)

	_, err := fresh.fetch(cache, "sandbox-c")
	if !errors.Is(err, runnerruntime.ErrCapacityExhausted) || !strings.Contains(err.Error(), "floor") {
		t.Fatalf("Acquire() into the floor: error = %v, want a capacity refusal naming the floor", err)
	}
	if hits := fresh.hits.Load(); hits != 0 {
		t.Errorf("a refused fetch downloaded anyway (%d requests)", hits)
	}
	wantFetches(t, rec, metrics.VolumeFetchFull, 1)

	cache.Release(keyFor(older.data), "sandbox-a")
	cache.Release(keyFor(newer.data), "sandbox-b")
	fresh.acquire(t, cache, "sandbox-c")
	wantGone(t, olderPath)
	if _, err := os.Stat(newerPath); err != nil {
		t.Errorf("evicted more than the floor needed: %v", err)
	}
	wantEvictions(t, rec, 1)

	// The evicted object leaves the budget and the reservation.
	cache.mu.Lock()
	var left int64
	for _, cached := range cache.entries {
		left += cached.allocatedBytes
	}
	cache.mu.Unlock()
	if used := cache.usedBytes(); used != left {
		t.Errorf("budget counts %d bytes, want the %d of the objects left", used, left)
	}
	if got := cache.DataDirReservation(); got != 1<<30-left {
		t.Errorf("reservation = %d, want the budget less the %d bytes left", got, left)
	}
}

// Creates leave free whatever of the budget the cache has not written yet.
func TestDataDirReservation(t *testing.T) {
	cache, _ := newTestCache(t, 1<<30)
	if got := cache.DataDirReservation(); got != 1<<30 {
		t.Fatalf("reservation of an empty cache = %d, want the whole 1 GiB budget", got)
	}
	serve(t, randomBytes(t, 64<<10), false).acquire(t, cache, "sandbox-a")
	cache.mu.Lock()
	want := 1<<30 - cache.allocatedBytes
	cache.mu.Unlock()
	if got := cache.DataDirReservation(); got != want {
		t.Errorf("reservation = %d, want the budget less what is on disk, %d", got, want)
	}

	// A download in progress has written little yet, so its share stays set
	// aside.
	gated := serve(t, randomBytes(t, 64<<10), true)
	done := gated.fetchAsync(cache, "sandbox-b")
	waitFor(t, "the download to start", func() bool { return gated.hits.Load() == 1 })
	if got := cache.DataDirReservation(); got != want {
		t.Errorf("reservation during a download = %d, want it unchanged at %d", got, want)
	}
	gated.open()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	// A cache on another filesystem takes nothing from the data volume.
	if sameFilesystem(cache.config.Dir, "/dev") {
		t.Skip("/dev is on the same filesystem as the test dir")
	}
	cache.config.DataDir = "/dev"
	if got := cache.DataDirReservation(); got != 0 {
		t.Errorf("reservation on another filesystem = %d, want 0", got)
	}
}

func TestSameFilesystem(t *testing.T) {
	dir := t.TempDir()
	if !sameFilesystem(dir, filepath.Join(dir, "missing")) {
		t.Error("a missing path is not judged by its parent")
	}
	if !sameFilesystem(dir, "/does/not/exist/at/all") {
		t.Error("an unreadable path did not count as the same filesystem")
	}
	if sameFilesystem(dir, "/dev") {
		t.Skip("/dev is on the same filesystem as the test dir")
	}
	if sameFilesystem(dir, "/dev/missing-volume-cache-test") {
		t.Error("a missing path on another filesystem was not judged by its parent")
	}
}

func TestStatDiskReadsTheFilesystem(t *testing.T) {
	space, err := statDisk(t.TempDir())
	if err != nil {
		t.Fatalf("statDisk() failed: %v", err)
	}
	if space.total <= 0 || space.free < 0 || space.free > space.total {
		t.Fatalf("statDisk() = %+v, want 0 <= free <= total and total > 0", space)
	}
}

func TestAcquireRefusesObjectLargerThanTheCache(t *testing.T) {
	cache, rec := newTestCache(t, 1024)
	_, err := serve(t, randomBytes(t, 2048), false).fetch(cache, "sandbox-a")
	if err == nil || errors.Is(err, runnerruntime.ErrCapacityExhausted) {
		t.Fatalf("Acquire() error = %v, want a refusal no other runner would lift", err)
	}
	wantFetches(t, rec, metrics.VolumeFetchFull, 1)
}

func TestSparseWrite(t *testing.T) {
	const size = 8 << 20
	data := make([]byte, size)
	copy(data, "header")
	data[3<<20+5] = 1
	for offset := 5 << 20; offset < 5<<20+3*sparseBlock; offset++ {
		data[offset] = byte(offset)
	}
	// The last 2 MiB stay zero, so the file ends in a hole that only the
	// final truncate creates.

	cache, _ := newTestCache(t, 1<<30)
	skipWithoutHoles(t, cache.config.Dir)
	path := serve(t, data, false).acquire(t, cache, "sandbox-a")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("cached content differs from the served object")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != size {
		t.Errorf("apparent size = %d, want %d", info.Size(), size)
	}
	alloc := int64(info.Sys().(*syscall.Stat_t).Blocks) * 512
	if alloc >= size/4 {
		t.Errorf("allocated %d bytes for a mostly-zero %d-byte object; want it sparse", alloc, size)
	}
	if used := cache.usedBytes(); used != alloc {
		t.Errorf("budget counts %d bytes, want the allocated %d", used, alloc)
	}
}

// skipWithoutHoles skips on filesystems that fill a hole a write skips over,
// such as APFS.
func skipWithoutHoles(t *testing.T, dir string) {
	t.Helper()
	probe, err := os.CreateTemp(dir, "probe-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(probe.Name())
	if _, err := probe.WriteAt([]byte{1}, 8<<20); err != nil {
		t.Fatal(err)
	}
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(probe.Name())
	if err != nil {
		t.Fatal(err)
	}
	if int64(info.Sys().(*syscall.Stat_t).Blocks)*512 >= 8<<20 {
		t.Skip("filesystem does not keep holes")
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	cache, _ := newTestCache(t, 1<<30)
	srv := serve(t, randomBytes(t, 4096), false)
	key := keyFor(srv.data)

	srv.acquire(t, cache, "sandbox-a")
	srv.acquire(t, cache, "sandbox-a")
	if holders := cache.holders(key); len(holders) != 1 {
		t.Fatalf("holders after a retried Acquire = %v, want one pin", holders)
	}
	cache.Release(key, "sandbox-a")
	if _, _, pinned := cache.stats(); pinned != 0 {
		t.Fatalf("pinned = %d after one Release of a retried Acquire, want 0", pinned)
	}
	cache.Release(key, "sandbox-a")
	cache.Release(keyFor([]byte("unknown")), "sandbox-a")
	cache.ReleaseAll("nobody")

	srv.acquire(t, cache, "sandbox-a")
	srv.acquire(t, cache, "sandbox-b")
	cache.ReleaseAll("sandbox-a")
	cache.ReleaseAll("sandbox-a")
	if holders := cache.holders(key); len(holders) != 1 || holders[0] != "sandbox-b" {
		t.Errorf("holders = %v, want only sandbox-b", holders)
	}
	cache.ReleaseAll("sandbox-b")
	if _, entryCount, pinned := cache.stats(); entryCount != 1 || pinned != 0 {
		t.Errorf("entries/pinned = %d/%d, want 1/0: release unpins but keeps the object", entryCount, pinned)
	}
}

// Whatever is at the cache dir, New leaves an empty directory there that only
// the runner can enter, and never touches the data dir. An existing directory
// is emptied in place, so a cache dir that is a mount point survives.
func TestNewPreparesCacheDir(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, dir, dataDir string){
		"missing": func(*testing.T, string, string) {},
		"stale files": func(t *testing.T, dir, _ string) {
			if err := os.MkdirAll(filepath.Join(dir, "stale-subdir"), 0o755); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"shared.old", ".download-123", "stale-subdir/x"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("stale"), 0o444); err != nil {
					t.Fatal(err)
				}
			}
		},
		"symlink to the data dir": func(t *testing.T, dir, dataDir string) {
			if err := os.Symlink(dataDir, dir); err != nil {
				t.Fatal(err)
			}
		},
		// Its owner could rename the read-only files in it.
		"owned by another uid": func(t *testing.T, dir, _ string) {
			if os.Geteuid() != 0 {
				t.Skip("needs root to chown")
			}
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chown(dir, 1000, 1000); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dir, dataDir := filepath.Join(root, "cache"), filepath.Join(root, "data")
			keep := filepath.Join(dataDir, "sandbox-file")
			if err := os.Mkdir(dataDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(keep, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			setup(t, dir, dataDir)
			before, beforeErr := os.Lstat(dir)

			if _, err := New(testConfig(dir, dataDir)); err != nil {
				t.Fatalf("New() failed: %v", err)
			}
			info, err := os.Lstat(dir)
			if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 || int(info.Sys().(*syscall.Stat_t).Uid) != os.Geteuid() {
				t.Fatalf("cache dir = %v, %v; want a directory of ours, mode 700", info, err)
			}
			if names := dirNames(t, dir); len(names) != 0 {
				t.Errorf("cache dir holds %v, want nothing", names)
			}
			if beforeErr == nil && before.IsDir() && !os.SameFile(before, info) {
				t.Error("New replaced the cache dir instead of emptying it")
			}
			if _, err := os.Stat(keep); err != nil {
				t.Errorf("data dir touched: %v", err)
			}
		})
	}
}

func TestNewRefusesUnsafeDir(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.Mkdir(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{
		"relative":                   "volume-cache",
		"root":                       "/",
		"the data dir":               dataDir,
		"the data dir, unclean":      dataDir + "/./",
		"parent of the data dir":     root,
		"data dir through a symlink": filepath.Join(root, "link", "data"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(testConfig(dir, dataDir)); err == nil {
				t.Fatalf("New() accepted %q with data dir %q", dir, dataDir)
			}
		})
	}
	if _, err := os.Stat(dataDir); err != nil {
		t.Fatalf("data dir gone: %v", err)
	}
}

func TestCacheDirWaitsForItsParent(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "volume")
	dir := filepath.Join(parent, "cache")
	cache, rec := openCache(t, testConfig(dir, filepath.Join(root, "data")))
	if _, err := os.Stat(parent); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("New() created the missing parent: %v", err)
	}
	srv := serve(t, randomBytes(t, 4096), false)

	if _, err := srv.fetch(cache, "sandbox-a"); err == nil {
		t.Fatal("Acquire() without the parent succeeded")
	}
	wantFetches(t, rec, metrics.VolumeFetchIO, 1)
	// A volume mounted late can bring a cache dir from an earlier process.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stale"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	srv.acquire(t, cache, "sandbox-a")
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("cache dir = %v, %v; want mode 700", info, err)
	}
	if names := dirNames(t, dir); len(names) != 1 || names[0] == "stale" {
		t.Errorf("cache dir holds %v, want only the new object", names)
	}
}
