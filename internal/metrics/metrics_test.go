package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestAPIRecorderDisabled(t *testing.T) {
	r := NewAPIRecorder(false)
	if r.Enabled() {
		t.Fatalf("expected disabled")
	}
	if r.Registry() != nil {
		t.Fatalf("expected nil registry")
	}
	// All observations must be safe to call and have no effect.
	r.ObserveHTTP("/x", http.MethodGet, http.StatusOK, time.Millisecond)
	r.ObserveSandboxOp(OpCreate, true)
	r.SetActiveSandboxes(func() float64 { return 1 })
	r.SetRunnersRegistered(func() float64 { return 1 })
}

func TestAPIRecorderObservations(t *testing.T) {
	r := NewAPIRecorder(true)
	if !r.Enabled() {
		t.Fatalf("expected enabled")
	}

	r.ObserveHTTP("/sandboxes/{id}", http.MethodGet, http.StatusOK, 50*time.Millisecond)
	r.ObserveHTTP("/sandboxes/{id}", http.MethodGet, http.StatusOK, 80*time.Millisecond)
	r.ObserveSandboxOp(OpCreate, true)
	r.ObserveSandboxOp(OpCreate, false)
	r.ObserveSandboxOp(OpDelete, true)

	if got := testutil.CollectAndCount(r.httpRequests); got != 1 {
		t.Errorf("http_requests_total series = %d, want 1", got)
	}
	if got := testutil.ToFloat64(r.httpRequests.WithLabelValues("/sandboxes/{id}", http.MethodGet, "200")); got != 2 {
		t.Errorf("http_requests_total counter = %v, want 2", got)
	}
	if got := testutil.ToFloat64(r.sandboxOps.WithLabelValues(OpCreate, "success")); got != 1 {
		t.Errorf("sandbox_operations_total{create,success} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(r.sandboxOps.WithLabelValues(OpCreate, "error")); got != 1 {
		t.Errorf("sandbox_operations_total{create,error} = %v, want 1", got)
	}
}

func TestAPIRecorderScrapeIncludesExpectedFamilies(t *testing.T) {
	r := NewAPIRecorder(true)
	r.SetActiveSandboxes(func() float64 { return 3 })
	r.SetRunnersRegistered(func() float64 { return 2 })
	r.ObserveHTTP("/sandboxes", http.MethodGet, http.StatusOK, 10*time.Millisecond)
	r.ObserveSandboxOp(OpCreate, true)

	body := scrape(t, r.Registry())
	for _, want := range []string{
		"sandbox_http_requests_total",
		"sandbox_http_request_duration_seconds",
		"sandbox_sandbox_operations_total",
		"sandbox_sandboxes_active",
		"sandbox_runners_registered",
		"go_goroutines",
		"process_start_time_seconds",
		`role="api"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape body missing %q", want)
		}
	}
}

func TestRunnerRecorderObservations(t *testing.T) {
	r := NewRunnerRecorder(true)

	r.ObserveContainerOp(OpCreate, true, 2*time.Second)
	r.ObserveContainerOp(OpCreate, false, 500*time.Millisecond)
	r.ObserveContainerOp(OpEnsureRunning, true, 100*time.Millisecond)

	if got := testutil.ToFloat64(r.containerOps.WithLabelValues(OpCreate, "success")); got != 1 {
		t.Errorf("container_operations_total{create,success} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(r.containerOps.WithLabelValues(OpEnsureRunning, "success")); got != 1 {
		t.Errorf("container_operations_total{ensure_running,success} = %v, want 1", got)
	}

	body := scrape(t, r.Registry())
	for _, want := range []string{
		"sandbox_container_operations_total",
		"sandbox_container_operation_duration_seconds",
		`role="runner"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape body missing %q", want)
		}
	}
}

