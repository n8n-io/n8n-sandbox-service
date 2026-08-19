package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/n8n-io/sandbox-service/internal/obs"
)

// LoggingMiddleware establishes the request's trace context and emits one
// canonical event per request when it finishes.
//
// It runs outermost so that rejected requests are logged too, which means the
// contexts that later middleware and handlers derive are invisible here. They
// contribute fields through the *obs.Fields pointer instead.
func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		traceparent := obs.EnsureTraceparent(r.Header.Get(obs.HeaderTraceparent))
		ctx, fields := obs.WithFields(obs.WithTraceparent(r.Context(), traceparent))
		r = r.WithContext(ctx)

		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)

		attrs := []any{
			"trace_id", obs.TraceIDOf(traceparent),
			"method", r.Method,
			"path", r.URL.Path,
			"query", r.URL.RawQuery,
			"status", sw.status,
			"duration_ms", time.Since(start).Milliseconds(),
		}
		slog.Info("request", append(attrs, fields.Attrs()...)...)
	})
}

// statusWriter wraps http.ResponseWriter to capture the status code for logging.
// It defaults to 200 so that handlers that call Write without WriteHeader are recorded correctly.
type statusWriter struct {
	http.ResponseWriter
	status int
}

// WriteHeader captures the status code before delegating to the wrapped ResponseWriter.
func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}

// Unwrap returns the underlying ResponseWriter so that middleware-aware helpers
// (e.g. http.ResponseController) can reach the original writer.
func (sw *statusWriter) Unwrap() http.ResponseWriter {
	return sw.ResponseWriter
}

// Flush implements http.Flusher by delegating to the wrapped ResponseWriter if it supports flushing.
func (sw *statusWriter) Flush() {
	if f, ok := sw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
