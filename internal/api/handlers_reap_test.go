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
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
	"github.com/n8n-io/sandbox-service/internal/sandboxproxy"
)

func TestSandboxProxyReapsStoreOnRunnerSandboxGone(t *testing.T) {
	runner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sandboxproxy.MarkSandboxGone(w.Header())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"` + runnerruntime.ErrSandboxNotFound.Error() + `"}`))
	}))
	defer runner.Close()

	s, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer s.Close()

	sandboxID := "11111111-1111-4111-8111-111111111111"
	if err := s.Create(&store.SandboxRecord{
		ID:             sandboxID,
		Status:         "stopped",
		CreatedAt:      time.Now().Unix(),
		LastActiveAt:   time.Now().Unix(),
		RunnerHTTPBase: runner.URL,
	}); err != nil {
		t.Fatalf("Create() failed: %v", err)
	}

	router, err := NewGatewayRouter(s, &config.APIConfig{
		APIKeys:      map[string]struct{}{"public-key": {}},
		RunnerAPIKey: "runner-key",
		MaxFileBytes: 1024,
	}, registry.New(45*time.Second), metrics.NewAPIRecorder(false))
	if err != nil {
		t.Fatalf("create gateway router: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/sandboxes/"+sandboxID+"/executions", strings.NewReader(`{"command":"echo hi"}`))
	req.Header.Set("X-Api-Key", "public-key")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("exec status = %d, want 404", rr.Code)
	}

	rec, err := s.Get(sandboxID)
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}
	if rec != nil {
		t.Fatal("expected sandbox record to be removed after runner sandbox gone")
	}

	getReq := httptest.NewRequest(http.MethodGet, "/sandboxes/"+sandboxID, nil)
	getReq.Header.Set("X-Api-Key", "public-key")
	getRR := httptest.NewRecorder()
	router.ServeHTTP(getRR, getReq)
	if getRR.Code != http.StatusNotFound {
		t.Fatalf("GET status = %d, want 404 after reap", getRR.Code)
	}
}

func TestSandboxProxyKeepsStoreOnRunnerExecutionNotFound(t *testing.T) {
	runner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"execution not found"}`))
	}))
	defer runner.Close()

	s, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer s.Close()

	sandboxID := "22222222-2222-4222-8222-222222222222"
	if err := s.Create(&store.SandboxRecord{
		ID:             sandboxID,
		Status:         "running",
		CreatedAt:      time.Now().Unix(),
		LastActiveAt:   time.Now().Unix(),
		RunnerHTTPBase: runner.URL,
	}); err != nil {
		t.Fatalf("Create() failed: %v", err)
	}

	router, err := NewGatewayRouter(s, &config.APIConfig{
		APIKeys:      map[string]struct{}{"public-key": {}},
		RunnerAPIKey: "runner-key",
		MaxFileBytes: 1024,
	}, registry.New(45*time.Second), metrics.NewAPIRecorder(false))
	if err != nil {
		t.Fatalf("create gateway router: %v", err)
	}

	execID := "33333333-3333-4333-8333-333333333333"
	req := httptest.NewRequest(http.MethodGet, "/sandboxes/"+sandboxID+"/executions/"+execID, nil)
	req.Header.Set("X-Api-Key", "public-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("exec status = %d, want 404", rr.Code)
	}

	rec, err := s.Get(sandboxID)
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}
	if rec == nil {
		t.Fatal("expected sandbox record to remain after execution not found")
	}
}
