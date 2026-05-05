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
	"google.golang.org/grpc/credentials/insecure"

	"github.com/n8n-io/sandbox-service/internal/api/grpc/pb"
	"github.com/n8n-io/sandbox-service/internal/grpctls"
	"github.com/n8n-io/sandbox-service/internal/runner"
	"github.com/n8n-io/sandbox-service/internal/runner/config"
	"github.com/n8n-io/sandbox-service/internal/runner/manager"
	"github.com/n8n-io/sandbox-service/internal/runner/register"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// Create stateless container manager.
	mgr, err := manager.New(cfg)
	if err != nil {
		slog.Error("create manager", "error", err)
		os.Exit(1)
	}

	// Build HTTP handler.
	handler := runner.NewRouter(mgr, cfg)

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

	go register.Run(ctx, cfg, mgr)

	var controlGRPC *grpc.Server
	if cfg.ControlGRPCListenAddr != "" {
		lis, err := net.Listen("tcp", cfg.ControlGRPCListenAddr)
		if err != nil {
			slog.Error("control grpc listen", "addr", cfg.ControlGRPCListenAddr, "error", err)
			os.Exit(1)
		}
		var opts []grpc.ServerOption
		if cfg.ControlGRPCServerCertFile != "" {
			creds, err := grpctls.NewServerTransportCredentials(
				cfg.ControlGRPCServerCertFile,
				cfg.ControlGRPCServerKeyFile,
				cfg.ControlGRPCClientCAFile,
			)
			if err != nil {
				slog.Error("control grpc tls", "error", err)
				os.Exit(1)
			}
			opts = append(opts, grpc.Creds(creds))
			slog.Info("sandbox control grpc mTLS enabled", "addr", cfg.ControlGRPCListenAddr)
		} else {
			opts = append(opts, grpc.Creds(insecure.NewCredentials()))
			slog.Info("sandbox control grpc listening (no TLS)", "addr", cfg.ControlGRPCListenAddr)
		}
		controlGRPC = grpc.NewServer(opts...)
		pb.RegisterSandboxControlServer(controlGRPC, &runner.SandboxControlGRPC{Mgr: mgr, Cfg: cfg})
		go func() {
			if err := controlGRPC.Serve(lis); err != nil {
				slog.Error("control grpc serve", "error", err)
			}
		}()
	}

	// Start server in background.
	serverErr := make(chan error, 1)
	go func() {
		slog.Info("server listening", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

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

	if controlGRPC != nil {
		controlGRPC.Stop()
	}

	// 2. Clean up containers
	mgr.Shutdown()

	slog.Info("server stopped")
}
