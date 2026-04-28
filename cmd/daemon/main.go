package main

import (
	"flag"
	"log/slog"
	"os"

	"github.com/n8n-io/sandbox-service/internal/daemon"
)

func main() {
	listenAddr := flag.String("listen-addr", ":8081", "TCP address to listen on")
	baseDir := flag.String("base-dir", "/", "Base directory for file operations")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	slog.SetDefault(logger)

	slog.Info("daemon starting", "listen_addr", *listenAddr, "base_dir", *baseDir)

	if err := daemon.Run(*listenAddr, *baseDir); err != nil {
		slog.Error("daemon exited with error", "error", err)
		os.Exit(1)
	}

	slog.Info("daemon stopped")
}
