package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/metrics"
)

// dedicatedMetricsSetup wires one recorder into both routers the way cmd/api
// does when SANDBOX_API_METRICS_LISTEN_ADDR names its own port, and drives one
// authenticated API request through the gateway so the registry has a series:
// scrapes only emit families that have at least one observed series.
func dedicatedMetricsSetup(t *testing.T) http.Handler {
	t.Helper()

	s, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	rec := metrics.NewAPIRecorder(true)
	gateway, err := NewGatewayRouter(s, &config.APIConfig{
		APIKeys:           map[string]struct{}{"public-key": {}},
		MaxFileBytes:      1024,
		ListenAddr:        ":8080",
		MetricsListenAddr: ":9100",
	}, registry.New(45*time.Second), rec)
	if err != nil {
		t.Fatalf("create gateway router: %v", err)
	}

	warm := httptest.NewRequest(http.MethodGet, "/sandboxes", nil)
	warm.Header.Set("X-Api-Key", "public-key")
	gateway.ServeHTTP(httptest.NewRecorder(), warm)

	return NewMetricsRouter(rec)
}

func TestMetricsRouterServesRegistryWithoutAPIKey(t *testing.T) {
	router := dedicatedMetricsSetup(t)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d (body: %s)", http.StatusOK, rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		"sandbox_http_requests_total",
		`role="api"`,
		`route="/sandboxes"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics body missing %q", want)
		}
	}
}

func TestMetricsRouterRejectsAPIRoutes(t *testing.T) {
	router := dedicatedMetricsSetup(t)

	for _, path := range []string{"/sandboxes", "/healthz"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("X-Api-Key", "public-key")
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)

			if rr.Code != http.StatusNotFound {
				t.Fatalf("expected %d, got %d", http.StatusNotFound, rr.Code)
			}
		})
	}
}

func TestMetricsRouterDoesNotRecordScrapes(t *testing.T) {
	router := dedicatedMetricsSetup(t)

	// A scrape is not API traffic, so nothing on this listener observes it.
	var body string
	for i := 0; i < 2; i++ {
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		body = rr.Body.String()
	}

	if strings.Contains(body, `route="/metrics"`) {
		t.Errorf("dedicated metrics listener recorded its own scrape:\n%s", body)
	}
}

func TestMetricsRouterWithDisabledRecorderReturns404(t *testing.T) {
	router := NewMetricsRouter(metrics.NewAPIRecorder(false))

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected %d, got %d", http.StatusNotFound, rr.Code)
	}
}
