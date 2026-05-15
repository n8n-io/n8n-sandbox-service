package api

import (
	"log/slog"
	"net/http"
)

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
