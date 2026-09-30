package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
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

// A public path with another method is refused before auth and metrics run, so
// the caller-chosen method is logged but never becomes a series.
func TestGatewayRejectsOtherMethodsOnPublicPaths(t *testing.T) {
	logs := captureLogs(t)
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

	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/healthz", nil),
		httptest.NewRequest("FOO", "/metrics", nil),
	} {
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: status = %d, want %d", req.Method, req.URL.Path, rr.Code, http.StatusMethodNotAllowed)
		}
		if got := rr.Header().Get("Allow"); got != "GET, HEAD" {
			t.Errorf("%s %s: Allow = %q, want %q", req.Method, req.URL.Path, got, "GET, HEAD")
		}
	}

	head := httptest.NewRecorder()
	router.ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/healthz", nil))
	if head.Code != http.StatusOK {
		t.Errorf("HEAD /healthz: status = %d, want %d", head.Code, http.StatusOK)
	}

	var logged []string
	for _, e := range logs() {
		logged = append(logged, fmt.Sprintf("%v %v %v", e["method"], e["path"], e["status"]))
	}
	if want := []string{"POST /healthz 405", "FOO /metrics 405"}; !slices.Equal(logged, want) {
		t.Errorf("logged requests = %q, want %q", logged, want)
	}

	scrape := httptest.NewRecorder()
	router.ServeHTTP(scrape, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := scrape.Body.String()
	if !strings.Contains(body, `route="/healthz",status="200"`) {
		t.Errorf("metrics body missing the HEAD /healthz series:\n%s", body)
	}
	for _, unwanted := range []string{`route="unmatched"`, `method="FOO"`, `method="other"`, `method="POST"`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("metrics body contains %q", unwanted)
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

func TestGatewayOmitsMetricsWhenOnDedicatedListener(t *testing.T) {
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer s.Close()

	router, err := NewGatewayRouter(s, &config.APIConfig{
		APIKeys:           map[string]struct{}{"public-key": {}},
		MaxFileBytes:      1024,
		ListenAddr:        ":8080",
		MetricsListenAddr: ":9100",
	}, registry.New(45*time.Second), metrics.NewAPIRecorder(true))
	if err != nil {
		t.Fatalf("create gateway router: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("X-Api-Key", "public-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("/metrics: expected %d, got %d", http.StatusNotFound, rr.Code)
	}

	// The rest of the gateway is untouched.
	list := httptest.NewRequest(http.MethodGet, "/sandboxes", nil)
	list.Header.Set("X-Api-Key", "public-key")
	listRR := httptest.NewRecorder()
	router.ServeHTTP(listRR, list)

	if listRR.Code != http.StatusOK {
		t.Fatalf("/sandboxes: expected %d, got %d", http.StatusOK, listRR.Code)
	}
}
