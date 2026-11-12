package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/daemon/volumedrives"
)

// fakeSys records the system calls of volume mounts, and dropping root and
// exiting with them. Every drive reports 512 bytes.
type fakeSys struct {
	mu        sync.Mutex
	calls     []string
	errs      map[string]error // keyed by the recorded call
	panicSize bool
}

func newFakeSys() *fakeSys {
	return &fakeSys{errs: map[string]error{}}
}

func (f *fakeSys) record(call string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	return f.errs[call]
}

func (f *fakeSys) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeSys) DeviceSize(string) (uint64, error) {
	if f.panicSize {
		panic("boom")
	}
	return 512, nil
}

func (f *fakeSys) FlushBuffers(device string) error { return f.record("flush " + device) }
func (f *fakeSys) MakeMountPoint(path string) error { return f.record("mkdir " + path) }
func (f *fakeSys) Mount(device, path string) error  { return f.record("mount " + device + " " + path) }
func (f *fakeSys) Unmount(path string) error        { return f.record("unmount " + path) }

func requireCalls(t *testing.T, sys *fakeSys, want ...string) {
	t.Helper()
	if got := sys.recorded(); !slices.Equal(got, want) {
		t.Fatalf("calls:\n got %q\nwant %q", got, want)
	}
}

// newRootHandler is the handler of a daemon started as root, with fake system
// calls and a fake euid.
func newRootHandler(t *testing.T, sys *fakeSys) *Handler {
	t.Helper()
	var euid atomic.Int32
	h := NewHandler()
	h.euid = func() int { return int(euid.Load()) }
	h.volumes = volumedrives.NewMounter(sys)
	h.dropRoot = func() error {
		if err := sys.record("drop root"); err != nil {
			return err
		}
		euid.Store(1000)
		return nil
	}
	h.exit = func(code int) { _ = sys.record(fmt.Sprintf("exit %d", code)) }
	t.Cleanup(h.Close)
	return h
}

func volumesBody(t *testing.T, volumes ...volumedrives.Volume) string {
	t.Helper()
	body, err := json.Marshal(map[string][]volumedrives.Volume{"volumes": volumes})
	if err != nil {
		t.Fatalf("marshal volumes: %v", err)
	}
	return string(body)
}

var oneVolume = volumedrives.Volume{Drive: 0, Sectors: 1, Path: "/mnt/a"}

func postVolumes(handler http.Handler, body string) *httptest.ResponseRecorder {
	return serve(handler, http.MethodPost, "/volumes/mount", body)
}

