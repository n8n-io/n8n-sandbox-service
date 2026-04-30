package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

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

	// 2. Clean up containers
	mgr.Shutdown()

	slog.Info("server stopped")
}
