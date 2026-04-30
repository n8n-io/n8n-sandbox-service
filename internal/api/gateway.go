package api

import (
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/store"
)

// NewGatewayRouter creates the public API gateway that manages state and
// coordinates with registered runner services.
func NewGatewayRouter(s *store.Store, cfg *config.APIConfig, reg *registry.Registry) (http.Handler, error) {
	sandboxProxy := sandboxProxyHandler(s, cfg)
	imageProxy := imageProxyHandler(reg, cfg)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	mux.HandleFunc("GET /sandboxes", handleListSandboxes(s))
	mux.HandleFunc("POST /sandboxes", handleCreateSandbox(s, reg, cfg.RunnerAPIKey))
	mux.HandleFunc("GET /sandboxes/{id}", handleGetSandbox(s))
	mux.HandleFunc("DELETE /sandboxes/{id}", handleDeleteSandbox(s, cfg.RunnerAPIKey))

	mux.HandleFunc("GET /images", imageProxy(false))
	mux.HandleFunc("GET /images/{id}", handleGetImage(reg, cfg))
	mux.HandleFunc("DELETE /images/{id}", handleDeleteImage(reg, cfg))

	mux.HandleFunc("POST /sandboxes/{id}/exec", sandboxProxy(false))
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
	handler = CORSMiddleware(handler)
	handler = RecoveryMiddleware(handler)
	return handler, nil
}

func newRunnerReverseProxy(runnerURL *url.URL, runnerAPIKey string, cfg *config.APIConfig) *httputil.ReverseProxy {
	target := *runnerURL
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&target)
			pr.Out.URL.Path = pr.In.URL.Path
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Host = target.Host
			if runnerAPIKey != "" {
				pr.Out.Header.Set("X-Api-Key", runnerAPIKey)
			} else {
				pr.Out.Header.Del("X-Api-Key")
			}
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				writeError(w, http.StatusBadRequest, "failed to read request body: "+maxBytesErr.Error())
				return
			}
			if strings.Contains(err.Error(), "request body too large") {
				writeError(w, http.StatusBadRequest, "failed to read request body: http: request body too large")
				return
			}
			writeError(w, http.StatusServiceUnavailable, "runner unavailable")
		},
	}
}
