package api

import (
	"log/slog"
	"net/http"
	"time"
)

// LoggingMiddleware logs request method, path, status, and duration.
func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"query", r.URL.RawQuery,
			"status", sw.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
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
