// Package codedaemon implements the runner-facing daemon inside a code sandbox.
package codedaemon

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/n8n-io/sandbox-service/internal/envvar"
)

type Config struct {
	ListenAddr        string
	AllowLocalClients bool
}

func LoadConfig() (Config, error) {
	cfg := Config{ListenAddr: ":8081"}
	envvar.String("CODE_DAEMON_LISTEN_ADDR", &cfg.ListenAddr)
	if err := envvar.Bool("CODE_DAEMON_ALLOW_LOCAL_CLIENTS", &cfg.AllowLocalClients); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

type Daemon struct {
	cfg Config
}

func New(cfg Config) *Daemon { return &Daemon{cfg: cfg} }

func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
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

func (d *Daemon) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", d.cfg.ListenAddr)
	if err != nil {
		return err
	}
	return d.serve(ctx, listener)
}

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
