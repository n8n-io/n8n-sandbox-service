// Package app wires a runtime, the runner HTTP and gRPC servers, registration
// and metrics into one runner process.
package app

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"

	"github.com/n8n-io/sandbox-service/internal/api/grpc/pb"
	"github.com/n8n-io/sandbox-service/internal/grpctls"
	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/obs"
	"github.com/n8n-io/sandbox-service/internal/runner"
	"github.com/n8n-io/sandbox-service/internal/runner/config"
	"github.com/n8n-io/sandbox-service/internal/runner/cordon"
	"github.com/n8n-io/sandbox-service/internal/runner/register"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
	"github.com/n8n-io/sandbox-service/internal/runner/volumecache"
)

type RuntimeFactory func(*config.Config) (runnerruntime.Runtime, error)

// Main loads shared runner config, constructs the backend runtime, and runs the
// common HTTP, registration, and control gRPC servers.
func Main(runtimeName string, factory RuntimeFactory) {
	var logLevel slog.LevelVar
	logLevel.Set(slog.LevelInfo)
	logger := slog.New(obs.TraceHandler(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: &logLevel})))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}
	logLevel.Set(cfg.LogLevel)

	rt, err := factory(cfg)
	if err != nil {
		slog.Error("create runtime", "runtime", runtimeName, "error", err)
		os.Exit(1)
	}
	slog.Info("runner runtime selected", "backend", runtimeName)

	Run(cfg, rt)
}

func Run(cfg *config.Config, rt runnerruntime.Runtime) {
	cd := cordon.New()
	mrec := metrics.NewRunnerRecorder(cfg.MetricsEnabled)
	if fc, ok := rt.(interface{ SetMetricsRecorder(*metrics.RunnerRecorder) }); ok {
		fc.SetMetricsRecorder(mrec)
	}
	if mrec.Enabled() {
		mrec.SetActiveContainers(func() float64 {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			capacity, err := rt.Capacity(ctx)
			if err != nil {
				slog.Warn("metrics: read runtime capacity", "error", err)
				return 0
			}
			return float64(capacity.Used)
		})
		mrec.SetStoppedContainers(func() float64 {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			capacity, err := rt.Capacity(ctx)
			if err != nil {
				slog.Warn("metrics: read runtime capacity", "error", err)
				return 0
			}
			return float64(capacity.Stopped)
		})
		mrec.SetCordoned(func() float64 {
			if cd.Cordoned() {
				return 1
			}
			return 0
		})
		mrec.SetTLSCertificateExpiry(grpctls.CertificateExpiryByCertPath)
		slog.Info("metrics endpoint enabled", "path", "/metrics")
	}

	// After the runtime is built: Firecracker's startup reconcile removes every
	// directory in the data dir, the default cache dir included. Nothing
	// acquires from the cache yet; building it wipes the dir, registers its
	// metrics and sets its budget aside from creates.
	cacheConfig := volumecache.Config{
		Dir:          cfg.VolumeCacheDir,
		DataDir:      cfg.DataDir,
		MaxBytes:     cfg.VolumeCacheMaxBytes,
		FetchTimeout: cfg.VolumeFetchTimeout,
		Metrics:      mrec,
	}
	// Volume objects the API serves itself are fetched with the identity the
	// runner registers with. An address gRPC can dial but that names no host
	// leaves those downloads without it, and the API refuses them.
	if apiHost, _, err := net.SplitHostPort(cfg.APIGRPCAddr); err != nil || apiHost == "" {
		slog.Warn("volume cache: SANDBOX_RUNNER_API_GRPC_ADDR is not host:port, so downloads from the API present no client certificate", "addr", cfg.APIGRPCAddr, "error", err)
	} else {
		apiTLS, err := grpctls.NewClientTLSConfig(cfg.GRPCServerCAFile, cfg.GRPCClientCertFile, cfg.GRPCClientKeyFile, cfg.GRPCServerName)
		if err != nil {
			slog.Error("volume cache tls", "error", err)
			os.Exit(1)
		}
		cacheConfig.APIHost, cacheConfig.APITLS = apiHost, apiTLS
	}
	cache, err := volumecache.New(cacheConfig)
	if err != nil {
		slog.Error("volume cache", "error", err)
		os.Exit(1)
	}
	if reserver, ok := rt.(runnerruntime.DiskReserver); ok {
		reserver.SetDiskReservation(cache.DataDirReservation)
	}

	handler := runner.NewRouter(rt, cfg, mrec)

	// VerifyClientCertIfGiven rather than RequireAndVerifyClientCert: health and
	// metrics must stay reachable by probes, which cannot present a client
	// certificate. AuthMiddleware requires one on every other route.
	httpTLS, err := grpctls.NewServerTLSConfig(
		cfg.ControlGRPCServerCertFile,
		cfg.ControlGRPCServerKeyFile,
		cfg.ControlGRPCClientCAFile,
		tls.VerifyClientCertIfGiven,
	)
	if err != nil {
		slog.Error("http tls", "error", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		TLSConfig:         httpTLS,
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      0, // disabled for streaming exec responses
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go register.Run(ctx, cfg, rt, cd)

	lis, err := net.Listen("tcp", cfg.ControlGRPCListenAddr)
	if err != nil {
		slog.Error("control grpc listen", "addr", cfg.ControlGRPCListenAddr, "error", err)
		os.Exit(1)
	}
	creds, err := grpctls.NewServerTransportCredentials(
		cfg.ControlGRPCServerCertFile,
		cfg.ControlGRPCServerKeyFile,
		cfg.ControlGRPCClientCAFile,
	)
	if err != nil {
		slog.Error("control grpc tls", "error", err)
		os.Exit(1)
	}
	slog.Info("sandbox control grpc mTLS enabled", "addr", cfg.ControlGRPCListenAddr)
	controlGRPC := grpc.NewServer(grpc.Creds(creds))
	pb.RegisterSandboxControlServer(controlGRPC, &runner.SandboxControlGRPC{Runtime: rt, Cfg: cfg, Rec: mrec, Cordon: cd})
	go func() {
		if err := controlGRPC.Serve(lis); err != nil {
			slog.Error("control grpc serve", "error", err)
		}
	}()

	serverErr := make(chan error, 1)
	go func() {
		slog.Info("server listening",
			"addr", cfg.ListenAddr,
			"tls", true,
			"max_inflight_per_sandbox", cfg.MaxInflightPerSandbox,
			"max_exec_timeout", cfg.MaxExecTimeout.String())
		// Certificate and key come from srv.TLSConfig.
		if err := srv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

	go rt.Prepare(ctx)

	select {
	case <-ctx.Done():
		slog.Info("received signal, shutting down")
	case err := <-serverErr:
		slog.Error("server error", "error", err)
		os.Exit(1)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown failed", "error", err)
	}

	done := make(chan struct{})
	go func() {
		controlGRPC.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		slog.Warn("control grpc graceful shutdown timed out; forcing stop")
		controlGRPC.Stop()
	}

	// Fresh budget: a long HTTP drain can use up shutdownCtx, and the
	// sandbox cleanup must still get to run.
	cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelCleanup()
	rt.Shutdown(cleanupCtx)

	slog.Info("server stopped")
}
