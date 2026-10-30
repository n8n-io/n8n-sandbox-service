package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/metrics"
)

// newActivityTestGateway is newTestGateway with the activity recorder exposed,
// so a test can wait for its writes.
func newActivityTestGateway(t *testing.T, adminKey string) (http.Handler, store.SandboxStore, *ActivityRecorder) {
	t.Helper()
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	activity := NewActivityRecorder(s, &config.APIConfig{ActivityWriteInterval: time.Hour})
	router, err := NewGatewayRouter(s, withRunnerTLS(&config.APIConfig{
		APIKeys:      map[string]struct{}{adminKey: {}},
		RunnerAPIKey: "runner-key",
		MaxFileBytes: 1024,
	}), registry.New(45*time.Second), metrics.NewAPIRecorder(false), activity)
	if err != nil {
		t.Fatalf("create gateway router: %v", err)
	}
	return router, s, activity
}

// inflight returns how many responses from sandbox id are still streaming.
func (a *ActivityRecorder) inflight(id string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e, ok := a.sandboxes[id]; ok {
		return e.open
	}
	return 0
}

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

	router, s, activity := newActivityTestGateway(t, "admin-key")

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
		activity.writes.Wait()

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
	activity.writes.Wait()
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

	router, s, activity := newActivityTestGateway(t, "admin-key")

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
	activity.writes.Wait()

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

	router, s, activity := newActivityTestGateway(t, "admin-key")

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
	activity.writes.Wait()

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
	router, s, cfg := newIdleTestGateway(t, "admin-key", &fakeSandboxControl{})

	stale := time.Now().Add(-cfg.IdleStopAfter - activityLag(cfg) - time.Second).Unix()
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

// A response that is still streaming, such as a long exec, is written every
// interval until it ends. Otherwise a command longer than the idle stop window
// would have its sandbox stopped under it.
func TestSandboxProxyCountsStreamingResponseUntilItEnds(t *testing.T) {
	release := make(chan struct{})
	runner := newTestRunnerServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"type":"started"}` + "\n"))
		w.(http.Flusher).Flush()
		<-release
		_, _ = w.Write([]byte(`{"type":"exit","exit_code":0}` + "\n"))
	}))
	defer runner.Close()

	router, s, activity := newActivityTestGateway(t, "admin-key")
	var clock atomic.Int64
	clock.Store(1_000_000)
	activity.now = func() time.Time { return time.Unix(clock.Load(), 0) }

	const sid = "abababab-6666-4666-8666-abababababab"
	if err := s.Create(&store.SandboxRecord{
		ID: sid, Status: "running", CreatedAt: 1, LastActiveAt: 1,
		TenantID: store.AdminTenantID, RunnerHTTPBase: runner.URL,
	}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	done := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/sandboxes/"+sid+"/executions/12121212-1212-4212-8212-121212121212?follow=true", nil)
		req.Header.Set("X-Api-Key", "admin-key")
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		done <- rr.Code
	}()
	waitFor(t, "the stream to start", func() bool { return activity.inflight(sid) == 1 })

	for _, at := range []int64{1_000_000, 1_003_600, 1_007_200} {
		clock.Store(at)
		if at > 1_000_000 {
			activity.writeOpen()
		}
		activity.writes.Wait()
		if got := mustGet(t, s, sid).LastActiveAt; got != at {
			t.Fatalf("at %d while streaming: last_active_at = %d, want %d", at, got, at)
		}
	}

	close(release)
	if code := <-done; code != http.StatusOK {
		t.Fatalf("proxy returned %d, want 200", code)
	}
	if n := activity.inflight(sid); n != 0 {
		t.Fatalf("in flight after the stream ended = %d, want 0", n)
	}
	clock.Store(1_010_800)
	activity.writeOpen()
	activity.writes.Wait()
	if got := mustGet(t, s, sid).LastActiveAt; got != 1_007_200 {
		t.Fatalf("after the stream: last_active_at = %d, want 1007200 unchanged", got)
	}
}

// A request that wakes a stopped sandbox, or comes after a quiet spell, is
// written at once: GET shows the sandbox running straight away, and the
// sweeper never acts on the old row while the request is served.
func TestSandboxProxyWritesWakeAndReturnAfterQuietAtOnce(t *testing.T) {
	runner := newTestRunnerServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer runner.Close()

	router, s, activity := newActivityTestGateway(t, "admin-key")
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		activity.Run(ctx)
		close(stopped)
	}()
	t.Cleanup(func() {
		cancel()
		<-stopped
	})

	for _, tc := range []struct{ sid, status string }{
		{"cdcdcdcd-7777-4777-8777-cdcdcdcdcdcd", "stopped"},
		{"efefefef-8888-4888-8888-efefefefefef", "running"},
	} {
		if err := s.Create(&store.SandboxRecord{
			ID: tc.sid, Status: tc.status, CreatedAt: 1, LastActiveAt: 1,
			TenantID: store.AdminTenantID, RunnerHTTPBase: runner.URL,
		}); err != nil {
			t.Fatalf("create sandbox: %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/sandboxes/"+tc.sid+"/files", nil)
		req.Header.Set("X-Api-Key", "admin-key")
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s sandbox: proxy returned %d, want 200", tc.status, rr.Code)
		}

		waitFor(t, "the "+tc.status+" sandbox's write", func() bool {
			rec := mustGet(t, s, tc.sid)
			return rec.Status == "running" && rec.LastActiveAt > 1
		})
	}
}
