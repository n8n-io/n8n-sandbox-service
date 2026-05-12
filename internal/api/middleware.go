package api

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"time"
)

// AuthMiddleware returns middleware that checks X-Api-Key against allowed keys.
// /healthz is always allowed through.
func AuthMiddleware(allowedKeys map[string]struct{}) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/healthz" {
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

// CORSMiddleware allows all origins.
func CORSMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Api-Key")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// RecoveryMiddleware catches panics from downstream handlers and returns a 500 error.
// It re-panics with http.ErrAbortHandler in two cases: when the original panic is
// ErrAbortHandler (preserving the stdlib's "silently abort" semantics), or when the
// response has already started writing (since the status line is already on the wire
// and a clean error response is no longer possible).
func RecoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &recoveryResponseWriter{ResponseWriter: w}
		defer func() {
			if err := recover(); err != nil {
				if err == http.ErrAbortHandler {
					panic(err)
				}
				slog.Error("panic recovered", "err", err, "path", r.URL.Path)
				if rw.wroteHeader {
					panic(http.ErrAbortHandler)
				}
				writeError(rw, http.StatusInternalServerError, "internal server error")
			}
		}()
		next.ServeHTTP(rw, r)
	})
}

// recoveryResponseWriter wraps http.ResponseWriter to track whether headers have
// already been sent. RecoveryMiddleware uses this to decide whether it can still
// write a clean 500 error response after a panic.
type recoveryResponseWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

// WriteHeader records that headers have been sent and delegates to the wrapped writer.
// Subsequent calls are no-ops to prevent the superfluous WriteHeader log from net/http.
func (rw *recoveryResponseWriter) WriteHeader(code int) {
	if rw.wroteHeader {
		return
	}
	rw.wroteHeader = true
	rw.ResponseWriter.WriteHeader(code)
}

// Write marks headers as sent (Go implicitly sends a 200 on first Write) and delegates.
func (rw *recoveryResponseWriter) Write(data []byte) (int, error) {
	if !rw.wroteHeader {
		rw.wroteHeader = true
	}
	return rw.ResponseWriter.Write(data)
}

// Flush marks headers as sent (flushing implicitly commits the response header)
// and delegates to the wrapped writer if it implements http.Flusher.
func (rw *recoveryResponseWriter) Flush() {
	if !rw.wroteHeader {
		rw.wroteHeader = true
	}
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap returns the underlying ResponseWriter for middleware-aware helpers.
func (rw *recoveryResponseWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}
