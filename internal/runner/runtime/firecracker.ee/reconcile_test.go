package firecracker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStartupNetworkCleanupScript(t *testing.T) {
	script := startupNetworkCleanupScript(2)
	if !strings.Contains(script, "fc-sb-0") || !strings.Contains(script, "fc-sb-1") {
		t.Fatalf("script = %q, want slot netns cleanup", script)
	}
	if !strings.Contains(script, "fc-veth-0") || !strings.Contains(script, "fc-veth-1") {
		t.Fatalf("script = %q, want host veth cleanup", script)
	}
}

func TestReconcileSandboxDataDirsRemovesEntries(t *testing.T) {
	dataDir := t.TempDir()
	sandboxDir := filepath.Join(dataDir, "sandbox-id-1234567890")
	if err := os.MkdirAll(filepath.Join(sandboxDir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sandboxDir, "rootfs.ext4"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	rt := newTestRuntime(1)
	rt.runnerConfig.DataDir = dataDir

	if err := rt.reconcileSandboxDataDirs(context.Background()); err != nil {
		t.Fatalf("reconcileSandboxDataDirs() failed: %v", err)
	}
	if _, err := os.Stat(sandboxDir); !os.IsNotExist(err) {
		t.Fatalf("sandbox dir still exists: %v", err)
	}
}

func TestReconcileJailerStateRunsCleanupScript(t *testing.T) {
	var gotScript string
	rt := newTestRuntime(1)
	rt.deps.pathExists = func(path string) bool { return path == "/srv/jailer/firecracker" }
	rt.deps.run = func(_ context.Context, name string, args ...string) error {
		if name != "sudo" || len(args) < 3 || args[0] != "/bin/sh" || args[1] != "-c" {
			t.Fatalf("unexpected command %s %v", name, args)
		}
		gotScript = args[2]
		return nil
	}

	if err := rt.reconcileJailerState(context.Background()); err != nil {
		t.Fatalf("reconcileJailerState() failed: %v", err)
	}
	if !strings.Contains(gotScript, "rm -rf") || !strings.Contains(gotScript, "/srv/jailer/firecracker") {
		t.Fatalf("script = %q, want jailer rm -rf", gotScript)
	}
}

func TestReconcileOnStartupRemovesOrphanSandboxDirs(t *testing.T) {
	dataDir := t.TempDir()
	leftover := filepath.Join(dataDir, "orphan-sandbox-id")
	if err := os.MkdirAll(leftover, 0o755); err != nil {
		t.Fatal(err)
	}

	rt := newTestRuntime(4)
	rt.runnerConfig.DataDir = dataDir
	var netnsScript string
	rt.deps.run = func(_ context.Context, name string, args ...string) error {
		if len(args) >= 3 && args[1] == "-c" && strings.Contains(args[2], "ip netns") {
			netnsScript = args[2]
		}
		return nil
	}
	rt.deps.pathExists = func(string) bool { return false }

	rt.reconcileOnStartup(context.Background())

	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Fatalf("orphan sandbox dir still exists after reconcile: %v", err)
	}
	if netnsScript == "" {
		t.Fatal("expected network cleanup script to run")
	}
}
