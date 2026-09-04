package api

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/store"
)

// A sandbox that only ever answers with server errors must not be able to keep
// itself alive. The idle sweeper stops on a stale last_active_at and deletes
// only what it has already stopped, so a retrying client must move neither.
func TestSandboxProxyDoesNotCountServerErrorsAsActivity(t *testing.T) {
	var upstreamStatus atomic.Int64
	upstreamStatus.Store(http.StatusOK)

	runner := newTestRunnerServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(upstreamStatus.Load()))
		_, _ = w.Write([]byte(`{"ok":false}`))
	}))
	defer runner.Close()

	router, s := newTestGateway(t, "admin-key")

	const sid = "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa"
	// A stopped sandbox with a stale last_active_at is what the sweeper is
	// about to reclaim.
	const staleActive = int64(1)
	if err := s.Create(&store.SandboxRecord{
		ID: sid, Status: "stopped", CreatedAt: 1, LastActiveAt: staleActive,
		TenantID: store.AdminTenantID, RunnerHTTPBase: runner.URL,
	}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	call := func() *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/sandboxes/"+sid+"/files", nil)
		req.Header.Set("X-Api-Key", "admin-key")
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}

	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable} {
		upstreamStatus.Store(int64(status))
		if rr := call(); rr.Code != status {
			t.Fatalf("upstream %d: proxy returned %d", status, rr.Code)
		}

		rec, err := s.Get(sid)
		if err != nil {
			t.Fatalf("get sandbox: %v", err)
		}
		if rec.LastActiveAt != staleActive {
			t.Errorf("upstream %d: last_active_at moved to %d, want %d", status, rec.LastActiveAt, staleActive)
		}
		if rec.Status != "stopped" {
			t.Errorf("upstream %d: status became %q, want stopped", status, rec.Status)
		}
	}

	// A working sandbox still counts, so the assertions above are about the
	// status code and not a bump that never happens.
	upstreamStatus.Store(http.StatusOK)
	if rr := call(); rr.Code != http.StatusOK {
		t.Fatalf("upstream 200: proxy returned %d", rr.Code)
	}
	rec, err := s.Get(sid)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if rec.LastActiveAt <= staleActive {
		t.Errorf("last_active_at = %d, want a fresh timestamp", rec.LastActiveAt)
	}
	if rec.Status != "running" {
		t.Errorf("status = %q, want running", rec.Status)
	}
}

// A 4xx means the caller sent something the sandbox rejected, which still
// proves the sandbox is up and in use.
func TestSandboxProxyCountsClientErrorsAsActivity(t *testing.T) {
	runner := newTestRunnerServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid path"}`))
	}))
	defer runner.Close()

	router, s := newTestGateway(t, "admin-key")

	const sid = "bbbbbbbb-2222-4222-8222-bbbbbbbbbbbb"
	if err := s.Create(&store.SandboxRecord{
		ID: sid, Status: "stopped", CreatedAt: 1, LastActiveAt: 1,
		TenantID: store.AdminTenantID, RunnerHTTPBase: runner.URL,
	}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/sandboxes/"+sid+"/files", nil)
	req.Header.Set("X-Api-Key", "admin-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("proxy returned %d, want 400", rr.Code)
	}

	rec, err := s.Get(sid)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if rec.LastActiveAt <= 1 {
		t.Errorf("last_active_at = %d, want a fresh timestamp", rec.LastActiveAt)
	}
	if rec.Status != "running" {
		t.Errorf("status = %q, want running", rec.Status)
	}
}

// An unreachable runner produces no upstream response at all, so the proxy's
// ErrorHandler path must not count as activity either.
func TestSandboxProxyDoesNotCountUnreachableRunnerAsActivity(t *testing.T) {
	runner := newTestRunnerServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	runnerURL := runner.URL
	runner.Close()

	router, s := newTestGateway(t, "admin-key")

	const sid = "cccccccc-3333-4333-8333-cccccccccccc"
	const staleActive = int64(1)
	if err := s.Create(&store.SandboxRecord{
		ID: sid, Status: "stopped", CreatedAt: 1, LastActiveAt: staleActive,
		TenantID: store.AdminTenantID, RunnerHTTPBase: runnerURL,
	}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/sandboxes/"+sid+"/files", nil)
	req.Header.Set("X-Api-Key", "admin-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("proxy returned %d, want 503", rr.Code)
	}

	rec, err := s.Get(sid)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if rec.LastActiveAt != staleActive {
		t.Errorf("last_active_at moved to %d, want %d", rec.LastActiveAt, staleActive)
	}
	if rec.Status != "stopped" {
		t.Errorf("status = %q, want stopped", rec.Status)
	}
}

// Registration rejects plaintext bases, but rows written before that rule still
// carry them. Proxying such a row would send the runner API key in the clear,
// because Transport.TLSClientConfig applies only to https URLs.
func TestSandboxProxyRefusesPlaintextRunnerBase(t *testing.T) {
	var upstreamHits atomic.Int64
	runner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer runner.Close()

	router, s := newTestGateway(t, "admin-key")

	const sid = "dddddddd-4444-4444-8444-dddddddddddd"
	if err := s.Create(&store.SandboxRecord{
		ID: sid, Status: "running", CreatedAt: 1, LastActiveAt: 1,
		TenantID: store.AdminTenantID, RunnerHTTPBase: runner.URL,
	}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/sandboxes/"+sid+"/files", nil)
	req.Header.Set("X-Api-Key", "admin-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("proxy returned %d, want %d", rr.Code, http.StatusBadGateway)
	}
	if hits := upstreamHits.Load(); hits != 0 {
		t.Fatalf("expected nothing to reach the plaintext runner, got %d requests", hits)
	}
}

// The idle-delete fence is the tenant-visible contract and must win over
// routing validation: a row that predates runner_http_base_url has no base,
// but once it is past its window the proxy still answers 404, not 502.
func TestSandboxProxyFencesExpiredSandboxWithoutRunnerBase(t *testing.T) {
	router, s, cfg := newIdleTestGateway(t, "admin-key")

	stale := time.Now().Add(-cfg.IdleStopAfter - time.Second).Unix()
	const sid = "eeeeeeee-5555-4555-8555-eeeeeeeeeeee"
	if err := s.Create(&store.SandboxRecord{
		ID: sid, Status: "running", CreatedAt: stale, LastActiveAt: stale,
		TenantID: store.AdminTenantID, Ephemeral: true,
	}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/sandboxes/"+sid+"/files", nil)
	req.Header.Set("X-Api-Key", "admin-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("proxy returned %d body=%s, want %d", rr.Code, rr.Body.String(), http.StatusNotFound)
	}
}
