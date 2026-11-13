package main

import (
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api"
	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/volumestore"
	"github.com/n8n-io/sandbox-service/internal/grpctls"
	"github.com/n8n-io/sandbox-service/internal/metrics"
)

// newVolumeObjectsServer builds the listener that serves the local volume
// store's objects to runners, or nils for any other store.
//
// Only a runner gets a connection: the listener presents the registration
// listener's certificate and requires a client certificate from its CA, as
// runner registration does.
//
// The listener is bound here rather than in the serving goroutine, so an
// occupied port fails at startup instead of racing the signal handler.
func newVolumeObjectsServer(cfg *config.APIConfig, volumes volumestore.Store, rec *metrics.APIRecorder) (*http.Server, net.Listener, error) {
	local, ok := volumes.(*volumestore.Local)
	if !ok {
		return nil, nil, nil
	}

	tlsConf, err := grpctls.NewServerTLSConfig(
		cfg.GRPCServerCertFile,
		cfg.GRPCServerKeyFile,
		cfg.GRPCClientCAFile,
		tls.RequireAndVerifyClientCert,
	)
	if err != nil {
		return nil, nil, err
	}

	srv := &http.Server{
		Addr:              cfg.VolumeLocalListenAddr,
		Handler:           api.NewVolumeObjectsRouter(local, cfg.MaxInflightPerTenant, rec),
		TLSConfig:         tlsConf,
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		// A large download outlasts any fixed deadline; the handler ends a
		// stalled one instead.
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
		// A refused handshake is reported here; keep it in the JSON log.
		ErrorLog: slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
	}

	lis, err := net.Listen("tcp", cfg.VolumeLocalListenAddr)
	if err != nil {
		return nil, nil, err
	}
	return srv, lis, nil
}
