// This file proxies code sandbox requests from the API to the runner.

package api

import (
	"net/http"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/codetask"
	"github.com/n8n-io/sandbox-service/internal/obs"
	"github.com/n8n-io/sandbox-service/internal/sandboxproxy"
)

// codeProxyRoute says how codeSandboxProxyHandler treats a route.
type codeProxyRoute struct {
	// runnerSuffix is the path on the runner after /sandboxes/{id}.
	runnerSuffix string
	// maxBody caps the request body.
	maxBody int64
	// withTaskID adds the {taskId} path value to runnerSuffix, after a check.
	withTaskID bool
}

// codeSandboxProxyHandler proxies code sandbox routes to the sandbox's runner.
// It has no cordon check; docs/architecture.md tells why.
func codeSandboxProxyHandler(s codeSandboxStore, cfg *config.APIConfig, transport http.RoundTripper, activity *ActivityRecorder) func(codeProxyRoute) http.HandlerFunc {
	return func(route codeProxyRoute) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("id")
			if !isValidUUID(id) {
				writeError(w, http.StatusBadRequest, "invalid sandbox id")
				return
			}
			runnerSuffix := route.runnerSuffix
			if route.withTaskID {
				taskID := r.PathValue("taskId")
				if !codetask.ValidTaskID(taskID) {
					writeError(w, http.StatusBadRequest, "invalid task id")
					return
				}
				runnerSuffix += "/" + taskID
			}

			rec, err := s.GetCodeSandbox(id)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if rec == nil || !canAccessSandbox(r, rec) || isPastIdleDeleteWindow(rec, cfg, time.Now().Unix()) {
				writeError(w, http.StatusNotFound, "sandbox not found")
				return
			}

			fields := obs.FieldsFrom(r.Context())
			fields.Add("sandbox_id", id, "runner_id", rec.RunnerID)

			u, ok := runnerTarget(w, rec)
			if !ok {
				return
			}
			// A refusal from the runner reaches the client while the client is
			// still uploading.
			early := sandboxproxy.StartEarlyReply(w, r)
			r.Body = http.MaxBytesReader(w, r.Body, route.maxBody)

			// The runner serves every kind under /sandboxes/{id}.
			out := r.WithContext(r.Context())
			runnerURL := *r.URL
			runnerURL.Path = "/sandboxes/" + id + runnerSuffix
			runnerURL.RawPath = ""
			out.URL = &runnerURL

			proxy := newRunnerReverseProxy(u, cfg.RunnerAPIKey, transport, runnerResponseHook(s, activity, rec, fields))
			modifyResponse, errorHandler := proxy.ModifyResponse, proxy.ErrorHandler
			proxy.ModifyResponse = func(resp *http.Response) error {
				if resp.StatusCode >= 300 {
					early.RefuseResponse(resp)
				}
				return modifyResponse(resp)
			}
			proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
				early.Refuse()
				errorHandler(w, r, err)
				early.StopReading()
			}
			proxy.ServeHTTP(w, out)
		}
	}
}
