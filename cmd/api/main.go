package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api"
	"github.com/n8n-io/sandbox-service/internal/api/config"
	grpcapi "github.com/n8n-io/sandbox-service/internal/api/grpc"
	"github.com/n8n-io/sandbox-service/internal/api/grpc/pb"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/grpctls"
	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/obs"
	"google.golang.org/grpc"
)

func main() {
	var logLevel slog.LevelVar
	logLevel.Set(slog.LevelInfo)
	logger := slog.New(obs.TraceHandler(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: &logLevel})))
	slog.SetDefault(logger)

	cfg, err := config.LoadAPI()
	if err != nil {
		slog.Error("failed to load api config", "error", err)
		os.Exit(1)
	}
	logLevel.Set(cfg.LogLevel)

	var (
		sandboxStore store.SandboxStore
		runnerReg    registry.RunnerRegistry
		sweepLockDB  *sql.DB
	)

	switch cfg.Store {
	case config.StorePostgres:
		pgStore, err := store.NewPostgres(cfg.Postgres)
		if err != nil {
			slog.Error("open postgres store", "error", err)
			os.Exit(1)
		}
		sandboxStore = pgStore
		runnerReg = registry.NewPostgres(pgStore.DB(), cfg.HeartbeatGrace)
		sweepLockDB = pgStore.DB()
		slog.Info("using postgres store", "host", cfg.Postgres.Host, "db", cfg.Postgres.Database)
	default:
		if info, err := os.Stat(cfg.DataDir); err != nil {
			slog.Error("data dir not accessible", "path", cfg.DataDir, "error", err)
			os.Exit(1)
		} else if !info.IsDir() {
			slog.Error("data dir is not a directory", "path", cfg.DataDir)
			os.Exit(1)
		}

		dbPath := filepath.Join(cfg.DataDir, "api.db")
		sqliteStore, err := store.NewSQLite(dbPath)
		if err != nil {
			slog.Error("open sqlite store", "error", err)
			os.Exit(1)
		}
		sandboxStore = sqliteStore
		runnerReg = registry.NewMemory(cfg.HeartbeatGrace)
		slog.Info("using sqlite store", "path", dbPath)
	}
	defer sandboxStore.Close()

	mrec := metrics.NewAPIRecorder(cfg.MetricsEnabled)
	if mrec.Enabled() {
		mrec.SetActiveSandboxes(func() float64 {
			n, err := sandboxStore.Count()
			if err != nil {
				slog.Warn("metrics: count sandboxes", "error", err)
				return 0
			}
			return float64(n)
		})
		mrec.SetRunnersRegistered(func() float64 { return float64(runnerReg.Len()) })
		slog.Info("metrics endpoint enabled",
			"path", "/metrics",
			"addr", cfg.ResolvedMetricsListenAddr(),
			"dedicated_listener", !cfg.MetricsOnMainListener())
	} else if cfg.MetricsListenAddr != "" {
		slog.Warn("metrics listen addr set but SANDBOX_API_METRICS_ENABLED is false; no metrics listener will start",
			"addr", cfg.MetricsListenAddr)
	}

	api.LogIdleSweepConfig(cfg)

	sweepCtx, sweepCancel := context.WithCancel(context.Background())
	defer sweepCancel()
	api.StartIdleSweeper(sweepCtx, sandboxStore, runnerReg, cfg, sweepLockDB)

	handler, err := api.NewGatewayRouter(sandboxStore, cfg, runnerReg, mrec)
	if err != nil {
		slog.Error("failed to create api router", "error", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
	}

	// Bound before any goroutine starts, so an occupied port fails at startup
	// rather than racing the signal handler.
	var (
		metricsSrv *http.Server
		metricsLis net.Listener
	)
	if mrec.Enabled() && !cfg.MetricsOnMainListener() {
		metricsSrv = &http.Server{
			Addr:              cfg.MetricsListenAddr,
			Handler:           api.NewMetricsRouter(mrec),
			ReadTimeout:       30 * time.Second,
			ReadHeaderTimeout: 10 * time.Second,
			// Unlike the API server, nothing here streams, so a deadline is safe.
			WriteTimeout: 30 * time.Second,
			IdleTimeout:  120 * time.Second,
		}
		metricsLis, err = net.Listen("tcp", cfg.MetricsListenAddr)
		if err != nil {
			slog.Error("metrics listen", "addr", cfg.MetricsListenAddr, "error", err)
			os.Exit(1)
		}
	}

	grpcLis, err := net.Listen("tcp", cfg.GRPCListenAddr)
	if err != nil {
		slog.Error("grpc listen", "addr", cfg.GRPCListenAddr, "error", err)
		os.Exit(1)
	}
	creds, err := grpctls.NewServerTransportCredentials(
		cfg.GRPCServerCertFile,
		cfg.GRPCServerKeyFile,
		cfg.GRPCClientCAFile,
	)
	if err != nil {
		slog.Error("grpc tls credentials", "error", err)
		os.Exit(1)
	}
	slog.Info("runner registry grpc mTLS enabled")
	grpcSrv := grpc.NewServer(grpc.Creds(creds))
	pb.RegisterRunnerRegistryServer(grpcSrv, &grpcapi.RunnerRegistryServer{
		Token: cfg.RegistrationToken,
		Reg:   runnerReg,
	})

	// One slot per listener, so a second failure never blocks its goroutine.
	serverErr := make(chan error, 3)
	go func() {
		slog.Info("api listening", "addr", cfg.ListenAddr, "grpc_addr", cfg.GRPCListenAddr, "store", cfg.Store)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

	go func() {
		slog.Info("runner registry grpc listening", "addr", cfg.GRPCListenAddr)
		if err := grpcSrv.Serve(grpcLis); err != nil {
			slog.Error("grpc server error", "error", err)
			serverErr <- err
		}
	}()

	if metricsSrv != nil {
		go func() {
			slog.Info("metrics listening", "addr", cfg.MetricsListenAddr, "path", "/metrics")
			if err := metricsSrv.Serve(metricsLis); err != nil && err != http.ErrServerClosed {
				serverErr <- err
			}
		}()
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)

	select {
	case sig := <-quit:
		slog.Info("received signal, shutting down api", "signal", sig)
		sweepCancel()
	case err := <-serverErr:
		sweepCancel()
		slog.Error("server error", "error", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	grpcSrv.Stop()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("graceful shutdown failed", "error", err)
	}
	// Metrics last, so a scrape in flight during a normal drain still completes.
	// The deadline above is deliberately shared: once it is spent the kubelet is
	// at terminationGracePeriodSeconds and about to SIGKILL us, so that case is
	// expected rather than an error worth logging.
	if metricsSrv != nil {
		if err := metricsSrv.Shutdown(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
			slog.Error("metrics graceful shutdown failed", "error", err)
		}
	}

	slog.Info("api stopped")
}
