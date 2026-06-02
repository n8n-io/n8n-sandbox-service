package runner

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/runner/config"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

type proxyContextKey struct{}

type proxyTarget struct {
	url  *url.URL
	path string
}

// ProxyHandler returns a handler that reverse-proxies requests to the sandbox daemon.
func ProxyHandler(rt runnerruntime.Runtime, cfg *config.Config, rec *metrics.RunnerRecorder) http.HandlerFunc {
	return proxyHandler(rt, cfg, rec, false)
}

// UploadProxyHandler is like ProxyHandler but enforces cfg.MaxFileBytes on the request body.
func UploadProxyHandler(rt runnerruntime.Runtime, cfg *config.Config, rec *metrics.RunnerRecorder) http.HandlerFunc {
	return proxyHandler(rt, cfg, rec, true)
}

func proxyHandler(rt runnerruntime.Runtime, cfg *config.Config, rec *metrics.RunnerRecorder, limitBody bool) http.HandlerFunc {
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
		id := r.PathValue("id")
		if !isValidID(id) {
			writeError(w, http.StatusBadRequest, "invalid sandbox id")
			return
		}

		daemonBaseURL, err := rt.DaemonURL(r.Context(), id)
		if err != nil && errors.Is(err, runnerruntime.ErrSandboxNotRunning) {
			wakeStart := time.Now()
			wakeErr := rt.EnsureSandboxRunning(r.Context(), id)
			rec.ObserveContainerOp(metrics.OpEnsureRunning, wakeErr == nil, time.Since(wakeStart))
			if wakeErr != nil {
				if errors.Is(wakeErr, runnerruntime.ErrSandboxNotFound) {
					writeError(w, http.StatusNotFound, wakeErr.Error())
				} else {
					writeError(w, http.StatusServiceUnavailable, "sandbox start: "+wakeErr.Error())
				}
				return
			}
			daemonBaseURL, err = rt.DaemonURL(r.Context(), id)
		}
		if err != nil {
			if errors.Is(err, runnerruntime.ErrSandboxNotFound) {
				writeError(w, http.StatusNotFound, err.Error())
			} else if errors.Is(err, runnerruntime.ErrSandboxNotRunning) {
				writeError(w, http.StatusBadGateway, runnerruntime.ErrSandboxNotRunning.Error())
			} else if errors.Is(err, runnerruntime.ErrSandboxNetworkUnavailable) {
				writeError(w, http.StatusServiceUnavailable, "sandbox temporarily unavailable")
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
