package runner

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/limits"
	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/runner/config"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

const otherTestSandboxID = "660e8400-e29b-41d4-a716-446655440001"

// blockingDaemon stands in for a sandbox daemon whose every response is a
// stream that stays open until the test ends or the caller goes away.
type blockingDaemon struct {
	*httptest.Server
	hits    atomic.Int64
	release chan struct{}
}

func newBlockingDaemon(t *testing.T) *blockingDaemon {
	t.Helper()
	d := &blockingDaemon{release: make(chan struct{})}
	d.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.hits.Add(1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(startedEvent("exec-1") + "\n"))
		w.(http.Flusher).Flush()
		select {
		case <-d.release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(d.Close)
	// Runs before Close, which would otherwise wait on handlers still blocked.
	t.Cleanup(func() { close(d.release) })
	return d
}

func (d *blockingDaemon) waitForHits(t *testing.T, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for d.hits.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("daemon hits = %d, want %d", d.hits.Load(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type limitTestRunner struct {
	router http.Handler
	daemon *blockingDaemon
	rec    *metrics.RunnerRecorder
}

func newLimitTestRunner(t *testing.T, tune func(*config.Config)) *limitTestRunner {
	t.Helper()
	d := newBlockingDaemon(t)
	cfg := &config.Config{APIKeys: map[string]struct{}{"k": {}}, MaxFileBytes: 1 << 20}
	tune(cfg)
	rec := metrics.NewRunnerRecorder(true)
	return &limitTestRunner{router: NewRouter(&fakeRuntime{daemonURL: d.URL}, cfg, rec), daemon: d, rec: rec}
}

// authedRequest passes AuthMiddleware the way the API does: with a client
// certificate and the runner API key.
func authedRequest(ctx context.Context, method, path, body string) *http.Request {
	req := httptest.NewRequestWithContext(ctx, method, path, strings.NewReader(body))
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{}}}
	req.Header.Set("X-Api-Key", "k")
	return req
}

func (lr *limitTestRunner) call(method, path, body string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	lr.router.ServeHTTP(rr, authedRequest(context.Background(), method, path, body))
	return rr
}

// hold starts n requests that stay open in the daemon until the test ends or
// the returned func is called, and returns once the daemon has seen them all.
func (lr *limitTestRunner) hold(t *testing.T, n int, method, path, body string) (release func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	before := lr.daemon.hits.Load()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lr.router.ServeHTTP(httptest.NewRecorder(), authedRequest(ctx, method, path, body))
		}()
	}
	lr.daemon.waitForHits(t, before+int64(n))
	stop := func() {
		cancel()
		wg.Wait()
	}
	t.Cleanup(stop)
	return stop
}

func TestSandboxLimitRefusesRequestsOverCap(t *testing.T) {
	lr := newLimitTestRunner(t, func(c *config.Config) { c.MaxInflightPerSandbox = 2 })
	sandbox := "/sandboxes/" + proxyTestSandboxID

	lr.hold(t, 1, http.MethodPost, sandbox+"/executions", `{"command":"sleep 60"}`)
	lr.hold(t, 1, http.MethodGet, sandbox+"/files/content?path=/tmp/big", "")

	for _, route := range []struct{ method, path, body string }{
		{http.MethodPost, "/executions", `{"command":"true"}`},
		{http.MethodGet, "/executions/exec-1?follow=true", ""},
		{http.MethodPut, "/files?path=/tmp/x", "data"},
		{http.MethodGet, "/stat?path=/tmp", ""},
	} {
		rr := lr.call(route.method, sandbox+route.path, route.body)
		if rr.Code != http.StatusTooManyRequests {
			t.Fatalf("%s %s: status = %d, want 429: %s", route.method, route.path, rr.Code, rr.Body.String())
		}
		if got := rr.Header().Get("Retry-After"); got != "1" {
			t.Errorf("Retry-After = %q, want 1", got)
		}
		var body struct{ Error, Reason string }
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode body %q: %v", rr.Body.String(), err)
		}
		if body.Reason != "sandbox_request_limit" || body.Error == "" {
			t.Errorf("body = %+v, want an error and reason sandbox_request_limit", body)
		}
	}
	if hits := lr.daemon.hits.Load(); hits != 2 {
		t.Errorf("daemon hits = %d, want 2: a refused request must not reach the daemon", hits)
	}
	if got := lr.rec.RequestsLimitedCount(metrics.LimitSandbox); got != 4 {
		t.Errorf("sandbox limited count = %v, want 4", got)
	}

	// The cap is per sandbox.
	release := lr.hold(t, 1, http.MethodGet, "/sandboxes/"+otherTestSandboxID+"/files", "")
	release()
}

