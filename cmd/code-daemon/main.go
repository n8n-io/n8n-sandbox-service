// Command code-daemon runs inside a code sandbox. It serves the Task API routes
// that the runner proxies. See internal/codedaemon.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/n8n-io/sandbox-service/internal/codedaemon"
	"github.com/n8n-io/sandbox-service/internal/logging"
)

func main() {
	var logLevel slog.LevelVar
	logLevel.Set(slog.LevelInfo)
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: &logLevel})))
	if v := os.Getenv("CODE_DAEMON_LOG_LEVEL"); v != "" {
		lvl, err := logging.ParseLevel(v)
		if err != nil {
			slog.Error("CODE_DAEMON_LOG_LEVEL", "error", err)
			os.Exit(1)
		}
		logLevel.Set(lvl)
	}

	cfg, err := codedaemon.LoadConfig()
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}
	rt, err := codedaemon.LoadRuntime(cfg.RuntimesFile, cfg.Runtime)
	if err != nil {
		slog.Error("load runtime", "error", err)
		os.Exit(1)
	}
	d, err := codedaemon.New(cfg, rt)
	if err != nil {
		slog.Error("init daemon", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	slog.Info("code daemon starting",
		"listen_addr", cfg.ListenAddr,
		"runtime", cfg.Runtime,
		"runtime_version", rt.Version,
		"slots", cfg.Slots,
		"task_memory_mb", cfg.TaskMemoryMB,
		"spare_workers", cfg.SpareWorkers)
	if err := d.Run(ctx); err != nil {
		slog.Error("code daemon exited with error", "error", err)
		os.Exit(1)
	}
	slog.Info("code daemon stopped")
}
