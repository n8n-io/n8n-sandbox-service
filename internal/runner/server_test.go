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
	"github.com/n8n-io/sandbox-service/internal/runner/manager"
)

type fakeContainerManager struct {
	dockerErr  error
	imageReady bool
	daemonURL  string
	daemonErr  error
}

func (f *fakeContainerManager) CreateContainer(context.Context, string, *manager.CreateOptions) (*manager.ContainerInfo, error) {
	return nil, nil
}

func (f *fakeContainerManager) GetContainerInfo(context.Context, string) (*manager.ContainerInfo, error) {
	return nil, nil
}

func (f *fakeContainerManager) DeleteContainer(context.Context, string) error {
	return nil
}

func (f *fakeContainerManager) EnsureSandboxRunning(context.Context, string) error {
	return nil
}

func (f *fakeContainerManager) DaemonURL(_ context.Context, _ string) (string, error) {
	if f.daemonErr != nil {
		return "", f.daemonErr
	}
	return f.daemonURL, nil
}

func (f *fakeContainerManager) FindContainerIDByLabel(context.Context, string) (string, error) {
	return "", manager.ErrSandboxNotFound
}

func (f *fakeContainerManager) DockerHealthy(context.Context) error {
	return f.dockerErr
}

func (f *fakeContainerManager) ImageReady() bool {
	return f.imageReady
}

func TestRunnerLivenessEndpointsDoNotCheckDocker(t *testing.T) {
	router := NewRouter(&fakeContainerManager{dockerErr: errors.New("docker down")}, &config.Config{}, metrics.NewRunnerRecorder(false))

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

func TestRunnerReadyzChecksDocker(t *testing.T) {
	router := NewRouter(&fakeContainerManager{imageReady: true}, &config.Config{}, metrics.NewRunnerRecorder(false))
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected readyz to return 200, got %d", rec.Code)
	}
}

func TestRunnerReadyzFailsWhenDockerUnavailable(t *testing.T) {
	router := NewRouter(&fakeContainerManager{dockerErr: errors.New("docker down"), imageReady: true}, &config.Config{}, metrics.NewRunnerRecorder(false))
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected readyz to return 503, got %d", rec.Code)
	}
}

func TestRunnerReadyzFailsWhenImageNotReady(t *testing.T) {
	router := NewRouter(&fakeContainerManager{imageReady: false}, &config.Config{}, metrics.NewRunnerRecorder(false))
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected readyz to return 503 when image not ready, got %d", rec.Code)
	}
}

func TestRunnerMetricsEndpointEnabledBypassesAuth(t *testing.T) {
	cfg := &config.Config{APIKeys: map[string]struct{}{"k": {}}}
	router := NewRouter(&fakeContainerManager{imageReady: true}, cfg, metrics.NewRunnerRecorder(true))

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
	router := NewRouter(&fakeContainerManager{}, cfg, metrics.NewRunnerRecorder(false))

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("X-Api-Key", "k")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}
