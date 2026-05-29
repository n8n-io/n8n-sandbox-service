package runner

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/n8n-io/sandbox-service/internal/runner/config"
	"github.com/n8n-io/sandbox-service/internal/runner/manager"
)

type proxyContextKey struct{}

type proxyTarget struct {
	url  *url.URL
	path string
}

// ProxyHandler returns a handler that reverse-proxies requests to the sandbox daemon.
func ProxyHandler(mgr ContainerManager, cfg *config.Config) http.HandlerFunc {
	return proxyHandler(mgr, cfg, false)
}

// UploadProxyHandler is like ProxyHandler but enforces cfg.MaxFileBytes on the request body.
func UploadProxyHandler(mgr ContainerManager, cfg *config.Config) http.HandlerFunc {
	return proxyHandler(mgr, cfg, true)
}

func proxyHandler(mgr ContainerManager, cfg *config.Config, limitBody bool) http.HandlerFunc {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// Comma-ok assertion: the context key is missing when
			// httputil.ReverseProxy replays Rewrite on a internally-constructed
			// request (e.g. 100-continue handshake, connection-level retry after
			// an idle-connection reset). Bail out so the request fails at the
			// transport layer and is handled by ErrorHandler instead of panicking.
			pt, ok := pr.In.Context().Value(proxyContextKey{}).(*proxyTarget)
			if !ok || pt == nil {
				return
			}
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
			writeError(w, http.StatusServiceUnavailable, "daemon temporarily unavailable")
		},
	}

	return func(w http.ResponseWriter, r *http.Request) {
		daemonBaseURL, ok := resolveDaemonURL(w, r, mgr)
		if !ok {
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

		id := r.PathValue("id")
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

// resolveDaemonURL validates the sandbox ID, looks up the daemon URL, and
// wakes the sandbox if necessary. On error it writes an HTTP response and
// returns ("", false).
func resolveDaemonURL(w http.ResponseWriter, r *http.Request, mgr ContainerManager) (string, bool) {
	id := r.PathValue("id")
	if !isValidID(id) {
		writeError(w, http.StatusBadRequest, "invalid sandbox id")
		return "", false
	}

	daemonBaseURL, err := mgr.DaemonURL(r.Context(), id)
	if err != nil && errors.Is(err, manager.ErrSandboxNotRunning) {
		if wakeErr := mgr.EnsureSandboxRunning(r.Context(), id); wakeErr != nil {
			if errors.Is(wakeErr, manager.ErrSandboxNotFound) {
				writeError(w, http.StatusNotFound, wakeErr.Error())
			} else {
				writeError(w, http.StatusServiceUnavailable, "sandbox start: "+wakeErr.Error())
			}
			return "", false
		}
		daemonBaseURL, err = mgr.DaemonURL(r.Context(), id)
	}
	if err != nil {
		if errors.Is(err, manager.ErrSandboxNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
		} else if errors.Is(err, manager.ErrSandboxNotRunning) {
			writeError(w, http.StatusBadGateway, manager.ErrSandboxNotRunning.Error())
		} else if errors.Is(err, manager.ErrSandboxNetworkUnavailable) {
			writeError(w, http.StatusServiceUnavailable, "sandbox temporarily unavailable")
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return "", false
	}

	return daemonBaseURL, true
}
