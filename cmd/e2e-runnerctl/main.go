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

func usage() {
	fmt.Fprintf(os.Stderr, "usage: %s stop <sandbox-id> | cordon | uncordon | cordon-status\n", os.Args[0])
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd := os.Args[1]
	var sandboxID string
	switch cmd {
	case "stop":
		if len(os.Args) != 3 {
			usage()
		}
		sandboxID = strings.TrimSpace(os.Args[2])
		if sandboxID == "" {
			fmt.Fprintln(os.Stderr, "sandbox id is required")
			os.Exit(2)
		}
	case "cordon", "uncordon", "cordon-status":
		if len(os.Args) != 2 {
			usage()
		}
	default:
		usage()
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

	if cmd == "stop" {
		if err := runnerctl.StopSandbox(ctx, target, apiKey, tlsCfg, sandboxID); err != nil {
			fmt.Fprintf(os.Stderr, "stop sandbox: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Prints the cordon state after the call, true or false.
	var cordoned bool
	var err error
	if cmd == "cordon-status" {
		cordoned, err = runnerctl.GetCordon(ctx, target, apiKey, tlsCfg)
	} else {
		cordoned, err = runnerctl.SetCordon(ctx, target, apiKey, tlsCfg, "", cmd == "cordon")
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
		os.Exit(1)
	}
	fmt.Println(cordoned)
}
