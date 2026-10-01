// Package sandboxproxy defines the response headers the runner uses to signal
// sandbox state to the API and the client across the two proxy hops.
package sandboxproxy

import "net/http"

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

// StripSignals removes every signal header from h. The runner applies it to
// daemon responses, which the guest controls.
func StripSignals(h http.Header) {
	h.Del(SandboxGoneHeader)
	h.Del(SandboxRestartedHeader)
}

// RunnerReportsSandboxGone reports whether a runner HTTP response means the sandbox
// was evicted, deleted, or is otherwise unknown to that runner. Only the header
// counts, since the runner relays daemon bodies.
func RunnerReportsSandboxGone(resp *http.Response) bool {
	return resp != nil && resp.Header.Get(SandboxGoneHeader) == "1"
}
