package main

import (
	"net"
	"net/http"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api"
	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/metrics"
)

// newMetricsServer builds the dedicated Prometheus listener, or nils when
// /metrics belongs on the API listener or is switched off altogether.
//
// The listener is bound here rather than in the serving goroutine, so an
// occupied port fails at startup instead of racing the signal handler.
func newMetricsServer(cfg *config.APIConfig, rec *metrics.APIRecorder) (*http.Server, net.Listener, error) {
	if !rec.Enabled() || cfg.MetricsOnMainListener() {
		return nil, nil, nil
	}

	srv := &http.Server{
		Addr:              cfg.MetricsListenAddr,
		Handler:           api.NewMetricsRouter(rec),
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		// Unlike the API server, nothing here streams, so a deadline is safe.
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	lis, err := net.Listen("tcp", cfg.MetricsListenAddr)
	if err != nil {
		return nil, nil, err
	}
	return srv, lis, nil
}