// Execution DELETE is how work in a sandbox is stopped, so it is counted apart
// from the sandbox's other requests: it gets through while they fill their
// limit, and takes none of their slots. Its own limit is bounded.
func TestSandboxLimitCountsExecutionDeletesApart(t *testing.T) {
	lr := newLimitTestRunner(t, func(c *config.Config) { c.MaxInflightPerSandbox = 1 })
	sandbox := "/sandboxes/" + proxyTestSandboxID

	releaseExec := lr.hold(t, 1, http.MethodPost, sandbox+"/executions", `{"command":"sleep 60"}`)
	lr.hold(t, maxExecutionDeletesPerSandbox, http.MethodDelete, sandbox+"/executions/exec-1", "")

	if rr := lr.call(http.MethodDelete, sandbox+"/executions/exec-1", ""); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("DELETE past its own limit: status = %d, want 429: %s", rr.Code, rr.Body.String())
	}

	releaseExec()
	lr.hold(t, 1, http.MethodPost, sandbox+"/executions", `{"command":"sleep 60"}`)
}

func TestSandboxLimitOffLeavesExecutionDeletesUnlimited(t *testing.T) {
	lr := newLimitTestRunner(t, func(c *config.Config) { c.MaxInflightPerSandbox = 0 })

	lr.hold(t, maxExecutionDeletesPerSandbox+1, http.MethodDelete, "/sandboxes/"+proxyTestSandboxID+"/executions/exec-1", "")
}

// A guest can starve its own daemon, and a DELETE waiting on it holds a slot
// in the runner and the API, so the wait is bounded.
func TestDeleteExecutionGivesUpOnAStalledDaemon(t *testing.T) {
	old := deleteExecutionTimeout
	deleteExecutionTimeout = 50 * time.Millisecond
	t.Cleanup(func() { deleteExecutionTimeout = old })

	// A starved daemon never gets as far as sending headers.
	release := make(chan struct{})
	daemon := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(daemon.Close)
	t.Cleanup(func() { close(release) })
	handler := DeleteExecutionHandler(&fakeRuntime{daemonURL: daemon.URL}, &config.Config{}, metrics.NewRunnerRecorder(false))
	req := httptest.NewRequest(http.MethodDelete, "/sandboxes/"+proxyTestSandboxID+"/executions/exec-1", nil)
	req.SetPathValue("id", proxyTestSandboxID)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		done <- rr
	}()
	select {
	case rr := <-done:
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503: %s", rr.Code, rr.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DELETE to a stalled daemon did not give up")
	}
}

func TestSandboxLimitFreesSlotWhenRequestEnds(t *testing.T) {
	lr := newLimitTestRunner(t, func(c *config.Config) { c.MaxInflightPerSandbox = 1 })
	sandbox := "/sandboxes/" + proxyTestSandboxID

	release := lr.hold(t, 1, http.MethodPost, sandbox+"/executions", `{"command":"sleep 60"}`)
	if rr := lr.call(http.MethodGet, sandbox+"/stat?path=/tmp", ""); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status with the slot held = %d, want 429", rr.Code)
	}
	release()
	lr.hold(t, 1, http.MethodGet, sandbox+"/stat?path=/tmp", "")
}

// The slot is taken before the wrapped handler runs, and that handler is what
// wakes a stopped sandbox, so a refused request never costs a wake.
func TestSandboxLimitRefusesBeforeTheHandlerRuns(t *testing.T) {
	l := limits.NewKeyedLimiter(1)
	if !l.TryAcquire(proxyTestSandboxID) {
		t.Fatal("setup: could not take the only slot")
	}
	var calls atomic.Int32
	handler := sandboxLimit(l, metrics.NewRunnerRecorder(false))(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	})

	rr := httptest.NewRecorder()
	handler(rr, proxyTestRequest())

	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rr.Code)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("handler calls = %d, want 0", got)
	}
}

// timeout_ms is the only bound on how long an execution, which outlives its
// stream, can run. The check runs before the sandbox is resolved, so a request
// refused for it never wakes a stopped sandbox.
func TestExecProxyRejectsTimeoutAboveMaxWithoutWaking(t *testing.T) {
	rt := &fakeRuntime{daemonErr: runnerruntime.ErrSandboxNotRunning}
	handler := ExecProxyHandler(rt, &config.Config{MaxExecTimeout: time.Hour}, metrics.NewRunnerRecorder(false))

	req := httptest.NewRequest(http.MethodPost, "/sandboxes/"+proxyTestSandboxID+"/executions",
		strings.NewReader(`{"command":"sleep 1","timeout_ms":3600001}`))
	req.SetPathValue("id", proxyTestSandboxID)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "3600000") {
		t.Errorf("body = %s, want it to name the maximum", rr.Body.String())
	}
	if got := rt.ensureCalls.Load(); got != 0 {
		t.Errorf("wakes = %d, want 0", got)
	}
}

func TestExecProxyAcceptsTimeoutAtMax(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(startedEvent("exec-1") + "\n" + exitEvent(1) + "\n"))
	}))
	defer daemon.Close()
	handler := ExecProxyHandler(&fakeRuntime{daemonURL: daemon.URL}, &config.Config{MaxExecTimeout: time.Hour}, metrics.NewRunnerRecorder(false))

	req := httptest.NewRequest(http.MethodPost, "/sandboxes/"+proxyTestSandboxID+"/executions",
		strings.NewReader(`{"command":"true","timeout_ms":3600000}`))
	req.SetPathValue("id", proxyTestSandboxID)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
}
