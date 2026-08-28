package sandboxproxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// SandboxGoneHeader is set by the runner when a sandbox ID is no longer tracked.
const SandboxGoneHeader = "X-Sandbox-Gone"

// MarkSandboxGone sets the response header that tells the API to drop its store row.
func MarkSandboxGone(h http.Header) {
	h.Set(SandboxGoneHeader, "1")
}

// SandboxRestartedHeader is set by the runner on the request it refuses because
// the sandbox had to be restarted under it. It is the channel that survives both
// proxy hops unambiguously: the runner's error bodies and the API's do not have
// the same shape, and a proxied response passes the runner's body through
// untouched, so a client cannot rely on one JSON field appearing everywhere.
const SandboxRestartedHeader = "X-Sandbox-Restarted"

// MarkSandboxRestarted sets the response header that tells the client its sandbox
// came back without the state it had.
func MarkSandboxRestarted(h http.Header) {
	h.Set(SandboxRestartedHeader, "1")
}

// RunnerReportsSandboxGone reports whether a runner HTTP response means the sandbox
// was evicted, deleted, or is otherwise unknown to that runner.
func RunnerReportsSandboxGone(resp *http.Response) bool {
	if resp == nil {
		return false
	}
	if resp.Header.Get(SandboxGoneHeader) == "1" {
		return true
	}
	if resp.StatusCode != http.StatusNotFound {
		return false
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		return false
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))

	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return false
	}
	return payload.Error == runnerruntime.ErrSandboxNotFound.Error()
}
