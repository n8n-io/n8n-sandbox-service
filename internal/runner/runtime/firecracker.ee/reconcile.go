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

// startupNetworkCleanupScript deletes every slot netns and host veth by name,
// not by slot number. One line per slot would make the script too long for one
// shell argument on a host with thousands of slots, and names also catch slots
// left over from an earlier, larger slot count. Deleting a netns removes its
// veth pair once nothing holds the namespace; the veth line catches a host end
// whose namespace outlived `ip netns delete` because a process still held it.
func startupNetworkCleanupScript() string {
	return "set -eu\n" +
		"ip netns list | awk '{print $1}' | grep '^fc-sb-' | xargs -r -n1 ip netns delete 2>/dev/null || true\n" +
		"ip -o link show | awk -F': ' '{print $2}' | cut -d@ -f1 | grep '^fc-veth-' | xargs -r -n1 ip link delete 2>/dev/null || true\n"
}

// reapInterruptedSnapshotBuilds kills a golden-snapshot microVM left by a
// runner that exited during the build, and removes that build's jail. New has
// not run yet on that path, so its reconcile cannot be what cleans it up, and
// a start that is stopped in the same place never reaches New either.
func reapInterruptedSnapshotBuilds(ctx context.Context, cfg Config) error {
	return runCommand(ctx, "sudo", "/bin/sh", "-c", snapshotBuildReapScript(cfg.JailerBaseDir))
}

// snapshotBuildReapScript kills fc-snapshot-* netns holders and lazy-unmounts
// the snapshot-* jails under the jailer root before removing those directories.
// Removing a jail fails the script when it still cannot be removed, so startup
// stops instead of continuing with those mounts in place.
func snapshotBuildReapScript(jailerBase string) string {
	root := shellquote.Quote(filepath.Join(jailerBase, "firecracker"))
	// find -exec ... {} +, which fails when the command fails. Neither {} \; nor a
	// pipe into while would: find ignores the result of {} \;, and /bin/sh has no
	// pipefail. Startup would then go on with the mounts still there.
	return "set -eu\n" +
		"for name in $(ip netns list | awk '{print $1}' | grep '^fc-snapshot-' || true); do\n" +
		"  ip netns pids \"$name\" | xargs -r kill -KILL || true\n" +
		"  ip netns delete \"$name\" || true\n" +
		"done\n" +
		"if [ -d " + root + " ]; then\n" +
		"  find " + root + " -mindepth 1 -maxdepth 1 -type d -name 'snapshot-*' -exec sh -c 'set -eu\n" +
		"for jail do\n" +
		"  find \"$jail\" -mindepth 1 -exec umount -l {} \\; || true\n" +
		"  rm -rf \"$jail\"\n" +
		"done\n" +
		"' sh {} +\n" +
		"fi\n"
}

func reconcileContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 2*time.Minute)
}
