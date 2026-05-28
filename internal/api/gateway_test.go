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

func TestGatewayHandlesSandboxList(t *testing.T) {
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer s.Close()

	router, err := NewGatewayRouter(s, &config.APIConfig{
		APIKeys:      map[string]struct{}{"public-key": {}},
		RunnerAPIKey: "runner-key",
		MaxFileBytes: 1024,
	}, registry.New(45*time.Second), metrics.NewAPIRecorder(false))
	if err != nil {
		t.Fatalf("create gateway router: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/sandboxes", nil)
	req.Header.Set("X-Api-Key", "public-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d", http.StatusOK, rr.Code)
	}

	// Should return empty array for new database
	expected := `[]`
	if strings.TrimSpace(rr.Body.String()) != expected {
		t.Fatalf("expected %s, got %s", expected, rr.Body.String())
	}
}

func TestGatewayRejectsMissingPublicAPIKey(t *testing.T) {
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer s.Close()

	router, err := NewGatewayRouter(s, &config.APIConfig{
		APIKeys:      map[string]struct{}{"public-key": {}},
		RunnerAPIKey: "runner-key",
		MaxFileBytes: 1024,
	}, registry.New(45*time.Second), metrics.NewAPIRecorder(false))
	if err != nil {
		t.Fatalf("create gateway router: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/sandboxes", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected %d, got %d", http.StatusUnauthorized, rr.Code)
	}
}

func TestGatewayMetricsEndpointEnabledBypassesAuth(t *testing.T) {
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer s.Close()

	router, err := NewGatewayRouter(s, &config.APIConfig{
		APIKeys:      map[string]struct{}{"public-key": {}},
		MaxFileBytes: 1024,
	}, registry.New(45*time.Second), metrics.NewAPIRecorder(true))
	if err != nil {
		t.Fatalf("create gateway router: %v", err)
	}

	// Warm-up: an authenticated request so HTTPMiddleware records a series.
	// Scrapes only emit families that have at least one observed series.
	warm := httptest.NewRequest(http.MethodGet, "/sandboxes", nil)
	warm.Header.Set("X-Api-Key", "public-key")
	router.ServeHTTP(httptest.NewRecorder(), warm)

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

func TestGatewayMetricsEndpointDisabledReturns404(t *testing.T) {
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer s.Close()

	router, err := NewGatewayRouter(s, &config.APIConfig{
		APIKeys:      map[string]struct{}{"public-key": {}},
		MaxFileBytes: 1024,
	}, registry.New(45*time.Second), metrics.NewAPIRecorder(false))
	if err != nil {
		t.Fatalf("create gateway router: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("X-Api-Key", "public-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected %d, got %d", http.StatusNotFound, rr.Code)
	}
}
