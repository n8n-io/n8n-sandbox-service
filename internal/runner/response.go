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
