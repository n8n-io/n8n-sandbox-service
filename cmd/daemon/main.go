package main

import (
	"flag"
	"log/slog"
	"os"

	"github.com/n8n-io/sandbox-service/internal/daemon"
	"github.com/n8n-io/sandbox-service/internal/logging"
)

func main() {
	listenAddr := flag.String("listen-addr", ":8081", "TCP address to listen on")
	flag.Parse()

	var logLevel slog.LevelVar
	logLevel.Set(slog.LevelInfo)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: &logLevel}))
	slog.SetDefault(logger)

	if v := os.Getenv("SANDBOX_DAEMON_LOG_LEVEL"); v != "" {
		lvl, err := logging.ParseLevel(v)
		if err != nil {
			slog.Error("SANDBOX_DAEMON_LOG_LEVEL", "error", err)
			os.Exit(1)
		}
		logLevel.Set(lvl)
	}

	slog.Info("daemon starting", "listen_addr", *listenAddr)

	if err := daemon.Run(*listenAddr); err != nil {
		slog.Error("daemon exited with error", "error", err)
		os.Exit(1)
	}

	slog.Info("daemon stopped")
}
