package runner

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/runner/config"
	"github.com/n8n-io/sandbox-service/internal/runner/manager"
)

// ContainerManager defines the interface for container operations.
type ContainerManager interface {
	CreateContainer(ctx context.Context, sandboxID string, opts *manager.CreateOptions) (*manager.ContainerInfo, error)
	GetContainerInfo(ctx context.Context, containerID string) (*manager.ContainerInfo, error)
	DeleteContainer(ctx context.Context, containerID string) error
	EnsureSandboxRunning(ctx context.Context, sandboxID string) error
	DaemonURL(ctx context.Context, containerID string) (string, error)
	FindContainerIDByLabel(ctx context.Context, sandboxID string) (string, error)
}

// NewRouter creates the HTTP handler for container operations. If rec is
// enabled, its /metrics handler is mounted and HTTPMiddleware wraps the chain.
func NewRouter(mgr ContainerManager, cfg *config.Config, rec *metrics.RunnerRecorder) http.Handler {
	mux := http.NewServeMux()

	// Health check
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	if rec.Enabled() {
		mux.Handle("GET /metrics", metrics.Handler(rec.Registry()))
	}

	mux.HandleFunc("GET /sandboxes/{id}", GetSandbox(mgr))

	// Proxy exec, files, mkdir, stat to daemon
	proxy := ProxyHandler(mgr, cfg, rec)
	uploadProxy := UploadProxyHandler(mgr, cfg, rec)

	mux.HandleFunc("POST /sandboxes/{id}/executions", proxy)
	mux.HandleFunc("GET /sandboxes/{id}/executions/{exec_id}", proxy)
	mux.HandleFunc("DELETE /sandboxes/{id}/executions/{exec_id}", proxy)
	mux.HandleFunc("POST /sandboxes/{id}/files/copy", proxy)
	mux.HandleFunc("POST /sandboxes/{id}/files/move", proxy)
	mux.HandleFunc("GET /sandboxes/{id}/files", proxy)
	mux.HandleFunc("GET /sandboxes/{id}/files/content", proxy)
	mux.HandleFunc("PUT /sandboxes/{id}/files", uploadProxy)
	mux.HandleFunc("POST /sandboxes/{id}/files", uploadProxy)
	mux.HandleFunc("DELETE /sandboxes/{id}/files", proxy)
	mux.HandleFunc("POST /sandboxes/{id}/mkdir", proxy)
	mux.HandleFunc("GET /sandboxes/{id}/stat", proxy)

	// Apply middleware (outermost first)
	var handler http.Handler = mux
	if rec.Enabled() {
		handler = metrics.HTTPMiddleware(rec)(handler)
	}
	handler = AuthMiddleware(cfg.APIKeys)(handler)
	handler = LoggingMiddleware(handler)
	handler = RecoveryMiddleware(handler)

	return handler
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