func TestRunnerRecorderDisabled(t *testing.T) {
	r := NewRunnerRecorder(false)
	if r.Enabled() {
		t.Fatalf("expected disabled")
	}
	r.ObserveHTTP("/x", http.MethodGet, http.StatusOK, time.Millisecond)
	r.ObserveContainerOp(OpCreate, true, time.Second)
	r.SetActiveContainers(func() float64 { return 1 })
}

func TestRouteFromPattern(t *testing.T) {
	cases := map[string]string{
		"":                                "unmatched",
		"GET /sandboxes":                  "/sandboxes",
		"POST /sandboxes/{id}/executions": "/sandboxes/{id}/executions",
		"DELETE /sandboxes/{id}":          "/sandboxes/{id}",
		"/no-method-prefix":               "/no-method-prefix",
	}
	for in, want := range cases {
		if got := routeFromPattern(in); got != want {
			t.Errorf("routeFromPattern(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHTTPMiddlewareRecordsRoutePattern(t *testing.T) {
	r := NewAPIRecorder(true)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /things/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	handler := HTTPMiddleware(r)(mux)

	req := httptest.NewRequest(http.MethodGet, "/things/abc", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusTeapot)
	}
	if got := testutil.ToFloat64(r.httpRequests.WithLabelValues("/things/{id}", http.MethodGet, "418")); got != 1 {
		t.Errorf("recorded route label = wrong; counter = %v", got)
	}
}

func TestHTTPMiddlewareRecordsPanickedRequests(t *testing.T) {
	r := NewAPIRecorder(true)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /boom", func(_ http.ResponseWriter, _ *http.Request) {
		panic("boom")
	})
	handler := HTTPMiddleware(r)(mux)

	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	rr := httptest.NewRecorder()

	defer func() {
		// Recovery is handled by an outer middleware in production; here we
		// just absorb the panic so the test process doesn't crash.
		_ = recover()
	}()

	func() {
		defer func() { _ = recover() }()
		handler.ServeHTTP(rr, req)
	}()

	// The panic unwound through HTTPMiddleware's defer, so the observation
	// must have happened. Status defaults to 200 because the handler panicked
	// before writing a header.
	if got := testutil.ToFloat64(r.httpRequests.WithLabelValues("/boom", http.MethodGet, "200")); got != 1 {
		t.Errorf("panicking handler should still record; counter = %v, want 1", got)
	}
}

func TestHTTPMiddlewareLatchesFirstStatusCode(t *testing.T) {
	r := NewAPIRecorder(true)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /double-write", func(w http.ResponseWriter, _ *http.Request) {
		// Mis-use: write 200 first, then try 500. net/http honors the first
		// on the wire (logging a warning for the second). Our metric must
		// also record the first.
		w.WriteHeader(http.StatusOK)
		w.WriteHeader(http.StatusInternalServerError)
	})
	handler := HTTPMiddleware(r)(mux)

	req := httptest.NewRequest(http.MethodGet, "/double-write", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if got := testutil.ToFloat64(r.httpRequests.WithLabelValues("/double-write", http.MethodGet, "200")); got != 1 {
		t.Errorf("first WriteHeader should be recorded; counter for 200 = %v, want 1", got)
	}
	if got := testutil.ToFloat64(r.httpRequests.WithLabelValues("/double-write", http.MethodGet, "500")); got != 0 {
		t.Errorf("subsequent WriteHeader must NOT be recorded; counter for 500 = %v, want 0", got)
	}
}

func TestHTTPMiddlewareLabelsUnmatchedRoutes(t *testing.T) {
	r := NewAPIRecorder(true)
	mux := http.NewServeMux()
	handler := HTTPMiddleware(r)(mux)

	req := httptest.NewRequest(http.MethodGet, "/nothing-here", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if got := testutil.ToFloat64(r.httpRequests.WithLabelValues("unmatched", http.MethodGet, "404")); got != 1 {
		t.Errorf("unmatched counter = %v, want 1", got)
	}
}

func scrape(t *testing.T, reg *prometheus.Registry) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	Handler(reg).ServeHTTP(rr, req)
	body, _ := io.ReadAll(rr.Body)
	return string(body)
}
