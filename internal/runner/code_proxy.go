package runner

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
	"github.com/n8n-io/sandbox-service/internal/sandboxproxy"
)

// CodeTaskMaxBodyBytes bounds a task request: 1 GiB of input binaries plus the
// other inputs.
const CodeTaskMaxBodyBytes int64 = 2200 * 1024 * 1024

// registerCodeRoutes adds the Task API routes. They proxy to the code daemon of
// a code sandbox. They never wake a sandbox: code sandboxes have no stop.
func registerCodeRoutes(mux *http.ServeMux, rt runnerruntime.Runtime) {
	proxy := codeProxyHandler(rt, 0)
	mux.HandleFunc("GET /code/sandboxes/{id}/healthz", proxy)
	mux.HandleFunc("GET /code/sandboxes/{id}/state", proxy)
	mux.HandleFunc("GET /code/sandboxes/{id}/channel", codeProxy(rt, 0, true))
	mux.HandleFunc("POST /code/sandboxes/{id}/tasks", codeProxyHandler(rt, CodeTaskMaxBodyBytes))
	mux.HandleFunc("GET /code/sandboxes/{id}/tasks/{taskId}/outputs/{path...}", proxy)
	mux.HandleFunc("DELETE /code/sandboxes/{id}/tasks/{taskId}", proxy)
}

func codeProxyHandler(rt runnerruntime.Runtime, maxBody int64) http.HandlerFunc {
	return codeProxy(rt, maxBody, maxBody > 0)
}

// codeProxy proxies to the daemon. longLived clears the read deadline.
func codeProxy(rt runnerruntime.Runtime, maxBody int64, longLived bool) http.HandlerFunc {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pt, ok := pr.In.Context().Value(proxyContextKey{}).(*proxyTarget)
			if !ok || pt == nil {
				return
			}
			pr.SetURL(pt.url)
			pr.Out.URL.Path = pt.path
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			// The code daemon needs no credentials, and the sandbox must not see
			// the runner key or other client credentials.
			for _, h := range []string{"X-Api-Key", "Authorization", "Proxy-Authorization", "Cookie"} {
				pr.Out.Header.Del(h)
			}
		},
		// Only the runner may say that a sandbox is gone or restarted. The API
		// acts on these headers, so a daemon (user code) must not set them.
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del(sandboxproxy.SandboxGoneHeader)
			resp.Header.Del(sandboxproxy.SandboxRestartedHeader)
			return nil
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
				return
			}
			daemonUnreachable(rt, r, "code proxy", err)
			writeError(w, http.StatusBadGateway, "code daemon unavailable")
		},
	}

	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !isValidID(id) {
			writeError(w, http.StatusBadRequest, "invalid sandbox id")
			return
		}
		if longLived {
			// The server ReadTimeout would cut long uploads and the channel.
			_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
		}

		base, err := rt.DaemonURL(r.Context(), id)
		if err != nil {
			switch {
			case errors.Is(err, runnerruntime.ErrSandboxNotFound):
				writeSandboxNotFound(w)
			case errors.Is(err, runnerruntime.ErrSandboxNotRunning), errors.Is(err, runnerruntime.ErrSandboxNetworkUnavailable):
				writeError(w, http.StatusBadGateway, "code sandbox is not running")
			default:
				slog.WarnContext(r.Context(), "code proxy: daemon url", "sandbox_id", id, "err", err)
				writeError(w, http.StatusInternalServerError, err.Error())
			}
			return
		}
		target, err := url.Parse(base)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "invalid daemon url")
			return
		}
		if maxBody > 0 {
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		}
		daemonPath := strings.TrimPrefix(r.URL.Path, "/code/sandboxes/"+id)
		if daemonPath == "" {
			daemonPath = "/"
		}
		ctx := context.WithValue(r.Context(), proxyContextKey{}, &proxyTarget{url: target, path: daemonPath})
		proxy.ServeHTTP(w, r.WithContext(ctx))
	}
}