func serve(handler http.Handler, method, target, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func TestVolumeMountEndpointMountsThenDropsRoot(t *testing.T) {
	sys := newFakeSys()
	handler := newRootHandler(t, sys)

	body := volumesBody(t, oneVolume)
	if rr := postVolumes(handler, body); rr.Code != http.StatusNoContent {
		t.Fatalf("first request: status %d, want 204: %s", rr.Code, rr.Body.String())
	}
	for _, later := range []string{body, `{"volumes":[]}`, `not json`} {
		if rr := postVolumes(handler, later); rr.Code != http.StatusConflict {
			t.Fatalf("later request %s: status %d, want 409: %s", later, rr.Code, rr.Body.String())
		}
	}
	requireCalls(t, sys, "flush /dev/vdb", "mkdir /mnt/a", "mount /dev/vdb /mnt/a", "drop root")
}

func TestVolumeMountEndpointDropsRootWithAnEmptyList(t *testing.T) {
	sys := newFakeSys()
	handler := newRootHandler(t, sys)

	if rr := postVolumes(handler, `{"volumes":[]}`); rr.Code != http.StatusNoContent {
		t.Fatalf("status %d, want 204: %s", rr.Code, rr.Body.String())
	}
	requireCalls(t, sys, "drop root")
}

// The runner sends the request once, so root goes whatever its outcome.
func TestVolumeMountEndpointDropsRootWhenTheRequestFails(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		status   int
		wantBody string
	}{
		{"invalid", volumesBody(t, volumedrives.Volume{Drive: 0, Sectors: 1, Path: "/etc/cron.d"}), http.StatusBadRequest, "is not below"},
		{"too large", `{"volumes":[]}` + strings.Repeat(" ", volumedrives.MaxRequestBytes), http.StatusBadRequest, "too large"},
		{"not JSON", `volumes`, http.StatusBadRequest, "invalid character"},
		{"mount failed", volumesBody(t, oneVolume), http.StatusInternalServerError,
			`volume drive 0 at /mnt/a: mount /dev/vdb: invalid argument","code":500,"reason":"mount"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sys := newFakeSys()
			sys.errs["mount /dev/vdb /mnt/a"] = errors.New("invalid argument")
			handler := newRootHandler(t, sys)

			rr := postVolumes(handler, tt.body)
			if rr.Code != tt.status || !strings.Contains(rr.Body.String(), tt.wantBody) {
				t.Fatalf("status %d body %s, want %d with %q", rr.Code, rr.Body.String(), tt.status, tt.wantBody)
			}
			if calls := sys.recorded(); calls[len(calls)-1] != "drop root" {
				t.Fatalf("calls %q, want root dropped last", calls)
			}
			if rr := postVolumes(handler, `{"volumes":[]}`); rr.Code != http.StatusConflict {
				t.Fatalf("later request: status %d, want 409: %s", rr.Code, rr.Body.String())
			}
		})
	}
}

// captureLogs redirects the default logger into the returned buffer.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

func TestVolumeMountEndpointExitsWhenRootCannotBeDropped(t *testing.T) {
	logs := captureLogs(t)
	sys := newFakeSys()
	sys.errs["drop root"] = errors.New("operation not permitted")
	sys.errs["mount /dev/vdb /mnt/a"] = errors.New("bad superblock")
	handler := newRootHandler(t, sys)

	postVolumes(handler, volumesBody(t, oneVolume))
	requireCalls(t, sys, "flush /dev/vdb", "mkdir /mnt/a", "mount /dev/vdb /mnt/a", "drop root", "exit 1")
	if rr := serve(handler, http.MethodGet, "/stat?path=/", ""); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /stat: status %d, want 503 while the daemon holds root", rr.Code)
	}
	if !strings.Contains(logs.String(), "operation not permitted") || !strings.Contains(logs.String(), "bad superblock") {
		t.Fatalf("logs = %s, want both the drop failure and the mount failure", logs.String())
	}
}

func TestVolumeMountEndpointLogsAndExitsWhenTheMountPanics(t *testing.T) {
	logs := captureLogs(t)
	sys := newFakeSys()
	sys.panicSize = true
	handler := newRootHandler(t, sys)

	postVolumes(handler, volumesBody(t, oneVolume))
	requireCalls(t, sys, "exit 1")
	if !strings.Contains(logs.String(), `"panic":"boom"`) || !strings.Contains(logs.String(), "DeviceSize") {
		t.Fatalf("logs = %s, want the panic and its stack", logs.String())
	}
}

func TestVolumeMountEndpointHandlesOneOfConcurrentRequests(t *testing.T) {
	sys := newFakeSys()
	handler := newRootHandler(t, sys)

	const requests = 16
	codes := make(chan int, requests)
	var start, wg sync.WaitGroup
	start.Add(1)
	for range requests {
		wg.Go(func() {
			start.Wait()
			codes <- postVolumes(handler, volumesBody(t, oneVolume)).Code
		})
	}
	start.Done()
	wg.Wait()
	close(codes)

	counts := map[int]int{}
	for code := range codes {
		counts[code]++
	}
	if counts[http.StatusNoContent] != 1 || counts[http.StatusConflict] != requests-1 {
		t.Fatalf("status counts = %v, want one 204 and %d 409", counts, requests-1)
	}
	requireCalls(t, sys, "flush /dev/vdb", "mkdir /mnt/a", "mount /dev/vdb /mnt/a", "drop root")
}

func TestRootDaemonServesOnlyTheVolumeMountRequestUntilItDropsRoot(t *testing.T) {
	dir := t.TempDir()
	handler := newRootHandler(t, newFakeSys())

	refused := []struct{ method, target string }{
		{http.MethodPost, "/executions"},
		{http.MethodGet, "/files?path=" + dir},
		{http.MethodPut, "/files?path=" + dir + "/x"},
		{http.MethodGet, "/stat?path=" + dir},
		{http.MethodHead, "/healthz"},
		{http.MethodGet, "/healthz/"},
		{http.MethodPost, "/volumes%2Fmount"},
		{http.MethodGet, "/no-such-route"},
	}
	for _, r := range refused {
		if rr := serve(handler, r.method, r.target, `{"command":"true"}`); rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s before the volume mount request: status %d, want 503", r.method, r.target, rr.Code)
		}
	}
	if rr := serve(handler, http.MethodGet, "/healthz", ""); rr.Code != http.StatusOK {
		t.Fatalf("GET /healthz: status %d, want 200", rr.Code)
	}

	if rr := postVolumes(handler, `{"volumes":[]}`); rr.Code != http.StatusNoContent {
		t.Fatalf("volume mount request: status %d, want 204: %s", rr.Code, rr.Body.String())
	}
	if rr := serve(handler, http.MethodGet, "/stat?path="+dir, ""); rr.Code != http.StatusOK {
		t.Fatalf("GET /stat after the volume mount request: status %d, want 200: %s", rr.Code, rr.Body.String())
	}
}

// The Docker daemon starts as uid 1000, and so does any test.
func TestVolumeMountEndpointWithoutRoot(t *testing.T) {
	handler := NewHandler()
	defer handler.Close()

	for range 2 {
		if rr := postVolumes(handler, `{"volumes":[]}`); rr.Code != http.StatusNotImplemented {
			t.Fatalf("status %d, want 501: %s", rr.Code, rr.Body.String())
		}
	}
}

// The gate follows the real euid, not whether there is a volume mounter.
func TestRootDaemonWithoutAVolumeMounterServesNothingElse(t *testing.T) {
	handler := NewHandler()
	defer handler.Close()
	handler.euid = func() int { return 0 }

	if rr := serve(handler, http.MethodGet, "/stat?path="+t.TempDir(), ""); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /stat: status %d, want 503", rr.Code)
	}
	if rr := postVolumes(handler, `{"volumes":[]}`); rr.Code != http.StatusNotImplemented {
		t.Fatalf("volume mount request: status %d, want 501", rr.Code)
	}
	if rr := serve(handler, http.MethodGet, "/healthz", ""); rr.Code != http.StatusOK {
		t.Fatalf("GET /healthz: status %d, want 200", rr.Code)
	}
}
