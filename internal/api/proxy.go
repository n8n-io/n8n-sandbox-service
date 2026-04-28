package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/n8n-io/sandbox-service/internal/config"
	"github.com/n8n-io/sandbox-service/internal/manager"
)

type proxyContextKey struct{}

type proxyTarget struct {
	url  *url.URL
	path string
}

// ProxyHandler returns a handler that reverse-proxies requests to the sandbox
// daemon, stripping the /sandboxes/{id} prefix from the path.
func ProxyHandler(mgr SandboxManager, cfg *config.Config) http.HandlerFunc {
	return proxyHandler(mgr, cfg, false)
}

// UploadProxyHandler is like ProxyHandler but enforces cfg.MaxFileBytes on the
// request body before proxying.
func UploadProxyHandler(mgr SandboxManager, cfg *config.Config) http.HandlerFunc {
	return proxyHandler(mgr, cfg, true)
}

func proxyHandler(mgr SandboxManager, cfg *config.Config, limitBody bool) http.HandlerFunc {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pt := pr.In.Context().Value(proxyContextKey{}).(*proxyTarget)
			pr.SetURL(pt.url)
			pr.Out.URL.Path = pt.path
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
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
			writeError(w, http.StatusBadGateway, "daemon unreachable")
		},
	}

	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !isValidID(id) {
			writeError(w, http.StatusBadRequest, "invalid sandbox id")
			return
		}

		daemonBaseURL, err := mgr.DaemonURL(r.Context(), id)
		if err != nil {
			if errors.Is(err, manager.ErrSandboxNotFound) {
				writeError(w, http.StatusNotFound, err.Error())
			} else {
				writeError(w, http.StatusInternalServerError, err.Error())
			}
			return
		}

		target, err := url.Parse(daemonBaseURL)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("invalid daemon url: %v", err))
			return
		}

		if limitBody {
			r.Body = http.MaxBytesReader(w, r.Body, cfg.MaxFileBytes)
		}

		// Strip /sandboxes/{id} prefix to get the daemon path.
		prefix := "/sandboxes/" + id
		daemonPath := strings.TrimPrefix(r.URL.Path, prefix)
		if daemonPath == "" {
			daemonPath = "/"
		}

		ctx := context.WithValue(r.Context(), proxyContextKey{}, &proxyTarget{
			url:  target,
			path: daemonPath,
		})
		proxy.ServeHTTP(w, r.WithContext(ctx))
	}
}
