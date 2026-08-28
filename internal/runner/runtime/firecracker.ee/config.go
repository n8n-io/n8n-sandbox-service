package firecracker

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultJailerBin          = "/opt/firecracker/bin/jailer"
	defaultFirecrackerBin     = "/opt/firecracker/bin/firecracker"
	defaultJailerBaseDir      = "/srv/jailer"
	defaultTemplateDir        = "/srv/firecracker/template"
	defaultSnapshotMemPath    = "/srv/firecracker/snapshots/mem"
	defaultSnapshotStatePath  = "/srv/firecracker/snapshots/state"
	defaultGuestIP            = "172.16.0.10"
	defaultHostTapDeviceName  = "fc-tap-0"
	defaultHostTapIPCIDR      = "172.16.0.1/24"
	defaultDaemonPort         = 8081
	defaultProxyListenIP      = "127.0.0.1"
	defaultProxyPortStart     = 18081
	defaultSocketWaitAttempts = 120
	defaultSocketWaitInterval = 20 * time.Millisecond
	defaultDaemonWaitTimeout  = 60 * time.Second
	defaultDaemonBin          = "/srv/firecracker/bin/sandbox-daemon"
)

// Config holds configuration for the Firecracker runtime backend.
type Config struct {
	// JailerBin is the path to the Firecracker jailer binary.
	// Parsed from SANDBOX_RUNNER_FIRECRACKER_JAILER_BIN.
	JailerBin string

	// FirecrackerBin is the path to the Firecracker VMM binary.
	// Parsed from SANDBOX_RUNNER_FIRECRACKER_BIN.
	FirecrackerBin string

	// JailerBaseDir is passed to jailer --chroot-base-dir.
	// Parsed from SANDBOX_RUNNER_FIRECRACKER_JAILER_BASE_DIR.
	JailerBaseDir string

	// TemplateDir contains rootfs.ext4 used by restored snapshots.
	// Parsed from SANDBOX_RUNNER_FIRECRACKER_TEMPLATE_DIR.
	TemplateDir string

	// SnapshotMemPath is the host path bind-mounted as /snapshot_mem.
	// Parsed from SANDBOX_RUNNER_FIRECRACKER_SNAPSHOT_MEM_PATH.
	SnapshotMemPath string

	// SnapshotStatePath is the host path bind-mounted as /snapshot_state.
	// Parsed from SANDBOX_RUNNER_FIRECRACKER_SNAPSHOT_STATE_PATH.
	// When CreateSnapshotScript is set, must share a directory with SnapshotMemPath.
	SnapshotStatePath string

	// GuestIP is the fixed guest IP expected by the snapshot.
	// Parsed from SANDBOX_RUNNER_FIRECRACKER_GUEST_IP.
	GuestIP string

	// HostTapDeviceName is the tap device name expected inside each netns.
	// Parsed from SANDBOX_RUNNER_FIRECRACKER_HOST_TAP_DEVICE_NAME.
	HostTapDeviceName string

	// HostTapIPCIDR is assigned to the host tap inside each netns.
	// Parsed from SANDBOX_RUNNER_FIRECRACKER_HOST_TAP_IP_CIDR.
	HostTapIPCIDR string

	// DaemonPort is the sandbox daemon port inside the guest.
	// Parsed from SANDBOX_RUNNER_FIRECRACKER_DAEMON_PORT.
	DaemonPort int

	// ProxyListenIP is the host-side IP used for per-sandbox daemon proxies.
	// Parsed from SANDBOX_RUNNER_FIRECRACKER_PROXY_LISTEN_IP.
	ProxyListenIP string

	// ProxyPortStart is the first host-side daemon proxy port.
	// Parsed from SANDBOX_RUNNER_FIRECRACKER_PROXY_PORT_START.
	ProxyPortStart int

	// SocketWaitAttempts controls firecracker.socket polling.
	// Parsed from SANDBOX_RUNNER_FIRECRACKER_SOCKET_WAIT_ATTEMPTS.
	SocketWaitAttempts int

	// SocketWaitInterval controls delay between firecracker.socket polls.
	// Parsed from SANDBOX_RUNNER_FIRECRACKER_SOCKET_WAIT_INTERVAL_MS.
	SocketWaitInterval time.Duration

	// DaemonWaitTimeout controls how long CreateSandbox waits for guest daemon health.
	// Parsed from SANDBOX_RUNNER_FIRECRACKER_DAEMON_WAIT_TIMEOUT.
	DaemonWaitTimeout time.Duration

	// ManifestPath is an optional release MANIFEST.json used to pin guest assets.
	// Parsed from SANDBOX_RUNNER_FIRECRACKER_MANIFEST_PATH.
	ManifestPath string

	// ExpectedGitSHA, when set, must match MANIFEST.json git_sha.
	// Parsed from SANDBOX_RUNNER_FIRECRACKER_EXPECTED_GIT_SHA.
	ExpectedGitSHA string

	// CreateSnapshotScript creates the host-local golden snapshot when mem/state are missing.
	// Empty means do not auto-create (Ready fails until snapshots exist).
	// Parsed from SANDBOX_RUNNER_FIRECRACKER_CREATE_SNAPSHOT_SCRIPT.
	CreateSnapshotScript string

	// DaemonBin is the host path to sandbox-daemon used when creating a golden snapshot.
	// Parsed from SANDBOX_RUNNER_FIRECRACKER_DAEMON_BIN.
	DaemonBin string
}

