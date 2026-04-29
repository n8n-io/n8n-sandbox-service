package api

import (
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/store"
)

// NewGatewayRouter creates the public API gateway that manages state and
// coordinates with the runner service.
func NewGatewayRouter(s *store.Store, cfg *config.APIConfig) (http.Handler, error) {
	runnerURL, err := url.Parse(cfg.RunnerURL)
	if err != nil {
		return nil, err
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(runnerURL)
			pr.Out.URL.Path = pr.In.URL.Path
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Host = runnerURL.Host
			if cfg.RunnerAPIKey != "" {
				pr.Out.Header.Set("X-Api-Key", cfg.RunnerAPIKey)
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
			writeError(w, http.StatusBadGateway, "runner unreachable")
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)

	mux.HandleFunc("GET /sandboxes", handleListSandboxes(s))
	mux.HandleFunc("POST /sandboxes", handleCreateSandbox(s, cfg, runnerURL, cfg.RunnerAPIKey))
	mux.HandleFunc("GET /sandboxes/{id}", handleGetSandbox(s))
	mux.HandleFunc("DELETE /sandboxes/{id}", handleDeleteSandbox(s, runnerURL, cfg.RunnerAPIKey))

	mux.HandleFunc("GET /images", newRunnerProxyHandler(cfg, proxy, false))
	mux.HandleFunc("GET /images/{id}", handleGetImage(proxy))
	mux.HandleFunc("DELETE /images/{id}", handleDeleteImage(proxy))

	mux.HandleFunc("POST /sandboxes/{id}/exec", newSandboxProxyHandler(s, cfg, proxy, false))
	mux.HandleFunc("POST /sandboxes/{id}/files/copy", newSandboxProxyHandler(s, cfg, proxy, false))
	mux.HandleFunc("POST /sandboxes/{id}/files/move", newSandboxProxyHandler(s, cfg, proxy, false))
	mux.HandleFunc("GET /sandboxes/{id}/files", newSandboxProxyHandler(s, cfg, proxy, false))
	mux.HandleFunc("GET /sandboxes/{id}/files/content", newSandboxProxyHandler(s, cfg, proxy, false))
	mux.HandleFunc("PUT /sandboxes/{id}/files", newSandboxProxyHandler(s, cfg, proxy, true))
	mux.HandleFunc("POST /sandboxes/{id}/files", newSandboxProxyHandler(s, cfg, proxy, true))
	mux.HandleFunc("DELETE /sandboxes/{id}/files", newSandboxProxyHandler(s, cfg, proxy, false))
	mux.HandleFunc("POST /sandboxes/{id}/mkdir", newSandboxProxyHandler(s, cfg, proxy, false))
	mux.HandleFunc("GET /sandboxes/{id}/stat", newSandboxProxyHandler(s, cfg, proxy, false))

	var handler http.Handler = mux
	handler = AuthMiddleware(cfg.APIKeys)(handler)
	handler = LoggingMiddleware(handler)
	handler = CORSMiddleware(handler)
	handler = RecoveryMiddleware(handler)
	return handler, nil
}
