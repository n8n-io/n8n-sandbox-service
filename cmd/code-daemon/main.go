// Command code-daemon configures logging and starts the runner-facing daemon inside a code sandbox.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/n8n-io/sandbox-service/internal/codedaemon"
	"github.com/n8n-io/sandbox-service/internal/envvar"
	"github.com/n8n-io/sandbox-service/internal/logging"
)

// main runs the daemon until it exits or receives SIGTERM or SIGINT.
func main() {
	level := slog.LevelInfo
	if v := envvar.Get("CODE_DAEMON_LOG_LEVEL"); v != "" {
		var err error
		level, err = logging.ParseLevel(v)
		if err != nil {
			slog.Error("invalid code daemon log level", "error", err)
			os.Exit(1)
		}
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})))
	cfg, err := codedaemon.LoadConfig()
	if err != nil {
		slog.Error("load code daemon config", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := codedaemon.New(cfg).Run(ctx); err != nil {
		slog.Error("code daemon exited", "error", err)
		os.Exit(1)
	}
}
