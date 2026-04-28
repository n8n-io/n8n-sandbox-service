package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/manager"
	"github.com/n8n-io/sandbox-service/internal/store"
)

type handlerTestManager struct {
	createFunc      func(context.Context, *manager.CreateOptions) (*store.SandboxRecord, error)
	getFunc         func(context.Context, string) (*store.SandboxRecord, error)
	listFunc        func(context.Context) ([]*store.SandboxRecord, error)
	deleteFunc      func(context.Context, string) error
	daemonURLFunc   func(context.Context, string) (string, error)
	getImageFunc    func(context.Context, string) (*store.ImageRecord, error)
	listImagesFunc  func(context.Context) ([]*store.ImageRecord, error)
	deleteImageFunc func(context.Context, string) error
}

func (m *handlerTestManager) Create(ctx context.Context, opts *manager.CreateOptions) (*store.SandboxRecord, error) {
	if m.createFunc != nil {
		return m.createFunc(ctx, opts)
	}
	return nil, nil
}

func (m *handlerTestManager) Get(ctx context.Context, id string) (*store.SandboxRecord, error) {
	if m.getFunc != nil {
		return m.getFunc(ctx, id)
	}
	return nil, nil
}

func (m *handlerTestManager) List(ctx context.Context) ([]*store.SandboxRecord, error) {
	if m.listFunc != nil {
		return m.listFunc(ctx)
	}
	return nil, nil
}

func (m *handlerTestManager) Delete(ctx context.Context, id string) error {
	if m.deleteFunc != nil {
		return m.deleteFunc(ctx, id)
	}
	return nil
}

func (m *handlerTestManager) DaemonURL(ctx context.Context, id string) (string, error) {
	if m.daemonURLFunc != nil {
		return m.daemonURLFunc(ctx, id)
	}
	return "", nil
}

func (m *handlerTestManager) GetImage(ctx context.Context, id string) (*store.ImageRecord, error) {
	if m.getImageFunc != nil {
		return m.getImageFunc(ctx, id)
	}
	return nil, nil
}

func (m *handlerTestManager) ListImages(ctx context.Context) ([]*store.ImageRecord, error) {
	if m.listImagesFunc != nil {
		return m.listImagesFunc(ctx)
	}
	return nil, nil
}

func (m *handlerTestManager) DeleteImage(ctx context.Context, id string) error {
	if m.deleteImageFunc != nil {
		return m.deleteImageFunc(ctx, id)
	}
	return nil
}

func TestCreateSandboxRejectsEmptyDockerfileStep(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/sandboxes", strings.NewReader(`{"dockerfile_steps":["   "]}`))
	rr := httptest.NewRecorder()

	CreateSandbox(&handlerTestManager{}).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected %d, got %d", http.StatusBadRequest, rr.Code)
	}
}

func TestCreateSandboxIncludesImageIDInResponse(t *testing.T) {
	mgr := &handlerTestManager{
		createFunc: func(_ context.Context, opts *manager.CreateOptions) (*store.SandboxRecord, error) {
			if len(opts.DockerfileSteps) != 1 || opts.DockerfileSteps[0] != "RUN apt-get update" {
				t.Fatalf("expected dockerfile_steps to be forwarded, got %v", opts.DockerfileSteps)
			}
			return &store.SandboxRecord{
				ID:           "123e4567-e89b-12d3-a456-426614174000",
				Status:       "running",
				ImageID:      "img-built",
				CreatedAt:    1,
				LastActiveAt: 2,
			}, nil
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/sandboxes", strings.NewReader(`{"dockerfile_steps":["RUN apt-get update"]}`))
	rr := httptest.NewRecorder()

	CreateSandbox(mgr).ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected %d, got %d: %s", http.StatusCreated, rr.Code, rr.Body.String())
	}

	var resp SandboxResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ImageID != "img-built" {
		t.Fatalf("expected image_id to be returned, got %q", resp.ImageID)
	}
}

func TestGetImageMapsNotFound(t *testing.T) {
	mgr := &handlerTestManager{
		getImageFunc: func(context.Context, string) (*store.ImageRecord, error) {
			return nil, manager.ErrImageNotFound
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/images/img-123", nil)
	req.SetPathValue("id", "img-123")
	rr := httptest.NewRecorder()

	GetImage(mgr).ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected %d, got %d", http.StatusNotFound, rr.Code)
	}
}

func TestDeleteImageMapsConflict(t *testing.T) {
	mgr := &handlerTestManager{
		deleteImageFunc: func(context.Context, string) error {
			return manager.ErrImageInUse
		},
	}
	req := httptest.NewRequest(http.MethodDelete, "/images/img-123", nil)
	req.SetPathValue("id", "img-123")
	rr := httptest.NewRecorder()

	DeleteImage(mgr).ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("expected %d, got %d", http.StatusConflict, rr.Code)
	}
}
