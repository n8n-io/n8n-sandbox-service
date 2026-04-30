package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/store"
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
	}, registry.New(45*time.Second))
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
	}, registry.New(45*time.Second))
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

func TestCreateSandboxRejectsOversizedJSONBody(t *testing.T) {
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer s.Close()

	const maxBody = 64
	router, err := NewGatewayRouter(s, &config.APIConfig{
		APIKeys:      map[string]struct{}{"public-key": {}},
		RunnerAPIKey: "runner-key",
		MaxFileBytes: maxBody,
	}, registry.New(45*time.Second))
	if err != nil {
		t.Fatalf("create gateway router: %v", err)
	}

	body := bytes.Repeat([]byte("a"), maxBody+1)
	req := httptest.NewRequest(http.MethodPost, "/sandboxes", bytes.NewReader(body))
	req.Header.Set("X-Api-Key", "public-key")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected %d, got %d body %q", http.StatusBadRequest, rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "failed to read request body") && !strings.Contains(rr.Body.String(), "invalid JSON") {
		t.Fatalf("expected oversized body rejection, got %q", rr.Body.String())
	}
}
