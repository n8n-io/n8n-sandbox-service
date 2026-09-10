package api

import (
	"net/http"

	"github.com/n8n-io/sandbox-service/internal/metrics"
)

// NewMetricsRouter returns the handler for the dedicated Prometheus listener,
// used when SANDBOX_API_METRICS_LISTEN_ADDR names a port other than the API's.
// It serves GET /metrics and nothing else: no auth (there is nothing else on the
// port to protect), no access log, no CORS, and no HTTP metrics of its own,
// since a scrape is not API traffic. RecoveryMiddleware wraps it so a panic in
// the exposition path becomes a logged 500 like everywhere else.
//
// A disabled recorder has a nil registry, which promhttp panics on at
// construction, so it yields a 404-only handler.
func NewMetricsRouter(rec *metrics.APIRecorder) http.Handler {
	mux := http.NewServeMux()
	if rec.Enabled() {
		mux.Handle("GET /metrics", metrics.Handler(rec.Registry()))
	}
	return RecoveryMiddleware(mux)
}
