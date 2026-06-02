package main

import (
	"context"
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
	"github.com/n8n-io/sandbox-service/internal/runner"
	"github.com/n8n-io/sandbox-service/internal/runner/config"
	"github.com/n8n-io/sandbox-service/internal/runner/manager"
	"github.com/n8n-io/sandbox-service/internal/runner/register"
)

func main() {
	var logLevel slog.LevelVar
	logLevel.Set(slog.LevelInfo)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: &logLevel}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}
	logLevel.Set(cfg.LogLevel)

	// Create stateless sandbox runtime.
	rt, err := manager.New(cfg)
	if err != nil {
		slog.Error("create runtime", "error", err)
		os.Exit(1)
	}

	mrec := metrics.NewRunnerRecorder(cfg.MetricsEnabled)
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
		slog.Info("metrics endpoint enabled", "path", "/metrics")
	}

	// Build HTTP handler.
	handler := runner.NewRouter(rt, cfg, mrec)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      0, // disabled for streaming exec responses
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go register.Run(ctx, cfg, rt)

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
	pb.RegisterSandboxControlServer(controlGRPC, &runner.SandboxControlGRPC{Runtime: rt, Cfg: cfg, Rec: mrec})
	go func() {
		if err := controlGRPC.Serve(lis); err != nil {
			slog.Error("control grpc serve", "error", err)
		}
	}()

	// Start server in background.
	serverErr := make(chan error, 1)
	go func() {
		slog.Info("server listening", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
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

	// Graceful shutdown sequence:
	// 1. Stop accepting new HTTP requests
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

	// 2. Clean up sandboxes
	rt.Shutdown(shutdownCtx)

	slog.Info("server stopped")
}
