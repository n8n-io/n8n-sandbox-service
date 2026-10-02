package runner

import (
	"net/http"

	"github.com/n8n-io/sandbox-service/internal/limits"
	"github.com/n8n-io/sandbox-service/internal/metrics"
)

// limitRetryAfter is the Retry-After sent with every limit response. A slot
// frees as soon as any request in progress finishes, so there is no point in
// asking a client to wait longer.
const limitRetryAfter = "1"

// maxExecutionDeletesPerSandbox caps a sandbox's execution DELETEs in
// progress. They are counted apart from its other requests, so work can still
// be cancelled when the sandbox's limit is full. Bounded, because a DELETE to a
// stalled daemon holds its slot for up to deleteExecutionTimeout.
const maxExecutionDeletesPerSandbox = 4

// sandboxLimit wraps a sandbox route in a per-sandbox cap held in l. It is
// exact across API replicas, since every request for a sandbox lands on the
// runner hosting it. The slot is taken before the handler can wake a stopped
// sandbox, so a refused request never costs a wake.
func sandboxLimit(l *limits.KeyedLimiter, rec *metrics.RunnerRecorder) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("id")
			if !isValidID(id) {
				writeError(w, http.StatusBadRequest, "invalid sandbox id")
				return
			}
			if !l.TryAcquire(id) {
				rec.ObserveRequestLimited(metrics.LimitSandbox)
				writeSandboxRequestLimit(w)
				return
			}
			defer l.Release(id)
			next(w, r)
		}
	}
}

// writeSandboxRequestLimit refuses a request over the per-sandbox cap. The API
// passes the body through untouched, so, like writeSandboxRestarted's, it
// carries a reason rather than the API's integer code.
func writeSandboxRequestLimit(w http.ResponseWriter) {
	w.Header().Set("Retry-After", limitRetryAfter)
	writeJSON(w, http.StatusTooManyRequests, map[string]string{
		"error":  "too many requests in progress for this sandbox",
		"reason": "sandbox_request_limit",
	})
}
