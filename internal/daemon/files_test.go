package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHandleFileCopyRejectsNestedDestination(t *testing.T) {
	base := tempBaseDir(t)
	srcDir := filepath.Join(base, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write src file: %v", err)
	}

	err := HandleFileCopy(base, "/src", "/src/child", true, false)
	if err == nil {
		t.Fatal("expected error when copying directory into its own subtree")
	}
	if !strings.Contains(err.Error(), "destination must not be inside source directory") {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, statErr := os.Stat(filepath.Join(srcDir, "child")); !os.IsNotExist(statErr) {
		t.Fatalf("destination directory should not be created, stat err: %v", statErr)
	}
}

func TestHandleFileMoveRejectsNestedDestination(t *testing.T) {
	base := tempBaseDir(t)
	srcDir := filepath.Join(base, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write src file: %v", err)
	}

	err := HandleFileMove(base, "/src", "/src/child", false)
	if err == nil {
		t.Fatal("expected error when moving directory into its own subtree")
	}
	if !strings.Contains(err.Error(), "destination must not be inside source directory") {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, statErr := os.Stat(filepath.Join(srcDir, "a.txt")); statErr != nil {
		t.Fatalf("source should remain untouched, stat err: %v", statErr)
	}
}

func tempBaseDir(t *testing.T) string {
	t.Helper()
	base, err := os.MkdirTemp("", "daemon-files-test-")
	if err != nil {
		t.Fatalf("mktemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	// Resolve symlinks so SafeResolve's final prefix check is stable on macOS (/var vs /private/var).
	resolvedBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatalf("eval symlinks: %v", err)
	}
	return resolvedBase
}
