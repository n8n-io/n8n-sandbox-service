// Package probes holds the health and metrics paths that the API and the runner
// serve without auth or an access-log line.
package probes

import "net/http"

// Paths is a set of such paths. Each one is served on a GET route, and
// ServeMux also serves HEAD on a GET route.
type Paths map[string]struct{}

// New returns a Paths holding paths.
func New(paths ...string) Paths {
	p := make(Paths, len(paths))
	for _, path := range paths {
		p[path] = struct{}{}
	}
	return p
}

// Public reports whether r skips auth and the access log.
func (p Paths) Public(r *http.Request) bool {
	_, ok := p[r.URL.Path]
	return ok && isGetOrHead(r.Method)
}

// RejectOtherMethods answers any other method on p with the same 405 ServeMux
// sends. Placed between the logging and auth middleware, the 405 is logged and
// never reaches the metrics middleware, so the method cannot add a series.
func (p Paths) RejectOtherMethods(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := p[r.URL.Path]; ok && !isGetOrHead(r.Method) {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isGetOrHead(method string) bool {
	return method == http.MethodGet || method == http.MethodHead
}
