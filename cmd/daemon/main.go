// Command daemon is the sandbox-daemon that runs inside every sandbox (container
// or microVM) and serves exec and file operations to the runner. Started with
// build parameters as the init of a builder microVM, it builds one volume image
// instead and serves nothing.
package main

import (
	"errors"
	"flag"
	"log/slog"
	"os"

	"github.com/n8n-io/sandbox-service/internal/daemon"
	"github.com/n8n-io/sandbox-service/internal/daemon/volumebuild"
	"github.com/n8n-io/sandbox-service/internal/logging"
)

func main() {
	listenAddr := flag.String("listen-addr", ":8081", "TCP address to listen on")
	flag.Parse()

	var logLevel slog.LevelVar
	logLevel.Set(slog.LevelInfo)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: &logLevel}))
	slog.SetDefault(logger)

	if params, requested, err := volumebuild.ParamsFromEnv(os.LookupEnv); requested {
		buildVolume(params, err)
	}

	if v := os.Getenv("SANDBOX_DAEMON_LOG_LEVEL"); v != "" {
		lvl, err := logging.ParseLevel(v)
		if err != nil {
			slog.Error("SANDBOX_DAEMON_LOG_LEVEL", "error", err)
			os.Exit(1)
		}
		logLevel.Set(lvl)
	}

	slog.Info("daemon starting", "listen_addr", *listenAddr)

	// A daemon started as root keeps it until the runner's volume mount
	// request; see daemon.NewHandler.
	if err := daemon.Run(*listenAddr); err != nil {
		slog.Error("daemon exited with error", "error", err)
		os.Exit(1)
	}

	slog.Info("daemon stopped")
}

// buildVolume is build mode and never returns. Anyone but the init of a
// builder microVM gets an error and nothing else.
func buildVolume(params volumebuild.Params, paramsErr error) {
	err := volumebuild.New(volumebuild.System()).Run(params, paramsErr)
	if errors.Is(err, volumebuild.ErrNotInit) {
		slog.Error("volume build refused", "error", err)
		os.Exit(1)
	}
	// Only a failed reboot gets here, after the status record was written. An
	// init that exits panics the kernel, and the guest's panic=1 then ends
	// the microVM, so the runner need not wait for its time limit.
	slog.Error("volume build could not reboot", "error", err)
	os.Exit(1)
}
