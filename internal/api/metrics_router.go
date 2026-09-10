package api

import (
	"net/http"

	"github.com/n8n-io/sandbox-service/internal/metrics"
)

// NewMetricsRouter returns the handler for the dedicated Prometheus listener,
// used when SANDBOX_API_METRICS_LISTEN_ADDR names a port other than the API's.
// Deliberately no auth, access log, CORS or HTTP metrics: there is nothing else
// on the port to protect, and a scrape is not API traffic.
//
// The Enabled guard is load-bearing: a disabled recorder has a nil registry,
// which promhttp panics on at construction.
func NewMetricsRouter(rec *metrics.APIRecorder) http.Handler {
	mux := http.NewServeMux()
	if rec.Enabled() {
		mux.Handle("GET /metrics", metrics.Handler(rec.Registry()))
	}
	return RecoveryMiddleware(mux)
}
