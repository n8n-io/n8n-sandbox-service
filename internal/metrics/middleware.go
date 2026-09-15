package metrics

import (
	"net/http"
	"strings"
	"time"
)

// HTTPObserver is the slice of the recorder API that the HTTP middleware needs.
// Both *APIRecorder and *RunnerRecorder satisfy it.
type HTTPObserver interface {
	ObserveHTTP(route, method string, status int, dur time.Duration)
}

// HTTPMiddleware wraps next, recording each request's route pattern, method,
// status, and duration to obs.
//
// The route label is http.Request.Pattern, which ServeMux sets on the request
// while routing, so it is read after next.ServeHTTP returns. The method prefix
// is stripped; unmatched requests are labelled "unmatched" to bound cardinality.
// Observation is deferred so panicking handlers are recorded too.
func HTTPMiddleware(obs HTTPObserver) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			start := time.Now()
			defer func() {
				obs.ObserveHTTP(routeFromPattern(r.Pattern), r.Method, sw.status, time.Since(start))
			}()
			next.ServeHTTP(sw, r)
		})
	}
}

// routeFromPattern strips the "METHOD " prefix from a ServeMux pattern.
func routeFromPattern(pattern string) string {
	if pattern == "" {
		return "unmatched"
	}
	if i := strings.Index(pattern, " "); i > 0 {
		return pattern[i+1:]
	}
	return pattern
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

// WriteHeader latches the first status code seen. Per the http.ResponseWriter
// contract, only the first WriteHeader call is honored on the wire; subsequent
// calls are logged by net/http and ignored. Recording the first call keeps the
// metric aligned with what the client actually receives.
func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
