package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/config"
	"github.com/n8n-io/sandbox-service/internal/manager"
	"github.com/n8n-io/sandbox-service/internal/store"
)

const testSandboxID = "123e4567-e89b-12d3-a456-426614174000"

type stubSandboxManager struct {
	daemonURL string
}

func (m *stubSandboxManager) Create(context.Context, *manager.CreateOptions) (*store.SandboxRecord, error) {
	return nil, nil
}

func (m *stubSandboxManager) Get(context.Context, string) (*store.SandboxRecord, error) {
	return nil, nil
}

func (m *stubSandboxManager) List(context.Context) ([]*store.SandboxRecord, error) {
	return nil, nil
}

func (m *stubSandboxManager) Delete(context.Context, string) error {
	return nil
}

func (m *stubSandboxManager) DaemonURL(context.Context, string) (string, error) {
	return m.daemonURL, nil
}

func (m *stubSandboxManager) GetImage(context.Context, string) (*store.ImageRecord, error) {
	return nil, nil
}

func (m *stubSandboxManager) ListImages(context.Context) ([]*store.ImageRecord, error) {
	return nil, nil
}

func (m *stubSandboxManager) DeleteImage(context.Context, string) error {
	return nil
}

func TestUploadProxyReturnsBadRequestForLargeBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	mgr := &stubSandboxManager{daemonURL: upstream.URL}
	cfg := &config.Config{
		APIKeys:      map[string]struct{}{"test-key": {}},
		MaxFileBytes: 4,
	}
	router := NewRouter(mgr, cfg)

	req := httptest.NewRequest(http.MethodPut, "/sandboxes/"+testSandboxID+"/files?path=/out.txt", strings.NewReader("12345"))
	req.Header.Set("X-Api-Key", "test-key")
	req.Header.Set("Content-Type", "application/octet-stream")

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected %d, got %d: %s", http.StatusBadRequest, rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "request body too large") {
		t.Fatalf("expected body-too-large error, got: %s", rr.Body.String())
	}
}
