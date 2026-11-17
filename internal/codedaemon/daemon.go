// Package codedaemon implements the runner-facing daemon inside a code sandbox.
// This file sets up its HTTP server, health probe, and client source guard.
package codedaemon

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/n8n-io/sandbox-service/internal/envvar"
)

// Config controls where the daemon listens and which clients can reach it.
type Config struct {
	ListenAddr string
	// AllowLocalClients disables the check that rejects requests from inside the sandbox.
	AllowLocalClients bool
}

// LoadConfig applies environment overrides to the default daemon settings.
func LoadConfig() (Config, error) {
	cfg := Config{ListenAddr: ":8081"}
	envvar.String("CODE_DAEMON_LISTEN_ADDR", &cfg.ListenAddr)
	if err := envvar.Bool("CODE_DAEMON_ALLOW_LOCAL_CLIENTS", &cfg.AllowLocalClients); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Daemon serves runner requests over HTTP inside the sandbox.
type Daemon struct {
	cfg Config
}

// New creates a daemon without opening its listener.
func New(cfg Config) *Daemon { return &Daemon{cfg: cfg} }

// Handler registers the runner's routes and applies the client source guard by default.
func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
	// The probe confirms that the runner can reach the daemon over HTTP.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	if d.cfg.AllowLocalClients {
		return mux
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The runner connects from outside the sandbox network namespace.
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || isLocalAddress(net.ParseIP(host)) {
			http.Error(w, "local clients are not allowed", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// isLocalAddress identifies loopback and sandbox interface addresses. If the
// source cannot be checked, it treats the request as local so it is rejected.
func isLocalAddress(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
		return true
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return true
	}
	for _, addr := range addrs {
		if n, ok := addr.(*net.IPNet); ok && n.IP.Equal(ip) {
			return true
		}
	}
	return false
}

// Run opens the listener and serves until the context is canceled or serving fails.
func (d *Daemon) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", d.cfg.ListenAddr)
	if err != nil {
		return err
	}
	return d.serve(ctx, listener)
}

// serve runs the HTTP server on an open listener and forces it closed if graceful shutdown fails.
func (d *Daemon) serve(ctx context.Context, listener net.Listener) error {
	srv := &http.Server{Handler: d.Handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute}
	stopped := make(chan error, 1)
	go func() { stopped <- srv.Serve(listener) }()
	select {
	case err := <-stopped:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := srv.Shutdown(shutdown)
		if err != nil {
			_ = srv.Close()
		}
		if serveErr := <-stopped; !errors.Is(serveErr, http.ErrServerClosed) {
			return serveErr
		}
		return err
	}
}
