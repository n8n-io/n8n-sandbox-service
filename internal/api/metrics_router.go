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
// rec must be enabled. A disabled recorder has no registry to expose, so a
// caller that got here is about to run a listener that can only 404 — and
// promhttp would panic on the nil registry anyway, further from the cause.
func NewMetricsRouter(rec *metrics.APIRecorder) http.Handler {
	if !rec.Enabled() {
		panic("api: NewMetricsRouter needs an enabled metrics recorder")
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metrics.Handler(rec.Registry()))
	return RecoveryMiddleware(mux)
}
