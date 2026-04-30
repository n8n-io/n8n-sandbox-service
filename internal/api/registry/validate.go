package registry

import (
	"net/url"
	"strings"
)

// isValidRunnerHTTPBaseURL returns true if s is an absolute http(s) URL suitable
// for the API to dial runners (scheme + host required).
func isValidRunnerHTTPBaseURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
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
