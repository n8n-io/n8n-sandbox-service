package firecracker

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

const (
	// bootParamsFileName is written by create-golden-snapshot.sh into the same
	// --out directory as snapshot_mem and snapshot_state.
	bootParamsFileName = "boot.json"

	// bootParamsSchemaVersion is the only sidecar layout this runner accepts.
	// A snapshot written by a newer script is rejected rather than guessed at.
	bootParamsSchemaVersion = 1
)

// bootParams records the machine configuration a golden snapshot was actually
// built with. Memory, vCPUs and the kernel command line are chosen by
// create-golden-snapshot.sh and have no equivalent in the runner's own
// configuration, so without this sidecar a cold boot would have to invent
// them. Recovery replays these values verbatim, which also keeps a sandbox
// pinned to the snapshot lineage it was created from once there are several.
type bootParams struct {
	SchemaVersion     int    `json:"schema_version"`
	VCPUCount         int    `json:"vcpu_count"`
	MemSizeMiB        int    `json:"mem_size_mib"`
	KernelImagePath   string `json:"kernel_image_path"`
	BootArgs          string `json:"boot_args"`
	RootfsDrivePath   string `json:"rootfs_drive_path"`
	GuestMAC          string `json:"guest_mac"`
	GuestIP           string `json:"guest_ip"`
	HostTapDeviceName string `json:"host_tap_device_name"`
	DaemonPort        int    `json:"daemon_port"`
}

// bootParamsPath resolves the sidecar from the golden snapshot's directory,
// which is the --out directory the create script writes all three files into.
func bootParamsPath(cfg Config) string {
	return filepath.Join(filepath.Dir(cfg.SnapshotMemPath), bootParamsFileName)
}

func loadBootParams(path string) (*bootParams, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read golden snapshot boot parameters: %w", err)
	}
	var p bootParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("parse golden snapshot boot parameters %s: %w", path, err)
	}
	if err := p.validate(); err != nil {
		return nil, fmt.Errorf("golden snapshot boot parameters %s: %w", path, err)
	}
	return &p, nil
}

func (p *bootParams) validate() error {
	if p.SchemaVersion != bootParamsSchemaVersion {
		return fmt.Errorf("unsupported schema_version %d, want %d; rebuild the golden snapshot with this release's create-golden-snapshot.sh", p.SchemaVersion, bootParamsSchemaVersion)
	}
	if p.VCPUCount <= 0 {
		return fmt.Errorf("vcpu_count must be positive, got %d", p.VCPUCount)
	}
	if p.MemSizeMiB <= 0 {
		return fmt.Errorf("mem_size_mib must be positive, got %d", p.MemSizeMiB)
	}
	if !strings.HasPrefix(p.KernelImagePath, "/") {
		return fmt.Errorf("kernel_image_path must be an absolute jail path, got %q", p.KernelImagePath)
	}
	if !strings.HasPrefix(p.RootfsDrivePath, "/") {
		return fmt.Errorf("rootfs_drive_path must be an absolute jail path, got %q", p.RootfsDrivePath)
	}
	if strings.TrimSpace(p.BootArgs) == "" {
		return fmt.Errorf("boot_args must not be empty")
	}
	if net.ParseIP(p.GuestIP) == nil {
		return fmt.Errorf("guest_ip must be an IP address, got %q", p.GuestIP)
	}
	if _, err := net.ParseMAC(p.GuestMAC); err != nil {
		return fmt.Errorf("guest_mac must be a MAC address, got %q", p.GuestMAC)
	}
	if strings.TrimSpace(p.HostTapDeviceName) == "" {
		return fmt.Errorf("host_tap_device_name must not be empty")
	}
	if p.DaemonPort <= 0 || p.DaemonPort > 65535 {
		return fmt.Errorf("daemon_port must be between 1 and 65535, got %d", p.DaemonPort)
	}
	return nil
}

// matchesConfig rejects runner settings the snapshot cannot honour. Each of
// these is baked into the guest or its restored device model, so a mismatch is
// not a degraded sandbox but one that never answers: the host proxy dials an
// address or port nothing listens on, or Firecracker cannot attach the NIC the
// snapshot expects. Failing admission turns that into one clear startup error
// instead of every sandbox on the runner timing out.
func (p *bootParams) matchesConfig(cfg Config) error {
	mismatches := make([]string, 0, 3)
	if p.GuestIP != cfg.GuestIP {
		mismatches = append(mismatches, fmt.Sprintf("guest_ip %q != SANDBOX_RUNNER_FIRECRACKER_GUEST_IP %q", p.GuestIP, cfg.GuestIP))
	}
	if p.HostTapDeviceName != cfg.HostTapDeviceName {
		mismatches = append(mismatches, fmt.Sprintf("host_tap_device_name %q != SANDBOX_RUNNER_FIRECRACKER_HOST_TAP_DEVICE_NAME %q", p.HostTapDeviceName, cfg.HostTapDeviceName))
	}
	if p.DaemonPort != cfg.DaemonPort {
		mismatches = append(mismatches, fmt.Sprintf("daemon_port %d != SANDBOX_RUNNER_FIRECRACKER_DAEMON_PORT %d", p.DaemonPort, cfg.DaemonPort))
	}
	if len(mismatches) == 0 {
		return nil
	}
	return fmt.Errorf("golden snapshot was built with settings the runner contradicts (%s); rebuild the snapshot or align the runner configuration", strings.Join(mismatches, "; "))
}
