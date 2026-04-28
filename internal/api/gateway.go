package api

import (
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/n8n-io/sandbox-service/internal/config"
)

// NewGatewayRouter creates the public API gateway that authenticates requests
// and forwards sandbox/image routes to the runner service.
func NewGatewayRouter(cfg *config.APIConfig) (http.Handler, error) {
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

	proxyHandler := func(limitBody bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if limitBody {
				r.Body = http.MaxBytesReader(w, r.Body, cfg.MaxFileBytes)
			}
			proxy.ServeHTTP(w, r)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// Sandbox CRUD
	mux.HandleFunc("GET /sandboxes", proxyHandler(false))
	mux.HandleFunc("POST /sandboxes", proxyHandler(false))
	mux.HandleFunc("GET /sandboxes/{id}", proxyHandler(false))
	mux.HandleFunc("DELETE /sandboxes/{id}", proxyHandler(false))

	// Image CRUD
	mux.HandleFunc("GET /images", proxyHandler(false))
	mux.HandleFunc("GET /images/{id}", proxyHandler(false))
	mux.HandleFunc("DELETE /images/{id}", proxyHandler(false))

	// Sandbox command/file endpoints proxied to runner.
	mux.HandleFunc("POST /sandboxes/{id}/exec", proxyHandler(false))
	mux.HandleFunc("POST /sandboxes/{id}/files/copy", proxyHandler(false))
	mux.HandleFunc("POST /sandboxes/{id}/files/move", proxyHandler(false))
	mux.HandleFunc("GET /sandboxes/{id}/files", proxyHandler(false))
	mux.HandleFunc("GET /sandboxes/{id}/files/content", proxyHandler(false))
	mux.HandleFunc("PUT /sandboxes/{id}/files", proxyHandler(true))
	mux.HandleFunc("POST /sandboxes/{id}/files", proxyHandler(true))
	mux.HandleFunc("DELETE /sandboxes/{id}/files", proxyHandler(false))
	mux.HandleFunc("POST /sandboxes/{id}/mkdir", proxyHandler(false))
	mux.HandleFunc("GET /sandboxes/{id}/stat", proxyHandler(false))

	var handler http.Handler = mux
	handler = AuthMiddleware(cfg.APIKeys)(handler)
	handler = LoggingMiddleware(handler)
	handler = CORSMiddleware(handler)
	handler = RecoveryMiddleware(handler)
	return handler, nil
}
