// Package codedaemon implements the runner-facing daemon inside a code sandbox.
// This file sets up its HTTP server, health probe, and client source guard.
package codedaemon

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/n8n-io/sandbox-service/internal/envvar"
)

type Config struct {
	ListenAddr string
	// AllowLocalClients disables the check that rejects requests from inside
	// the sandbox. Only for debugging and local development.
	AllowLocalClients bool
	MaxConcurrency    int // Number of lanes. Each running or uploading task holds one.

	WorkDir        string   // root for task directories and the runtime's working directory
	RuntimeCommand []string // executable followed by its arguments, from the runtime registry
	Limits         Limits   // per-task budgets; non-positive values use defaults
	// RuntimeLogWrites caps the writes to each of the runtime's stdout and
	// stderr that reach the daemon log. Non-positive uses the default of 16.
	RuntimeLogWrites int
	DefaultTimeout   time.Duration
	MaxTimeout       time.Duration
}

func LoadConfig() (Config, error) {
	cfg := Config{ListenAddr: ":8081", MaxConcurrency: 1, RuntimeLogWrites: defaultRuntimeLogWrites}
	envvar.String("CODE_DAEMON_LISTEN_ADDR", &cfg.ListenAddr)
	if err := envvar.Bool("CODE_DAEMON_ALLOW_LOCAL_CLIENTS", &cfg.AllowLocalClients); err != nil {
		return Config{}, err
	}
	if err := envvar.IntBetween("CODE_DAEMON_MAX_CONCURRENCY", &cfg.MaxConcurrency, 1, 32); err != nil {
		return Config{}, err
	}
	limits, err := loadLimits()
	if err != nil {
		return Config{}, err
	}
	cfg.Limits = limits
	if err := envvar.IntBetween("CODE_DAEMON_RUNTIME_LOG_WRITES", &cfg.RuntimeLogWrites, 1, 10_000); err != nil {
		return Config{}, err
	}
	if cfg.DefaultTimeout, cfg.MaxTimeout, err = loadTaskTimeouts(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Daemon serves runner requests over HTTP inside the sandbox.
type Daemon struct {
	cfg   Config
	lanes chan struct{} // One token per occupied lane; capacity is the lane limit.

	tasksMu            sync.Mutex
	dispatchedByTaskID map[string]bool // false before dispatch, true once dispatch is attempted

	workerMu       sync.Mutex
	startupTimeout time.Duration // how long a started worker has to report ready
	worker         *worker       // started lazily and never replaced once set
	startFailed    bool          // set when the worker could not be started; it is not retried
	closed         bool          // prevents starting a worker after Close
}

func New(cfg Config) *Daemon {
	cfg.Limits = cfg.Limits.withDefaults()
	if cfg.RuntimeLogWrites <= 0 {
		cfg.RuntimeLogWrites = defaultRuntimeLogWrites
	}
	if cfg.DefaultTimeout <= 0 {
		cfg.DefaultTimeout = defaultTaskTimeout
	}
	if cfg.MaxTimeout <= 0 {
		cfg.MaxTimeout = maxTaskTimeout
	}
	return &Daemon{
		cfg: cfg, lanes: make(chan struct{}, max(1, cfg.MaxConcurrency)), dispatchedByTaskID: make(map[string]bool),
		startupTimeout: workerStartupTimeout,
	}
}

// Handler registers the runner's routes and applies the client source guard by default.
func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !d.isHealthy() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /tasks", d.handleTask)
	if d.cfg.AllowLocalClients {
		return mux
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The runner connects from outside the sandbox network namespace.
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || isLocalAddress(net.ParseIP(host)) {
			rejectAdmission(w, r, "local clients are not allowed", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// isHealthy reports whether the daemon can still serve tasks. It is healthy
// before its worker starts, and unhealthy once closed, the worker fails to
// start, or the worker stops.
func (d *Daemon) isHealthy() bool {
	d.workerMu.Lock()
	defer d.workerMu.Unlock()
	return !d.closed && !d.startFailed && (d.worker == nil || !d.worker.isStopped())
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
	defer d.Close()
	srv := &http.Server{
		Handler: d.Handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute,
		// Cancelling the daemon also cancels active task requests.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	stopped := make(chan error, 1)
	go func() { stopped <- srv.Serve(listener) }()
	select {
	case err := <-stopped:
		return err
	case <-ctx.Done():
		// Stop the runtime so disconnected handlers waiting for task cleanup
		// can release their lanes and directories during HTTP shutdown.
		d.Close()
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
