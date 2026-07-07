package firecracker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/n8n-io/sandbox-service/internal/shellquote"
)

func sandboxDataDir(dataDir, sandboxID string) string {
	return filepath.Join(dataDir, sandboxID)
}

func sandboxRootfsPath(dataDir, sandboxID string) string {
	return filepath.Join(sandboxDataDir(dataDir, sandboxID), "rootfs.ext4")
}

func sandboxSnapshotMemPath(dataDir string) string {
	return filepath.Join(dataDir, "snapshot_mem")
}

func sandboxSnapshotStatePath(dataDir string) string {
	return filepath.Join(dataDir, "snapshot_state")
}

// cloneSparseFile copies a host file using sparse-aware cp when available.
func cloneSparseFile(ctx context.Context, templatePath, destPath string) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("create parent dir: %w", err)
	}
	script := fmt.Sprintf(`
set -eu
if cp --reflink=auto --sparse=always %s %s 2>/dev/null; then
  exit 0
fi
if cp --sparse=always %s %s 2>/dev/null; then
  exit 0
fi
cp %s %s
`, shellquote.Quote(templatePath), shellquote.Quote(destPath), shellquote.Quote(templatePath), shellquote.Quote(destPath), shellquote.Quote(templatePath), shellquote.Quote(destPath))
	return runCommand(ctx, "/bin/sh", "-c", script)
}

// cloneRootfs copies the golden template rootfs to a per-sandbox writable file.
func cloneRootfs(ctx context.Context, templatePath, destPath string) error {
	return cloneSparseFile(ctx, templatePath, destPath)
}

// cloneGoldenSnapshotAssets copies the host golden snapshot files into a
// per-sandbox data directory. Firecracker snapshot/create writes in place to
// the bind-mounted mem/state paths; sharing the golden files would let one
// sandbox's stop overwrite the template used by every other sandbox.
func cloneGoldenSnapshotAssets(ctx context.Context, goldenMemPath, goldenStatePath, dataDir string) error {
	if err := cloneSparseFile(ctx, goldenMemPath, sandboxSnapshotMemPath(dataDir)); err != nil {
		return fmt.Errorf("clone snapshot mem: %w", err)
	}
	if err := cloneSparseFile(ctx, goldenStatePath, sandboxSnapshotStatePath(dataDir)); err != nil {
		return fmt.Errorf("clone snapshot state: %w", err)
	}
	return nil
}

// removeSandboxDataDir deletes the per-sandbox data directory.
func removeSandboxDataDir(ctx context.Context, dataDir string) error {
	if dataDir == "" {
		return nil
	}
	return runCommand(ctx, "rm", "-rf", dataDir)
}
