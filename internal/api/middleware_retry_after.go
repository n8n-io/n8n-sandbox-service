package api

import "net/http"

// RetryAfterMiddleware gives every 429 and 503 a Retry-After, so a client can
// treat all of them alike: back off and repeat the same request. The API's
// own refusals set it already; this covers the ones relayed from runners and
// daemons, which do not.
func RetryAfterMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&retryAfterWriter{ResponseWriter: w}, r)
	})
}

type retryAfterWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (rw *retryAfterWriter) WriteHeader(code int) {
	if !rw.wroteHeader {
		rw.wroteHeader = true
		if (code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable) && rw.Header().Get("Retry-After") == "" {
			rw.Header().Set("Retry-After", "1")
		}
	}
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *retryAfterWriter) Write(b []byte) (int, error) {
	if !rw.wroteHeader {
		rw.WriteHeader(http.StatusOK)
	}
	return rw.ResponseWriter.Write(b)
}

// Flush keeps execution streams streaming.
func (rw *retryAfterWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (rw *retryAfterWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}
