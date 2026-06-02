package runner

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/runner/config"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

type fakeRuntime struct {
	readyErr error
}

func (f *fakeRuntime) Prepare(context.Context) {}

func (f *fakeRuntime) Ready(context.Context) error {
	return f.readyErr
}

func (f *fakeRuntime) ReadyCh() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func (f *fakeRuntime) Capacity(context.Context) (runnerruntime.Capacity, error) {
	return runnerruntime.Capacity{}, nil
}

func (f *fakeRuntime) CreateSandbox(context.Context, string, *runnerruntime.CreateOptions) (*runnerruntime.SandboxInfo, error) {
	return nil, nil
}

func (f *fakeRuntime) GetSandboxInfo(context.Context, string) (*runnerruntime.SandboxInfo, error) {
	return nil, nil
}

func (f *fakeRuntime) DeleteSandbox(context.Context, string) error {
	return nil
}

func (f *fakeRuntime) StopSandbox(context.Context, string) error {
	return nil
}

func (f *fakeRuntime) EnsureSandboxRunning(context.Context, string) error {
	return nil
}

func (f *fakeRuntime) DaemonURL(context.Context, string) (string, error) {
	return "", nil
}

func (f *fakeRuntime) Shutdown(context.Context) {}

func TestRunnerLivenessEndpointsDoNotCheckRuntime(t *testing.T) {
	router := NewRouter(&fakeRuntime{readyErr: errors.New("runtime down")}, &config.Config{}, metrics.NewRunnerRecorder(false))

	for _, path := range []string{"/healthz", "/livez"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()

			router.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected %s to return 200, got %d", path, rec.Code)
			}
		})
	}
}

func TestRunnerReadyzChecksRuntime(t *testing.T) {
	router := NewRouter(&fakeRuntime{}, &config.Config{}, metrics.NewRunnerRecorder(false))
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected readyz to return 200, got %d", rec.Code)
	}
}

func TestRunnerReadyzFailsWhenRuntimeUnavailable(t *testing.T) {
	router := NewRouter(&fakeRuntime{readyErr: errors.New("runtime down")}, &config.Config{}, metrics.NewRunnerRecorder(false))
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected readyz to return 503, got %d", rec.Code)
	}
}

func TestRunnerReadyzFailsWhenRuntimeNotReady(t *testing.T) {
	router := NewRouter(&fakeRuntime{readyErr: errors.New("runtime not ready")}, &config.Config{}, metrics.NewRunnerRecorder(false))
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected readyz to return 503 when runtime not ready, got %d", rec.Code)
	}
}

func TestRunnerMetricsEndpointEnabledBypassesAuth(t *testing.T) {
	cfg := &config.Config{APIKeys: map[string]struct{}{"k": {}}}
	router := NewRouter(&fakeRuntime{}, cfg, metrics.NewRunnerRecorder(true))

	// Warm-up: a request the mux can dispatch so HTTPMiddleware records a series.
	// Scrapes only emit families that have at least one observed series.
	// /healthz is allowlisted from auth, so no API key needed.
	warm := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	router.ServeHTTP(httptest.NewRecorder(), warm)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"sandbox_http_requests_total",
		`role="runner"`,
		`route="/healthz"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics body missing %q", want)
		}
	}
}

func TestRunnerMetricsEndpointDisabledReturns404(t *testing.T) {
	cfg := &config.Config{APIKeys: map[string]struct{}{"k": {}}}
	router := NewRouter(&fakeRuntime{}, cfg, metrics.NewRunnerRecorder(false))

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("X-Api-Key", "k")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}
