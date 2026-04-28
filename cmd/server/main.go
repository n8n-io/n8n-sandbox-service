package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api"
	"github.com/n8n-io/sandbox-service/internal/config"
	"github.com/n8n-io/sandbox-service/internal/manager"
	"github.com/n8n-io/sandbox-service/internal/store"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// Ensure data directory exists.
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		slog.Error("create data dir", "error", err)
		os.Exit(1)
	}

	// Open SQLite store.
	dbPath := filepath.Join(cfg.DataDir, "sandboxes.db")
	s, err := store.New(dbPath)
	if err != nil {
		slog.Error("open store", "error", err)
		os.Exit(1)
	}
	defer s.Close()

	// Create manager (marks stale sandboxes as terminated).
	mgr, err := manager.New(s, cfg)
	if err != nil {
		slog.Error("create manager", "error", err)
		os.Exit(1)
	}

	// Start idle reaper.
	reaperDone := make(chan struct{})
	mgr.StartReaper(reaperDone)

	// Build HTTP handler.
	handler := api.NewRouter(mgr, cfg)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      0, // disabled for streaming exec responses
		IdleTimeout:       120 * time.Second,
	}

	// Start server in background.
	serverErr := make(chan error, 1)
	go func() {
		slog.Info("server listening", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

	// Wait for SIGTERM or SIGINT.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)

	select {
	case sig := <-quit:
		slog.Info("received signal, shutting down", "signal", sig)
	case err := <-serverErr:
		slog.Error("server error", "error", err)
		os.Exit(1)
	}

	// Graceful shutdown sequence:
	// 1. Stop accepting new HTTP requests
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("graceful shutdown failed", "error", err)
	}

	// 2. Stop reaper
	close(reaperDone)

	// 3. Kill all sandboxes, unmount, close clients
	mgr.Shutdown()

	slog.Info("server stopped")
}
