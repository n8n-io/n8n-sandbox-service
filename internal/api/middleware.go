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

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}

func (sw *statusWriter) Unwrap() http.ResponseWriter {
	return sw.ResponseWriter
}

// Implement http.Flusher for streaming support
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

// RecoveryMiddleware catches panics and returns 500.
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

type recoveryResponseWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (rw *recoveryResponseWriter) WriteHeader(code int) {
	if rw.wroteHeader {
		return
	}
	rw.wroteHeader = true
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *recoveryResponseWriter) Write(data []byte) (int, error) {
	if !rw.wroteHeader {
		rw.wroteHeader = true
	}
	return rw.ResponseWriter.Write(data)
}

func (rw *recoveryResponseWriter) Flush() {
	if !rw.wroteHeader {
		rw.wroteHeader = true
	}
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (rw *recoveryResponseWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}
