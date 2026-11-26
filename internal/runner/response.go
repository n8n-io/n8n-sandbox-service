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
// No Retry-After, because there is nothing to wait for.
func writeSandboxRestarted(w http.ResponseWriter) {
	sandboxproxy.MarkSandboxRestarted(w.Header())
	writeJSON(w, http.StatusConflict, map[string]string{
		"error":  "sandbox restarted after guest crash; state in memory was lost",
		"reason": "sandbox_restarted",
	})
}

// writeExecutionGone answers a delete whose sandbox is not running. The execution it
// names was in the guest's memory, which a stop or a crash already took, so the
// delete has its outcome and 204 reports it — the same status the daemon gives for
// deleting an execution it does not have.
func writeExecutionGone(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

// writeTaskNotRunning answers a task delete whose sandbox is not running. A stop
// or a crash already ended the task, so there is nothing to cancel. This is
// the daemon's answer for a task it does not run.
func writeTaskNotRunning(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]string{"code": "not_found", "message": "task not running"})
}
