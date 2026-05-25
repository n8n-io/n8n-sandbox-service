package runner

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/runner/config"
	"github.com/n8n-io/sandbox-service/internal/runner/manager"
)

type fakeContainerManager struct {
	dockerErr error
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

func (f *fakeContainerManager) DaemonURL(context.Context, string) (string, error) {
	return "", nil
}

func (f *fakeContainerManager) FindContainerIDByLabel(context.Context, string) (string, error) {
	return "", manager.ErrSandboxNotFound
}

func (f *fakeContainerManager) DockerHealthy(context.Context) error {
	return f.dockerErr
}

func TestRunnerLivenessEndpointsDoNotCheckDocker(t *testing.T) {
	router := NewRouter(&fakeContainerManager{dockerErr: errors.New("docker down")}, &config.Config{})

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
	router := NewRouter(&fakeContainerManager{}, &config.Config{})
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected readyz to return 200, got %d", rec.Code)
	}
}

func TestRunnerReadyzFailsWhenDockerUnavailable(t *testing.T) {
	router := NewRouter(&fakeContainerManager{dockerErr: errors.New("docker down")}, &config.Config{})
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected readyz to return 503, got %d", rec.Code)
	}
}
