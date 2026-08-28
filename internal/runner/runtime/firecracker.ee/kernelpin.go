package firecracker

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// templateKernelPath is the host artifact prepareJail bind-mounts at the jail
// kernel path the sandbox's sidecar records. One file, shared by every sandbox on
// the host, bound rather than copied — so no sandbox owns a private copy of it.
func templateKernelPath(cfg Config) string {
	return filepath.Join(cfg.TemplateDir, "vmlinux")
}

// kernelPin identifies the template kernel a sandbox was reserved against.
//
// The sidecar pins the path Firecracker opens inside the jail, not the file bound
// at it, and prepareJail resolves that file from TemplateDir afresh on every
// activation — the one that cold boots a crashed sandbox included. That leaves the
// kernel as the only boot input which does not travel with the sandbox, and this is
// what closes it.
//
// Size and modification time rather than a checksum: this is captured on the create
// path, for a file tens of megabytes wide, to answer only whether that file is still
// the one the sandbox was reserved against. build-rootfs-template.sh replaces the
// template wholesale, so a rebuild moves both; one that moved neither installed the
// same kernel again.
type kernelPin struct {
	size    int64
	modTime time.Time
}

func statTemplateKernel(path string) (kernelPin, error) {
	info, err := os.Stat(path)
	if err != nil {
		return kernelPin{}, fmt.Errorf("stat template kernel: %w", err)
	}
	return kernelPin{size: info.Size(), modTime: info.ModTime()}, nil
}

func (p kernelPin) String() string {
	return fmt.Sprintf("%d bytes modified %s", p.size, p.modTime.UTC().Format(time.RFC3339Nano))
}

// verifyKernelPin refuses a cold boot on a kernel that is not the one the sandbox
// was reserved against.
//
// A cold boot is the only path that reads the kernel at all — a restore takes it
// from the memory image — and it replays boot arguments against a rootfs from the
// sandbox's own lineage. Pairing those with a rebuilt kernel is the one mismatch
// nothing downstream can report: the guest either comes up or does not, and one that
// comes up far enough to answer a probe is served to the client as recovered.
//
// A new kernel is meant to reach a host by replacing the runner, so this guards an
// in-place template rebuild under a live one rather than recovering from it. What
// failing costs is the recovery and nothing beyond it: the sandbox stays as the
// crash left it, stopped with its rootfs on disk.
func (r *Runtime) verifyKernelPin(pinned kernelPin) error {
	path := templateKernelPath(r.config)
	current, err := r.deps.statTemplateKernel(path)
	if err != nil {
		return err
	}
	if pinned.size != current.size || !pinned.modTime.Equal(current.modTime) {
		return fmt.Errorf("template kernel %s was replaced after this sandbox was created (reserved against %s, now %s), so cold booting it would pair a rebuilt kernel with the rootfs and boot arguments of the build this sandbox belongs to; roll a kernel out by replacing the runner instead of rebuilding the template under a running one", path, pinned, current)
	}
	return nil
}
