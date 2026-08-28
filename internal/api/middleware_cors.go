package api

import (
	"net/http"

	"github.com/n8n-io/sandbox-service/internal/sandboxproxy"
)

// CORSMiddleware allows all origins.
func CORSMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Api-Key")
		// Response headers are hidden from browser JavaScript unless exposed, and a
		// client that cannot read this one cannot tell a restarted sandbox from any
		// other 409.
		w.Header().Set("Access-Control-Expose-Headers", sandboxproxy.SandboxRestartedHeader)

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}
