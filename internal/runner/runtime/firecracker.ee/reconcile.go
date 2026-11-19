package firecracker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/n8n-io/sandbox-service/internal/shellquote"
)

// reconcileOnStartup removes orphaned per-sandbox host state left after a runner
// crash or restart. Sandboxes are not reattached; this matches Docker runner
// reconcile semantics (runner down = sandboxes lost).
func (r *Runtime) reconcileOnStartup(ctx context.Context) {
	if err := r.reconcileSandboxDataDirs(ctx); err != nil {
		slog.Warn("firecracker startup reconcile: sandbox data dirs", "err", err)
	}
	// Before the jailer state: that cannot remove a jail with binds in it.
	if err := r.reconcileVolumeBuild(ctx); err != nil {
		slog.Warn("firecracker startup reconcile: volume build", "err", err)
	}
	if err := r.reconcileJailerState(ctx); err != nil {
		slog.Warn("firecracker startup reconcile: jailer state", "err", err)
	}
	if err := r.reconcileNetworkNamespaces(ctx); err != nil {
		slog.Warn("firecracker startup reconcile: network namespaces", "err", err)
	}
}

func (r *Runtime) reconcileSandboxDataDirs(ctx context.Context) error {
	dataDir := r.runnerConfig.DataDir
	if dataDir == "" {
		return nil
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			// Not created here: inside its volume, a missing data dir can mean the
			// volume is not mounted, and creating it would put sandboxes on the disk
			// underneath. Creates fail until it exists.
			return fmt.Errorf("data dir %s does not exist; creates fail until it does", dataDir)
		}
		return fmt.Errorf("read data dir %s: %w", dataDir, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(dataDir, entry.Name())
		if err := removeSandboxDataDir(ctx, path); err != nil {
			slog.Warn("firecracker startup reconcile: remove sandbox data dir", "path", path, "err", err)
		}
	}
	return nil
}

func (r *Runtime) reconcileJailerState(ctx context.Context) error {
	jailerRoot := filepath.Join(r.config.JailerBaseDir, "firecracker")
	if !r.deps.pathExists(jailerRoot) {
		return nil
	}
	script := fmt.Sprintf("set -eu\nrm -rf %s\n", shellquote.Quote(jailerRoot))
	return r.deps.run(ctx, "sudo", "/bin/sh", "-c", script)
}

func (r *Runtime) reconcileNetworkNamespaces(ctx context.Context) error {
	script := startupNetworkCleanupScript()
	return r.deps.run(ctx, "sudo", "/bin/sh", "-c", script)
}

// startupNetworkCleanupScript deletes every slot netns and host veth by name.
// It runs before admission sizes the slots, so it cannot count them; deleting a
// netns removes its veth pair, and the veth line catches a host end left by a
// build that died before moving its peer into the namespace.
func startupNetworkCleanupScript() string {
	return "set -eu\n" +
		"ip netns list | awk '{print $1}' | grep '^fc-sb-' | xargs -r -n1 ip netns delete 2>/dev/null || true\n" +
		"ip -o link show | awk -F': ' '{print $2}' | cut -d@ -f1 | grep '^fc-veth-' | xargs -r -n1 ip link delete 2>/dev/null || true\n"
}

func reconcileContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 2*time.Minute)
}
