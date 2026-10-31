// Command e2e-runnerctl calls a runner's SandboxControl gRPC for e2e tests.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/runnerctl"
)

func main() {
	if len(os.Args) != 3 || os.Args[1] != "stop" {
		fmt.Fprintf(os.Stderr, "usage: %s stop <sandbox-id>\n", os.Args[0])
		os.Exit(2)
	}
	sandboxID := strings.TrimSpace(os.Args[2])
	if sandboxID == "" {
		fmt.Fprintln(os.Stderr, "sandbox id is required")
		os.Exit(2)
	}

	target := strings.TrimSpace(os.Getenv("E2E_RUNNER_CONTROL_GRPC_ADDR"))
	apiKey := strings.TrimSpace(os.Getenv("E2E_RUNNER_API_KEY"))
	if target == "" || apiKey == "" {
		fmt.Fprintln(os.Stderr, "E2E_RUNNER_CONTROL_GRPC_ADDR and E2E_RUNNER_API_KEY must be set")
		os.Exit(2)
	}

	tlsCfg := &runnerctl.TLS{
		CAFile:     os.Getenv("E2E_RUNNER_CONTROL_TLS_CA"),
		CertFile:   os.Getenv("E2E_RUNNER_CONTROL_TLS_CERT"),
		KeyFile:    os.Getenv("E2E_RUNNER_CONTROL_TLS_KEY"),
		ServerName: strings.TrimSpace(os.Getenv("E2E_RUNNER_CONTROL_TLS_SERVER_NAME")),
	}
	if tlsCfg.CAFile == "" || tlsCfg.CertFile == "" || tlsCfg.KeyFile == "" {
		fmt.Fprintln(os.Stderr, "E2E_RUNNER_CONTROL_TLS_CA, _CERT, and _KEY must be set")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := runnerctl.StopSandbox(ctx, target, apiKey, tlsCfg, sandboxID); err != nil {
		fmt.Fprintf(os.Stderr, "stop sandbox: %v\n", err)
		os.Exit(1)
	}
}
