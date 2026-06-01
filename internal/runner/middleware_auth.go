package runner

import "net/http"

// AuthMiddleware checks for valid API keys.
// Health endpoints are always allowed through; /metrics is also unauthenticated
// when it's mounted (operators are expected to firewall the port).
func AuthMiddleware(apiKeys map[string]struct{}) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/healthz", "/livez", "/readyz", "/metrics":
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
