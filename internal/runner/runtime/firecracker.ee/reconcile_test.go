package firecracker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestStartupNetworkCleanupScript(t *testing.T) {
	script := startupNetworkCleanupScript()
	if !strings.Contains(script, "grep '^fc-sb-' | xargs -r -n1 ip netns delete") {
		t.Fatalf("script = %q, want slot netns cleanup", script)
	}
	if !strings.Contains(script, "grep '^fc-veth-' | xargs -r -n1 ip link delete") {
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

// A missing data dir is reported, not created: it may be the mount point of a
// volume that is not mounted, and creating it would put sandboxes on the
// filesystem underneath.
func TestReconcileSandboxDataDirsReportsMissingDir(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "sandboxes")
	rt := newTestRuntime(1)
	rt.runnerConfig.DataDir = dataDir

	if err := rt.reconcileSandboxDataDirs(context.Background()); err == nil || !strings.Contains(err.Error(), dataDir) {
		t.Fatalf("reconcileSandboxDataDirs() error = %v, want the missing data dir named", err)
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatalf("data dir after reconcile: stat error = %v, want it still missing", err)
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

func TestSnapshotBuildReapScriptKillsTheVMBeforeRemovingItsJail(t *testing.T) {
	script := snapshotBuildReapScript("/srv/jailer's base")
	kill := strings.Index(script, "grep '^fc-snapshot-'")
	pids := strings.Index(script, "ip netns pids \"$name\" | xargs -r kill -KILL")
	unmount := strings.Index(script, "umount -l {}")
	remove := strings.Index(script, "rm -rf \"$jail\"")
	if kill < 0 || pids < kill || unmount < pids || remove < unmount {
		t.Fatalf("script = %q, want the snapshot netns killed, then its binds unmounted, then the jail removed", script)
	}
	// find ignores the result of -exec ... {} \;, so only {} + lets a failed rm
	// fail the script.
	if strings.Contains(script, "while ") || !strings.Contains(script, "' sh {} +") {
		t.Fatalf("script = %q, want find -exec ... {} + so a failed rm fails the script", script)
	}
	if strings.Contains(script, "fc-sb-") {
		t.Fatalf("script = %q, want only snapshot-builder netns", script)
	}
	if !strings.Contains(script, "'/srv/jailer'\"'\"'s base/firecracker'") || strings.Contains(script, "/srv/jailer's base") {
		t.Fatalf("script = %q, want the jailer path quoted", script)
	}
	if !strings.Contains(script, "-name 'snapshot-*'") {
		t.Fatalf("script = %q, want only snapshot-* jails removed", script)
	}
	if err := exec.Command("/bin/sh", "-n", "-c", script).Run(); err != nil {
		t.Fatalf("script does not parse: %v\n%s", err, script)
	}
	innerStart := strings.Index(script, "sh -c '")
	innerEnd := strings.LastIndex(script, "' sh")
	if innerStart < 0 || innerEnd < innerStart {
		t.Fatalf("script = %q, want an inner sh -c", script)
	}
	inner := script[innerStart+len("sh -c '") : innerEnd]
	if err := exec.Command("/bin/sh", "-n", "-c", inner).Run(); err != nil {
		t.Fatalf("inner script does not parse: %v\n%s", err, inner)
	}
}