func defaultConfig() Config {
	return Config{
		JailerBin:          defaultJailerBin,
		FirecrackerBin:     defaultFirecrackerBin,
		JailerBaseDir:      defaultJailerBaseDir,
		TemplateDir:        defaultTemplateDir,
		SnapshotMemPath:    defaultSnapshotMemPath,
		SnapshotStatePath:  defaultSnapshotStatePath,
		GuestIP:            defaultGuestIP,
		HostTapDeviceName:  defaultHostTapDeviceName,
		HostTapIPCIDR:      defaultHostTapIPCIDR,
		DaemonPort:         defaultDaemonPort,
		ProxyListenIP:      defaultProxyListenIP,
		ProxyPortStart:     defaultProxyPortStart,
		SocketWaitAttempts: defaultSocketWaitAttempts,
		SocketWaitInterval: defaultSocketWaitInterval,
		DaemonWaitTimeout:  defaultDaemonWaitTimeout,
		DaemonBin:          defaultDaemonBin,
	}
}

// LoadConfig reads Firecracker runtime configuration from environment variables.
func LoadConfig(capacityTotal int32) (Config, error) {
	cfg := defaultConfig()

	if v := strings.TrimSpace(os.Getenv("SANDBOX_RUNNER_FIRECRACKER_JAILER_BIN")); v != "" {
		cfg.JailerBin = v
	}
	if v := strings.TrimSpace(os.Getenv("SANDBOX_RUNNER_FIRECRACKER_BIN")); v != "" {
		cfg.FirecrackerBin = v
	}
	if v := strings.TrimSpace(os.Getenv("SANDBOX_RUNNER_FIRECRACKER_JAILER_BASE_DIR")); v != "" {
		cfg.JailerBaseDir = v
	}
	if v := strings.TrimSpace(os.Getenv("SANDBOX_RUNNER_FIRECRACKER_TEMPLATE_DIR")); v != "" {
		cfg.TemplateDir = v
	}
	if v := strings.TrimSpace(os.Getenv("SANDBOX_RUNNER_FIRECRACKER_SNAPSHOT_MEM_PATH")); v != "" {
		cfg.SnapshotMemPath = v
	}
	if v := strings.TrimSpace(os.Getenv("SANDBOX_RUNNER_FIRECRACKER_SNAPSHOT_STATE_PATH")); v != "" {
		cfg.SnapshotStatePath = v
	}
	if v := strings.TrimSpace(os.Getenv("SANDBOX_RUNNER_FIRECRACKER_GUEST_IP")); v != "" {
		cfg.GuestIP = v
	}
	if v := strings.TrimSpace(os.Getenv("SANDBOX_RUNNER_FIRECRACKER_HOST_TAP_DEVICE_NAME")); v != "" {
		cfg.HostTapDeviceName = v
	}
	if v := strings.TrimSpace(os.Getenv("SANDBOX_RUNNER_FIRECRACKER_HOST_TAP_IP_CIDR")); v != "" {
		cfg.HostTapIPCIDR = v
	}
	if n, ok, err := parsePositiveIntEnv("SANDBOX_RUNNER_FIRECRACKER_DAEMON_PORT"); err != nil {
		return Config{}, err
	} else if ok {
		cfg.DaemonPort = n
	}
	if v := strings.TrimSpace(os.Getenv("SANDBOX_RUNNER_FIRECRACKER_PROXY_LISTEN_IP")); v != "" {
		cfg.ProxyListenIP = v
	}
	if n, ok, err := parsePositiveIntEnv("SANDBOX_RUNNER_FIRECRACKER_PROXY_PORT_START"); err != nil {
		return Config{}, err
	} else if ok {
		cfg.ProxyPortStart = n
	}
	if n, ok, err := parsePositiveIntEnv("SANDBOX_RUNNER_FIRECRACKER_SOCKET_WAIT_ATTEMPTS"); err != nil {
		return Config{}, err
	} else if ok {
		cfg.SocketWaitAttempts = n
	}
	if d, ok, err := parseMillisecondsEnv("SANDBOX_RUNNER_FIRECRACKER_SOCKET_WAIT_INTERVAL_MS"); err != nil {
		return Config{}, err
	} else if ok {
		cfg.SocketWaitInterval = d
	}
	if d, ok, err := parseDurationEnv("SANDBOX_RUNNER_FIRECRACKER_DAEMON_WAIT_TIMEOUT"); err != nil {
		return Config{}, err
	} else if ok {
		cfg.DaemonWaitTimeout = d
	}
	if v := strings.TrimSpace(os.Getenv("SANDBOX_RUNNER_FIRECRACKER_MANIFEST_PATH")); v != "" {
		cfg.ManifestPath = v
	}
	if v := strings.TrimSpace(os.Getenv("SANDBOX_RUNNER_FIRECRACKER_EXPECTED_GIT_SHA")); v != "" {
		cfg.ExpectedGitSHA = v
	}
	if v := strings.TrimSpace(os.Getenv("SANDBOX_RUNNER_FIRECRACKER_CREATE_SNAPSHOT_SCRIPT")); v != "" {
		cfg.CreateSnapshotScript = v
	}
	if v := strings.TrimSpace(os.Getenv("SANDBOX_RUNNER_FIRECRACKER_DAEMON_BIN")); v != "" {
		cfg.DaemonBin = v
	}

	if err := validateConfig(cfg, capacityTotal); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func parsePositiveIntEnv(name string) (int, bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, false, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, true, fmt.Errorf("%s must be a positive integer, got %q", name, raw)
	}
	return n, true, nil
}

