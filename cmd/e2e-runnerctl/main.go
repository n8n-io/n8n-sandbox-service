// Command e2e-runnerctl calls a runner's SandboxControl gRPC for e2e tests.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc/status"

	"github.com/n8n-io/sandbox-service/internal/api/runnerctl"
	"github.com/n8n-io/sandbox-service/internal/daemon"
	"github.com/n8n-io/sandbox-service/internal/grpctls"
)

func usage() {
	fmt.Fprintf(os.Stderr, `usage:
  %[1]s stop <sandbox-id>
  %[1]s create <sandbox-id> [create-options-json]
  %[1]s delete <sandbox-id>
  %[1]s exec <sandbox-id> <command>

create and exec print their outcome as one line of JSON. create exits 1 when
the create fails, exec when the runner answers other than 200 or the command's
exit never arrives. exec goes to the runner's HTTP listener at
E2E_RUNNER_HTTP_ADDR, with the control mTLS identity.
`, os.Args[0])
	os.Exit(2)
}

type target struct {
	addr   string
	apiKey string
	tls    *runnerctl.TLS
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "stop":
		if len(os.Args) != 3 {
			usage()
		}
		stop(strings.TrimSpace(os.Args[2]))
	case "create":
		if len(os.Args) != 3 && len(os.Args) != 4 {
			usage()
		}
		createOptions := ""
		if len(os.Args) == 4 {
			createOptions = os.Args[3]
		}
		create(sandboxID(os.Args[2]), createOptions)
	case "delete":
		if len(os.Args) != 3 {
			usage()
		}
		remove(sandboxID(os.Args[2]))
	case "exec":
		if len(os.Args) != 4 {
			usage()
		}
		execute(sandboxID(os.Args[2]), os.Args[3])
	default:
		usage()
	}
}

func sandboxID(arg string) string {
	id := strings.TrimSpace(arg)
	if id == "" {
		fmt.Fprintln(os.Stderr, "sandbox id is required")
		os.Exit(2)
	}
	return id
}

func runner() target {
	addr := strings.TrimSpace(os.Getenv("E2E_RUNNER_CONTROL_GRPC_ADDR"))
	apiKey := strings.TrimSpace(os.Getenv("E2E_RUNNER_API_KEY"))
	if addr == "" || apiKey == "" {
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
	return target{addr: addr, apiKey: apiKey, tls: tlsCfg}
}

func stop(sandboxID string) {
	if sandboxID == "" {
		fmt.Fprintln(os.Stderr, "sandbox id is required")
		os.Exit(2)
	}
	to := runner()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := runnerctl.StopSandbox(ctx, to.addr, to.apiKey, to.tls, sandboxID); err != nil {
		fmt.Fprintf(os.Stderr, "stop sandbox: %v\n", err)
		os.Exit(1)
	}
}

// createOutcome is what create prints.
type createOutcome struct {
	// Result is "ok", "refused" (a capacity refusal) or "error".
	Result string `json:"result"`
	// Applied is applied_create_options_json.
	Applied json.RawMessage `json:"applied,omitempty"`
	// Code and Message are the gRPC status of an error.
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

func create(sandboxID, createOptions string) {
	to := runner()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	resp, err := runnerctl.CreateSandbox(ctx, to.addr, to.apiKey, to.tls, sandboxID, createOptions)
	outcome := createOutcome{Result: "ok"}
	switch {
	case err == nil && resp.GetAppliedCreateOptionsJson() != "":
		outcome.Applied = json.RawMessage(resp.GetAppliedCreateOptionsJson())
	case err == nil:
	case runnerctl.IsCapacityRefusal(err):
		outcome = createOutcome{Result: "refused"}
	default:
		outcome = createOutcome{Result: "error", Code: status.Code(err).String(), Message: status.Convert(err).Message()}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "create sandbox: %v\n", err)
	}
	printJSON(outcome)
	if err != nil {
		os.Exit(1)
	}
}

func remove(sandboxID string) {
	to := runner()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := runnerctl.DeleteSandbox(ctx, to.addr, to.apiKey, to.tls, sandboxID); err != nil {
		fmt.Fprintf(os.Stderr, "delete sandbox: %v\n", err)
		os.Exit(1)
	}
}

// execOutcome is what exec prints: the HTTP status and, for 200, the output
// and exit code; otherwise the body.
type execOutcome struct {
	Status   int    `json:"status"`
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	Body     string `json:"body,omitempty"`
}

func execute(sandboxID, command string) {
	to := runner()
	httpAddr := strings.TrimSpace(os.Getenv("E2E_RUNNER_HTTP_ADDR"))
	if httpAddr == "" {
		fmt.Fprintln(os.Stderr, "E2E_RUNNER_HTTP_ADDR must be set")
		os.Exit(2)
	}
	tlsConfig, err := grpctls.NewClientTLSConfig(to.tls.CAFile, to.tls.CertFile, to.tls.KeyFile, to.tls.ServerName)
	exitOn("runner TLS", err)
	client := &http.Client{
		Transport:     &http.Transport{TLSClientConfig: tlsConfig},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	body, err := json.Marshal(map[string]any{"command": command, "timeout_ms": 60_000})
	exitOn("encode exec", err)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	execURL := "https://" + httpAddr + "/sandboxes/" + url.PathEscape(sandboxID) + "/executions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, execURL, bytes.NewReader(body))
	exitOn("build exec request", err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", to.apiKey)
	resp, err := client.Do(req)
	exitOn("exec", err)
	defer resp.Body.Close()

	outcome := execOutcome{Status: resp.StatusCode, ExitCode: -1}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		outcome.Body = string(raw)
		printJSON(outcome)
		os.Exit(1)
	}
	var stdout, stderr strings.Builder
	exited := false
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for scanner.Scan() {
		var event daemon.Response
		exitOn("decode exec event", json.Unmarshal(scanner.Bytes(), &event))
		switch event.Type {
		case daemon.ResponseTypeStdout:
			stdout.WriteString(event.Data)
		case daemon.ResponseTypeStderr:
			stderr.WriteString(event.Data)
		case daemon.ResponseTypeExit:
			outcome.ExitCode = event.ExitCode
			exited = true
		case daemon.ResponseTypeError:
			outcome.Body = event.Error
		}
	}
	exitOn("read exec stream", scanner.Err())
	outcome.Stdout, outcome.Stderr = stdout.String(), stderr.String()
	printJSON(outcome)
	if !exited {
		os.Exit(1)
	}
}

func printJSON(value any) {
	line, err := json.Marshal(value)
	exitOn("encode outcome", err)
	fmt.Println(string(line))
}

func exitOn(action string, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", action, err)
		os.Exit(1)
	}
}
