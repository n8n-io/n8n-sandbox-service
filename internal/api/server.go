package api

import (
	"net/http"

	"github.com/n8n-io/sandbox-service/internal/config"
)

// NewRouter creates the HTTP handler with all routes registered and middleware applied.
func NewRouter(mgr SandboxManager, cfg *config.Config) http.Handler {
	mux := http.NewServeMux()

	// Health check
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// Sandbox CRUD
	mux.HandleFunc("GET /sandboxes", ListSandboxes(mgr))
	mux.HandleFunc("POST /sandboxes", CreateSandbox(mgr))
	mux.HandleFunc("GET /sandboxes/{id}", GetSandbox(mgr))
	mux.HandleFunc("DELETE /sandboxes/{id}", DeleteSandbox(mgr))

	// Image CRUD
	mux.HandleFunc("GET /images", ListImages(mgr))
	mux.HandleFunc("GET /images/{id}", GetImage(mgr))
	mux.HandleFunc("DELETE /images/{id}", DeleteImage(mgr))

	// Proxy exec, files, mkdir, stat to daemon
	proxy := ProxyHandler(mgr, cfg)
	uploadProxy := UploadProxyHandler(mgr, cfg)

	mux.HandleFunc("POST /sandboxes/{id}/exec", proxy)
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
	handler = AuthMiddleware(cfg.APIKeys)(handler)
	handler = LoggingMiddleware(handler)
	handler = CORSMiddleware(handler)
	handler = RecoveryMiddleware(handler)

	return handler
}
