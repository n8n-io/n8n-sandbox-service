package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/obs"
)

const testTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

// captureLifecycleEvents redirects the default logger into a buffer and returns
// the lifecycle events logged with the given message.
func captureLifecycleEvents(t *testing.T) func(msg string) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	return func(msg string) []map[string]any {
		var events []map[string]any
		for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
			if len(line) == 0 {
				continue
			}
			var event map[string]any
			if err := json.Unmarshal(line, &event); err != nil {
				t.Fatalf("log line is not JSON: %v", err)
			}
			if event["msg"] == msg {
				events = append(events, event)
			}
		}
		return events
	}
}

func requireStepFields(t *testing.T, event map[string]any, steps ...string) {
	t.Helper()
	for _, step := range steps {
		if _, ok := event[step+"_ms"]; !ok {
			t.Fatalf("event is missing %s_ms: %v", step, event)
		}
	}
	if _, ok := event["total_ms"]; !ok {
		t.Fatalf("event is missing total_ms: %v", event)
	}
}

func TestCreateSandboxEmitsStepTimings(t *testing.T) {
	events := captureLifecycleEvents(t)
	rt := testRuntimeT(t, 1)
	stubCreateDeps(rt)
	rt.SetMetricsRecorder(metrics.NewRunnerRecorder(true))

	ctx := obs.WithTraceparent(context.Background(), testTraceparent)
	if _, err := rt.CreateSandbox(ctx, "sandbox-id-123456", nil); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}

	created := events("firecracker sandbox created")
	if len(created) != 1 {
		t.Fatalf("got %d create events, want 1", len(created))
	}
	if got := created[0]["trace_id"]; got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("create event trace_id = %v, want the caller's trace id", got)
	}
	if got := created[0]["op"]; got != metrics.OpCreate {
		t.Fatalf("create event op = %v, want %q", got, metrics.OpCreate)
	}
	requireStepFields(t, created[0],
		stepCloneRootfs, stepCloneSnapshot, stepPrepareJail, stepSetupNetwork,
		stepStartJailer, stepWaitSocket, stepLoadSnapshot, stepStartProxy, stepProbeDaemon)
}

func TestEnsureSandboxRunningEmitsStepTimings(t *testing.T) {
	events := captureLifecycleEvents(t)
	rt := testRuntimeT(t, 1)
	stubCreateDeps(rt)

	const sandboxID = "sandbox-id-123456"
	if _, err := rt.CreateSandbox(context.Background(), sandboxID, nil); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	if err := rt.StopSandbox(context.Background(), sandboxID); err != nil {
		t.Fatalf("StopSandbox() failed: %v", err)
	}

	ctx := obs.WithTraceparent(context.Background(), testTraceparent)
	if err := rt.EnsureSandboxRunning(ctx, sandboxID); err != nil {
		t.Fatalf("EnsureSandboxRunning() failed: %v", err)
	}

	woke := events("firecracker sandbox woke")
	if len(woke) != 1 {
		t.Fatalf("got %d wake events, want 1", len(woke))
	}
	if got := woke[0]["trace_id"]; got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("wake event trace_id = %v, want the caller's trace id", got)
	}
	if got := woke[0]["op"]; got != metrics.OpEnsureRunning {
		t.Fatalf("wake event op = %v, want %q", got, metrics.OpEnsureRunning)
	}
	requireStepFields(t, woke[0],
		stepPrepareJail, stepSetupNetwork, stepStartJailer,
		stepWaitSocket, stepLoadSnapshot, stepStartProxy, stepProbeDaemon)
	// A wake reuses the sandbox's disk, so it must not repeat the clone steps.
	if _, ok := woke[0][stepCloneRootfs+"_ms"]; ok {
		t.Fatalf("wake event records a rootfs clone: %v", woke[0])
	}
}

func TestStepTimingsWorkWithoutRecorder(t *testing.T) {
	events := captureLifecycleEvents(t)
	rt := testRuntimeT(t, 1)
	stubCreateDeps(rt)

	if _, err := rt.CreateSandbox(context.Background(), "sandbox-id-123456", nil); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}

	created := events("firecracker sandbox created")
	if len(created) != 1 {
		t.Fatalf("got %d create events, want 1", len(created))
	}
	if got, ok := created[0]["trace_id"]; !ok || got != "" {
		t.Fatalf("create event trace_id = %v, want empty for an untraced call", got)
	}
}
