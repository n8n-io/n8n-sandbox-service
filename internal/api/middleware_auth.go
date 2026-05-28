package api

import (
	"crypto/subtle"
	"net/http"
)

// AuthMiddleware returns middleware that checks X-Api-Key against allowed keys.
// /healthz and /metrics are always allowed through; /metrics is only mounted
// when SANDBOX_API_METRICS_ENABLED, and operators are expected to firewall it.
func AuthMiddleware(allowedKeys map[string]struct{}) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
				next.ServeHTTP(w, r)
				return
			}

			key := r.Header.Get("X-Api-Key")
			if key == "" {
				writeError(w, http.StatusUnauthorized, "missing X-Api-Key header")
				return
			}

			if !constantTimeContains(allowedKeys, key) {
				writeError(w, http.StatusUnauthorized, "invalid API key")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// constantTimeContains checks if key exists in the allowed set using constant-time comparison.
func constantTimeContains(allowed map[string]struct{}, key string) bool {
	for k := range allowed {
		if subtle.ConstantTimeCompare([]byte(k), []byte(key)) == 1 {
			return true
		}
	}
	return false
}
