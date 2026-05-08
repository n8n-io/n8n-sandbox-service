package runner

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

// AuthMiddleware checks for valid API keys.
func AuthMiddleware(apiKeys map[string]struct{}) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

// LoggingMiddleware logs HTTP requests.
func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		next.ServeHTTP(w, r)
		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"duration", time.Since(start),
		)
	})
}

// RecoveryMiddleware recovers from panics.
func RecoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &recoveryResponseWriter{ResponseWriter: w}
		defer func() {
			if err := recover(); err != nil {
				if err == http.ErrAbortHandler {
					panic(err)
				}
				slog.Error("panic recovered", "error", err, "stack", string(debug.Stack()))
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
