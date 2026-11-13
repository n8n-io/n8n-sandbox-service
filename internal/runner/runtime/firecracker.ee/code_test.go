package firecracker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

func testCodeOptions() *runnerruntime.CreateOptions {
	return &runnerruntime.CreateOptions{
		Egress:       runnerruntime.EgressNone,
		Kind:         runnerruntime.KindCode,
		Runtime:      "n8n-js",
		Slots:        2,
		TaskMemoryMB: 512,
		SpareWorkers: true,
		ReuseWorkers: true,
	}
}

func codeTestRuntime(t *testing.T) *Runtime {
	t.Helper()
	rt := testRuntimeT(t, 2)
	stubCreateDeps(rt)
	dir := t.TempDir()
	rt.config.CodeTemplateDir = "/srv/firecracker/code-template"
	rt.config.CodeSnapshotDir = dir
	if err := os.WriteFile(filepath.Join(dir, codeOptionsFileName),
		[]byte(`{"runtime":"n8n-js","slots":2,"taskMemoryMb":512,"spareWorkers":true,"reuseWorkers":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return rt
}

func TestCodeSandboxUsesSharedGoldenCodeSnapshot(t *testing.T) {
	rt := codeTestRuntime(t)
	var commands []recordedCommand
	rt.deps.run = func(_ context.Context, name string, args ...string) error {
		commands = append(commands, recordedCommand{name: name, args: args})
		return nil
	}
	var clonedTemplate string
	rt.deps.cloneRootfs = func(_ context.Context, templatePath, _ string) error {
		clonedTemplate = templatePath
		return nil
	}
	rt.deps.cloneGoldenSnapshot = func(context.Context, string, string, string) error {
		t.Fatal("a code sandbox must not clone the golden snapshot")
		return nil
	}
	rt.deps.probeDaemon = func(context.Context, string) error {
		t.Fatal("a code sandbox must use the code daemon probe")
		return nil
	}
	probed := false
	rt.deps.probeCodeDaemon = func(context.Context, string) error {
		probed = true
		return nil
	}

	if _, err := rt.CreateSandbox(context.Background(), "sandbox-id-123456", testCodeOptions()); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	if clonedTemplate != "/srv/firecracker/code-template/rootfs.ext4" {
		t.Fatalf("cloned template = %s", clonedTemplate)
	}
	if !probed {
		t.Fatal("code daemon probe not called")
	}
	prepareScript := commands[0].args[len(commands[0].args)-1]
	goldenMem := filepath.Join(rt.config.CodeSnapshotDir, "snapshot_mem")
	if !strings.Contains(prepareScript, goldenMem) {
		t.Fatalf("prepare jail script = %q, want golden code snapshot mem bind mount", prepareScript)
	}
	if !strings.Contains(prepareScript, "remount,bind,ro") {
		t.Fatalf("prepare jail script = %q, want read-only snapshot mounts", prepareScript)
	}
	if strings.Contains(prepareScript, "/root/snapshot_mem /srv/jailer") || strings.Contains(prepareScript, "chmod 0664 /srv/jailer/firecracker/sandbox-"+shortID("sandbox-id-123456")+"/root/snapshot_mem") {
		t.Fatalf("prepare jail script = %q, must not chown the golden snapshot", prepareScript)
	}

	if err := rt.StopSandbox(context.Background(), "sandbox-id-123456"); !errors.Is(err, errCodeSandboxNoStop) {
		t.Fatalf("StopSandbox() error = %v, want errCodeSandboxNoStop", err)
	}
}

func TestCodeSandboxRejectsOtherOptions(t *testing.T) {
	rt := codeTestRuntime(t)
	opts := testCodeOptions()
	opts.Slots = 4
	_, err := rt.CreateSandbox(context.Background(), "sandbox-id-123456", opts)
	if err == nil || !strings.Contains(err.Error(), "only serves") {
		t.Fatalf("CreateSandbox() error = %v, want options mismatch", err)
	}
}

func TestCodeSandboxNeedsCodeSnapshot(t *testing.T) {
	rt := testRuntimeT(t, 1)
	stubCreateDeps(rt)
	_, err := rt.CreateSandbox(context.Background(), "sandbox-id-123456", testCodeOptions())
	if err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("CreateSandbox() error = %v, want code sandboxes not enabled", err)
	}
}

func TestCodeSandboxIsNotWokenAfterGuestDeath(t *testing.T) {
	rt := codeTestRuntime(t)
	rt.deps.probeCodeDaemon = func(context.Context, string) error { return nil }
	if _, err := rt.CreateSandbox(context.Background(), "sandbox-id-123456", testCodeOptions()); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	rt.mu.Lock()
	state := rt.sandboxes["sandbox-id-123456"]
	state.running = false
	state.stopped = true
	rt.mu.Unlock()
	if _, err := rt.EnsureSandboxRunning(context.Background(), "sandbox-id-123456"); !errors.Is(err, runnerruntime.ErrSandboxNotRunning) {
		t.Fatalf("EnsureSandboxRunning() error = %v, want ErrSandboxNotRunning", err)
	}
}
