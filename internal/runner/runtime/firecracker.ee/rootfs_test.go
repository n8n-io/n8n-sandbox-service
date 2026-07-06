package firecracker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSandboxRootfsPath(t *testing.T) {
	got := sandboxRootfsPath("/var/sandboxes", "sandbox-id-123456")
	want := filepath.Join("/var/sandboxes", "sandbox-id-123456", "rootfs.ext4")
	if got != want {
		t.Fatalf("sandboxRootfsPath() = %q, want %q", got, want)
	}
}

func TestCloneRootfsSparseCopy(t *testing.T) {
	dir := t.TempDir()
	templatePath := filepath.Join(dir, "template.ext4")
	if err := os.WriteFile(templatePath, []byte("template-rootfs"), 0o644); err != nil {
		t.Fatalf("WriteFile() failed: %v", err)
	}

	destPath := filepath.Join(dir, "sandbox-a", "rootfs.ext4")
	if err := cloneRootfs(context.Background(), templatePath, destPath); err != nil {
		t.Fatalf("cloneRootfs() failed: %v", err)
	}

	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("ReadFile() failed: %v", err)
	}
	if string(got) != "template-rootfs" {
		t.Fatalf("cloned content = %q, want %q", string(got), "template-rootfs")
	}

	info, err := os.Stat(destPath)
	if err != nil {
		t.Fatalf("Stat() failed: %v", err)
	}
	if info.IsDir() {
		t.Fatal("cloned rootfs path is a directory")
	}
}

func TestCloneRootfsCreatesParentDirectory(t *testing.T) {
	dir := t.TempDir()
	templatePath := filepath.Join(dir, "template.ext4")
	if err := os.WriteFile(templatePath, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile() failed: %v", err)
	}

	destPath := filepath.Join(dir, "nested", "sandbox", "rootfs.ext4")
	if err := cloneRootfs(context.Background(), templatePath, destPath); err != nil {
		t.Fatalf("cloneRootfs() failed: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(destPath)); err != nil {
		t.Fatalf("parent dir missing: %v", err)
	}
}

func TestRemoveSandboxDataDir(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "sandbox-id-123456")
	rootfsPath := filepath.Join(dataDir, "rootfs.ext4")
	if err := os.MkdirAll(filepath.Dir(rootfsPath), 0o755); err != nil {
		t.Fatalf("MkdirAll() failed: %v", err)
	}
	if err := os.WriteFile(rootfsPath, []byte("data"), 0o644); err != nil {
		t.Fatalf("WriteFile() failed: %v", err)
	}

	if err := removeSandboxDataDir(context.Background(), dataDir); err != nil {
		t.Fatalf("removeSandboxDataDir() failed: %v", err)
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatalf("data dir still exists: %v", err)
	}
}

func TestRemoveSandboxDataDirNoopForEmptyPath(t *testing.T) {
	if err := removeSandboxDataDir(context.Background(), ""); err != nil {
		t.Fatalf("removeSandboxDataDir() failed: %v", err)
	}
}
