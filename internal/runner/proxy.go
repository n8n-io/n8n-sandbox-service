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
	return proxyHandler(rt, cfg, rec, false, true)
}

// UploadProxyHandler is like ProxyHandler but enforces cfg.MaxFileBytes on the request body.
func UploadProxyHandler(rt runnerruntime.Runtime, cfg *config.Config, rec *metrics.RunnerRecorder) http.HandlerFunc {
	return proxyHandler(rt, cfg, rec, true, true)
}

// DeleteExecutionHandler serves DELETE /sandboxes/{id}/executions/{exec_id}, the
// one sandbox route that answers without waking.
//
// An execution lives only in the guest's memory, so a sandbox that is stopped, or
// that came back from a crash, has already lost the one this names and the delete
// has nothing left to do. It gets 204.
//
// Not waking is what keeps the crash report honest. A recovery reports the restart
// to the single request that wakes the sandbox, and this delete is not a client
// asking to use it: the SDK sends one in the background after every command and
// discards the answer. Letting it spend the report would hide the crash from the
// request that comes next.
func DeleteExecutionHandler(rt runnerruntime.Runtime, cfg *config.Config, rec *metrics.RunnerRecorder) http.HandlerFunc {
	return proxyHandler(rt, cfg, rec, false, false)
}

// wake says whether a sandbox that is not running is started for the request. Only
// DeleteExecutionHandler passes false; see there for why.
func proxyHandler(rt runnerruntime.Runtime, cfg *config.Config, rec *metrics.RunnerRecorder, limitBody bool, wake bool) http.HandlerFunc {
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
		daemonBaseURL, ok := resolveDaemonURL(w, r, rt, rec, wake)
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

		// Strip /sandboxes/{id} prefix to get the daemon path.
		id := r.PathValue("id")
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
//
// wake=false is for a route that must not start a stopped sandbox. It answers the
// request here rather than leaving the caller to check first and proxy after: the
// lookup below is a docker ps and inspect, so a caller that decided on its own
// lookup and then reached a handler that repeats it leaves a window a crash fits
// inside — one wide enough to have failed in CI. One lookup, one decision.
func resolveDaemonURL(w http.ResponseWriter, r *http.Request, rt runnerruntime.Runtime, rec *metrics.RunnerRecorder, wake bool) (string, bool) {
	id := r.PathValue("id")
	if !isValidID(id) {
		writeError(w, http.StatusBadRequest, "invalid sandbox id")
		return "", false
	}

	daemonBaseURL, err := rt.DaemonURL(r.Context(), id)
	if err != nil && errors.Is(err, runnerruntime.ErrSandboxNotRunning) {
		if !wake {
			writeExecutionGone(w)
			return "", false
		}
		wakeStart := time.Now()
		wake, wakeErr := rt.EnsureSandboxRunning(r.Context(), id)
		if rec != nil {
			// Under the operation the wake actually was, so a cold boot's latency does
			// not land in the wake percentiles and the totals agree with the per-step
			// timings the runtime records under the same label.
			op := metrics.OpEnsureRunning
			if wake.Recovered {
				op = metrics.OpRecover
			}
			rec.ObserveContainerOp(op, wakeErr == nil, time.Since(wakeStart))
		}
		if wakeErr != nil {
			if errors.Is(wakeErr, runnerruntime.ErrSandboxNotFound) {
				writeSandboxNotFound(w)
			} else {
				writeError(w, http.StatusServiceUnavailable, "sandbox start: "+wakeErr.Error())
			}
			return "", false
		}
		// Recover first, then refuse. The sandbox is up by the time this runs, so the
		// client's retry succeeds deterministically; proxying instead would hand back a
		// healthy-looking sandbox whose processes are gone and leave the client to
		// discover that as a bug in its own code. Every request coalesced into this one
		// recovery lands here, so a burst after a crash tells all of them, once.
		if wake.Recovered {
			writeSandboxRestarted(w)
			return "", false
		}
		daemonBaseURL, err = rt.DaemonURL(r.Context(), id)
	}
	if err != nil {
		if errors.Is(err, runnerruntime.ErrSandboxNotFound) {
			writeSandboxNotFound(w)
		} else if errors.Is(err, runnerruntime.ErrSandboxNotRunning) {
			writeError(w, http.StatusBadGateway, runnerruntime.ErrSandboxNotRunning.Error())
		} else if errors.Is(err, runnerruntime.ErrSandboxNetworkUnavailable) {
			writeError(w, http.StatusServiceUnavailable, "sandbox temporarily unavailable")
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return "", false
	}

	return daemonBaseURL, true
}
