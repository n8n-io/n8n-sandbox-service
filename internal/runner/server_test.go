package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/runner/config"
	"github.com/n8n-io/sandbox-service/internal/runner/manager"
)

// stubContainerManager satisfies ContainerManager for routing tests; methods
// panic if called, so tests that exercise sandbox routes must replace them.
type stubContainerManager struct{}

func (stubContainerManager) CreateContainer(context.Context, string, *manager.CreateOptions) (*manager.ContainerInfo, error) {
	panic("not implemented")
}
func (stubContainerManager) GetContainerInfo(context.Context, string) (*manager.ContainerInfo, error) {
	panic("not implemented")
}
func (stubContainerManager) DeleteContainer(context.Context, string) error {
	panic("not implemented")
}
func (stubContainerManager) EnsureSandboxRunning(context.Context, string) error {
	panic("not implemented")
}
func (stubContainerManager) DaemonURL(context.Context, string) (string, error) {
	panic("not implemented")
}
func (stubContainerManager) FindContainerIDByLabel(context.Context, string) (string, error) {
	panic("not implemented")
}

func TestRouterMetricsEndpointEnabledBypassesAuth(t *testing.T) {
	cfg := &config.Config{APIKeys: map[string]struct{}{"k": {}}}
	router := NewRouter(stubContainerManager{}, cfg, metrics.NewRunnerRecorder(true))

	// Warm-up: an authenticated request that the mux can dispatch, so
	// HTTPMiddleware records a series. Scrapes only emit families that have
	// at least one observed series. /healthz needs an API key on the runner.
	warm := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	warm.Header.Set("X-Api-Key", "k")
	router.ServeHTTP(httptest.NewRecorder(), warm)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rr.Code, http.StatusOK, rr.Body.String())
	}
	body := rr.Body.String()
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

func TestRouterMetricsEndpointDisabledReturns404(t *testing.T) {
	cfg := &config.Config{APIKeys: map[string]struct{}{"k": {}}}
	router := NewRouter(stubContainerManager{}, cfg, metrics.NewRunnerRecorder(false))

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("X-Api-Key", "k")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusNotFound)
	}
}
