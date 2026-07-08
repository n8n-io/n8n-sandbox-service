package firecracker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	fcnetwork "github.com/n8n-io/sandbox-service/internal/runner/runtime/firecracker.ee/network"
	"github.com/n8n-io/sandbox-service/internal/shellquote"
)

// reconcileOnStartup removes orphaned per-sandbox host state left after a runner
// crash or restart. Sandboxes are not reattached; this matches Docker runner
// reconcile semantics (runner down = sandboxes lost).
func (r *Runtime) reconcileOnStartup(ctx context.Context) {
	if err := r.reconcileSandboxDataDirs(ctx); err != nil {
		slog.Warn("firecracker startup reconcile: sandbox data dirs", "err", err)
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
			return nil
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
	script := startupNetworkCleanupScript(len(r.slots))
	return r.deps.run(ctx, "sudo", "/bin/sh", "-c", script)
}

func startupNetworkCleanupScript(capacity int) string {
	var b strings.Builder
	b.WriteString("set -eu\n")
	for slot := 0; slot < capacity; slot++ {
		netnsName := fmt.Sprintf("fc-sb-%d", slot)
		b.WriteString(strings.TrimSpace(fcnetwork.CleanupScript(netnsName, fcnetwork.HostVethName(slot))))
		b.WriteByte('\n')
	}
	b.WriteString("ip netns list | awk '{print $1}' | grep '^fc-sb-' | xargs -r -n1 ip netns delete 2>/dev/null || true\n")
	return b.String()
}

func reconcileContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 2*time.Minute)
}
