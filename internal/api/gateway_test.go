package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/config"
)

func TestGatewayForwardsWithRunnerAPIKey(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer upstream.Close()

	router, err := NewGatewayRouter(&config.APIConfig{
		APIKeys:      map[string]struct{}{"public-key": {}},
		RunnerURL:    upstream.URL,
		RunnerAPIKey: "runner-key",
		MaxFileBytes: 1024,
	})
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
	if gotAuth != "runner-key" {
		t.Fatalf("expected upstream X-Api-Key to be runner key, got %q", gotAuth)
	}
}

func TestGatewayRejectsMissingPublicAPIKey(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	router, err := NewGatewayRouter(&config.APIConfig{
		APIKeys:      map[string]struct{}{"public-key": {}},
		RunnerURL:    upstream.URL,
		RunnerAPIKey: "runner-key",
		MaxFileBytes: 1024,
	})
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
