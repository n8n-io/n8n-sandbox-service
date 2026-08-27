package registry

import (
	"net/url"
	"strings"
)

// IsValidRunnerHTTPBaseURL reports whether s is an absolute https URL suitable
// for the API to dial runners (scheme + host required). Use at registration, not on each proxy request.
//
// https is required, not merely preferred: http.Transport applies
// TLSClientConfig only to https URLs, so an http base would silently downgrade
// proxied traffic to plaintext and put the runner API key on the wire.
func IsValidRunnerHTTPBaseURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" {
		return false
	}
	if u.Host == "" {
		return false
	}
	// Require absolute form "scheme://host..." so relative paths like "/runner" are rejected.
	if !strings.HasPrefix(strings.ToLower(s), scheme+"://") {
		return false
	}
	return true
}
