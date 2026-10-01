package runtime

import "net/http"

// RefuseRedirect is the CheckRedirect of every runner client that talks to a
// sandbox daemon. The daemon runs inside the sandbox, so its answers are
// untrusted: following a redirect would let the guest point the runner, and on
// a 307 or 308 the request body, at anything the runner can reach. The caller
// gets the 3xx itself instead.
func RefuseRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}
