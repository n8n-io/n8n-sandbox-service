package runner

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/n8n-io/sandbox-service/internal/obs"
)

// LoggingMiddleware establishes the request's trace context from the header the
// API forwards, so lifecycle events emitted deeper in the runner carry the same
// trace id, and logs one line per request.
func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz", "/livez", "/readyz", "/metrics":
			next.ServeHTTP(w, r)
			return
		}
		traceparent := obs.EnsureTraceparent(r.Header.Get(obs.HeaderTraceparent))
		r = r.WithContext(obs.WithTraceparent(r.Context(), traceparent))

		start := time.Now()
		next.ServeHTTP(w, r)
		slog.Info("request",
			"trace_id", obs.TraceIDOf(traceparent),
			"method", r.Method,
			"path", r.URL.Path,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}
