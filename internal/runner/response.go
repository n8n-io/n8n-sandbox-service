package runner

import (
	"net/http"

	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
	"github.com/n8n-io/sandbox-service/internal/sandboxproxy"
)

func writeSandboxNotFound(w http.ResponseWriter) {
	sandboxproxy.MarkSandboxGone(w.Header())
	writeError(w, http.StatusNotFound, runnerruntime.ErrSandboxNotFound.Error())
}

// writeSandboxRestarted refuses a request whose sandbox had to be recovered while
// it was in flight. The recovery has already finished, so the retry succeeds — this
// exists only so the state that was lost with the old guest is never lost silently.
//
// 409 rather than a 5xx: nothing is broken, and the client is the only one who can
// act on it. It is also deliberately not 503, which the SDK retries and which would
// swallow the signal, and not 404, which makes the API drop its store row.
//
// No Retry-After, because there is nothing to wait for. That changes the day
// recovery moves to the background rather than blocking this request.
func writeSandboxRestarted(w http.ResponseWriter) {
	sandboxproxy.MarkSandboxRestarted(w.Header())
	writeJSON(w, http.StatusConflict, map[string]string{
		"error":  "sandbox restarted after guest crash; state in memory was lost",
		"reason": "sandbox_restarted",
	})
}
