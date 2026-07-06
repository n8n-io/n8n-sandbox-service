package firecracker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

func sandboxDataDir(dataDir, sandboxID string) string {
	return filepath.Join(dataDir, sandboxID)
}

func sandboxRootfsPath(dataDir, sandboxID string) string {
	return filepath.Join(sandboxDataDir(dataDir, sandboxID), "rootfs.ext4")
}

// cloneRootfs copies the golden template rootfs to a per-sandbox writable file.
// It prefers reflink+sparse copy when the filesystem supports it.
func cloneRootfs(ctx context.Context, templatePath, destPath string) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("create sandbox data dir: %w", err)
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
`, shellQuote(templatePath), shellQuote(destPath), shellQuote(templatePath), shellQuote(destPath), shellQuote(templatePath), shellQuote(destPath))
	return runCommand(ctx, "/bin/sh", "-c", script)
}

// removeSandboxDataDir deletes the per-sandbox data directory.
func removeSandboxDataDir(ctx context.Context, dataDir string) error {
	if dataDir == "" {
		return nil
	}
	return runCommand(ctx, "rm", "-rf", dataDir)
}
