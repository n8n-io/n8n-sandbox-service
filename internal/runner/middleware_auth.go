package runner

import "net/http"

// AuthMiddleware checks for valid API keys.
// Health endpoints are always allowed through.
func AuthMiddleware(apiKeys map[string]struct{}) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/healthz" || r.URL.Path == "/livez" || r.URL.Path == "/readyz" {
				next.ServeHTTP(w, r)
				return
			}

			apiKey := r.Header.Get("X-Api-Key")
			if apiKey == "" {
				writeError(w, http.StatusUnauthorized, "missing API key")
				return
			}
			if _, ok := apiKeys[apiKey]; !ok {
				writeError(w, http.StatusUnauthorized, "invalid API key")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