func parseDurationEnv(name string) (time.Duration, bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, false, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, true, fmt.Errorf("%s must be a positive duration, got %q", name, raw)
	}
	return d, true, nil
}

func parseMillisecondsEnv(name string) (time.Duration, bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, false, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, true, fmt.Errorf("%s must be a positive integer number of milliseconds, got %q", name, raw)
	}
	return time.Duration(n) * time.Millisecond, true, nil
}

func validateConfig(cfg Config, capacityTotal int32) error {
	requiredAbs := map[string]string{
		"SANDBOX_RUNNER_FIRECRACKER_JAILER_BIN":          cfg.JailerBin,
		"SANDBOX_RUNNER_FIRECRACKER_BIN":                 cfg.FirecrackerBin,
		"SANDBOX_RUNNER_FIRECRACKER_JAILER_BASE_DIR":     cfg.JailerBaseDir,
		"SANDBOX_RUNNER_FIRECRACKER_TEMPLATE_DIR":        cfg.TemplateDir,
		"SANDBOX_RUNNER_FIRECRACKER_SNAPSHOT_MEM_PATH":   cfg.SnapshotMemPath,
		"SANDBOX_RUNNER_FIRECRACKER_SNAPSHOT_STATE_PATH": cfg.SnapshotStatePath,
	}
	for name, value := range requiredAbs {
		if !strings.HasPrefix(value, "/") {
			return fmt.Errorf("%s must be an absolute path, got %q", name, value)
		}
	}
	if net.ParseIP(cfg.GuestIP) == nil {
		return fmt.Errorf("SANDBOX_RUNNER_FIRECRACKER_GUEST_IP must be an IP address, got %q", cfg.GuestIP)
	}
	if _, _, err := net.ParseCIDR(cfg.HostTapIPCIDR); err != nil {
		return fmt.Errorf("SANDBOX_RUNNER_FIRECRACKER_HOST_TAP_IP_CIDR must be CIDR notation, got %q: %w", cfg.HostTapIPCIDR, err)
	}
	if net.ParseIP(cfg.ProxyListenIP) == nil {
		return fmt.Errorf("SANDBOX_RUNNER_FIRECRACKER_PROXY_LISTEN_IP must be an IP address, got %q", cfg.ProxyListenIP)
	}
	if cfg.DaemonPort <= 0 || cfg.DaemonPort > 65535 {
		return fmt.Errorf("SANDBOX_RUNNER_FIRECRACKER_DAEMON_PORT must be between 1 and 65535, got %d", cfg.DaemonPort)
	}
	if cfg.ProxyPortStart <= 0 || cfg.ProxyPortStart > 65535 {
		return fmt.Errorf("SANDBOX_RUNNER_FIRECRACKER_PROXY_PORT_START must be between 1 and 65535, got %d", cfg.ProxyPortStart)
	}
	if capacityTotal <= 0 {
		return fmt.Errorf("SANDBOX_RUNNER_CAPACITY_TOTAL must be positive for firecracker backend")
	}
	if int64(cfg.ProxyPortStart)+int64(capacityTotal)-1 > 65535 {
		return fmt.Errorf("firecracker proxy port range starting at %d exceeds 65535 for capacity %d", cfg.ProxyPortStart, capacityTotal)
	}
	if cfg.ManifestPath != "" && !strings.HasPrefix(cfg.ManifestPath, "/") {
		return fmt.Errorf("SANDBOX_RUNNER_FIRECRACKER_MANIFEST_PATH must be an absolute path, got %q", cfg.ManifestPath)
	}
	if cfg.CreateSnapshotScript != "" && !strings.HasPrefix(cfg.CreateSnapshotScript, "/") {
		return fmt.Errorf("SANDBOX_RUNNER_FIRECRACKER_CREATE_SNAPSHOT_SCRIPT must be an absolute path, got %q", cfg.CreateSnapshotScript)
	}
	if cfg.CreateSnapshotScript != "" && !snapshotDirsMatch(cfg.SnapshotMemPath, cfg.SnapshotStatePath) {
		return fmt.Errorf("SANDBOX_RUNNER_FIRECRACKER_SNAPSHOT_MEM_PATH (%q) and SANDBOX_RUNNER_FIRECRACKER_SNAPSHOT_STATE_PATH (%q) must be in the same directory when SANDBOX_RUNNER_FIRECRACKER_CREATE_SNAPSHOT_SCRIPT is set; create-golden-snapshot.sh writes snapshot_mem, snapshot_state and boot.json into a single --out directory",
			cfg.SnapshotMemPath, cfg.SnapshotStatePath)
	}
	if cfg.DaemonBin != "" && !strings.HasPrefix(cfg.DaemonBin, "/") {
		return fmt.Errorf("SANDBOX_RUNNER_FIRECRACKER_DAEMON_BIN must be an absolute path, got %q", cfg.DaemonBin)
	}
	return nil
}

// snapshotDirsMatch reports whether mem and state paths share a parent directory.
// Required for auto-create: the script emits both files into one --out dir and the
// runner symlinks the configured names to snapshot_mem/snapshot_state there.
func snapshotDirsMatch(memPath, statePath string) bool {
	return filepath.Clean(filepath.Dir(memPath)) == filepath.Clean(filepath.Dir(statePath))
}
