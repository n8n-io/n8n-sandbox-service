package api

import (
	"net/http"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/store"
)

// NewGatewayRouter creates the public API gateway that manages state and
// coordinates with registered runner services.
func NewGatewayRouter(s *store.Store, cfg *config.APIConfig, reg *registry.Registry) (http.Handler, error) {
	sandboxProxy := sandboxProxyHandler(s, cfg)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	mux.HandleFunc("GET /sandboxes", handleListSandboxes(s))
	mux.HandleFunc("POST /sandboxes", handleCreateSandbox(s, reg, cfg))
	mux.HandleFunc("GET /sandboxes/{id}", handleGetSandbox(s))
	mux.HandleFunc("DELETE /sandboxes/{id}", handleDeleteSandbox(s, cfg))

	mux.HandleFunc("POST /sandboxes/{id}/executions", sandboxProxy(false))
	mux.HandleFunc("GET /sandboxes/{id}/executions/{exec_id}", sandboxProxy(false))
	mux.HandleFunc("DELETE /sandboxes/{id}/executions/{exec_id}", sandboxProxy(false))
	mux.HandleFunc("POST /sandboxes/{id}/files/copy", sandboxProxy(false))
	mux.HandleFunc("POST /sandboxes/{id}/files/move", sandboxProxy(false))
	mux.HandleFunc("GET /sandboxes/{id}/files", sandboxProxy(false))
	mux.HandleFunc("GET /sandboxes/{id}/files/content", sandboxProxy(false))
	mux.HandleFunc("PUT /sandboxes/{id}/files", sandboxProxy(true))
	mux.HandleFunc("POST /sandboxes/{id}/files", sandboxProxy(true))
	mux.HandleFunc("DELETE /sandboxes/{id}/files", sandboxProxy(false))
	mux.HandleFunc("POST /sandboxes/{id}/mkdir", sandboxProxy(false))
	mux.HandleFunc("GET /sandboxes/{id}/stat", sandboxProxy(false))

	var handler http.Handler = mux
	handler = AuthMiddleware(cfg.APIKeys)(handler)
	handler = LoggingMiddleware(handler)
	if cfg.EnableCORS {
		handler = CORSMiddleware(handler)
	}
	handler = RecoveryMiddleware(handler)
	return handler, nil
}
