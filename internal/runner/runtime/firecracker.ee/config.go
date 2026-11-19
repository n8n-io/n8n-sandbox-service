package firecracker

import (
	"fmt"
	"math"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/n8n-io/sandbox-service/internal/envvar"
	fcnetwork "github.com/n8n-io/sandbox-service/internal/runner/runtime/firecracker.ee/network"
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
	defaultVolumeBuildTimeout = 5 * time.Minute
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

	// TemplateDir holds rootfs.ext4 (cloned per sandbox) and vmlinux (bind-mounted, pinned).
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

	// VolumeBuildTimeout is how long a volume builder microVM may run, from its
	// start to its exit. Parsed from SANDBOX_RUNNER_FIRECRACKER_VOLUME_BUILD_TIMEOUT.
	VolumeBuildTimeout time.Duration
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
		VolumeBuildTimeout: defaultVolumeBuildTimeout,
	}
}

// LoadConfig reads Firecracker runtime configuration from environment variables.
func LoadConfig(capacityTotal int32) (Config, error) {
	cfg, err := loadConfigWithoutCapacity()
	if err != nil {
		return Config{}, err
	}
	if err := validateCapacity(cfg, capacityTotal); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// loadConfigWithoutCapacity is LoadConfig for a slot count admission derives later.
func loadConfigWithoutCapacity() (Config, error) {
	cfg, err := parseConfig()
	if err != nil {
		return Config{}, err
	}
	if err := validateConfig(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// parseConfig reads the environment without validating it.
func parseConfig() (Config, error) {
	cfg := defaultConfig()

	envvar.String("SANDBOX_RUNNER_FIRECRACKER_JAILER_BIN", &cfg.JailerBin)
	envvar.String("SANDBOX_RUNNER_FIRECRACKER_BIN", &cfg.FirecrackerBin)
	envvar.String("SANDBOX_RUNNER_FIRECRACKER_JAILER_BASE_DIR", &cfg.JailerBaseDir)
	envvar.String("SANDBOX_RUNNER_FIRECRACKER_TEMPLATE_DIR", &cfg.TemplateDir)
	envvar.String("SANDBOX_RUNNER_FIRECRACKER_SNAPSHOT_MEM_PATH", &cfg.SnapshotMemPath)
	envvar.String("SANDBOX_RUNNER_FIRECRACKER_SNAPSHOT_STATE_PATH", &cfg.SnapshotStatePath)
	envvar.String("SANDBOX_RUNNER_FIRECRACKER_GUEST_IP", &cfg.GuestIP)
	envvar.String("SANDBOX_RUNNER_FIRECRACKER_HOST_TAP_DEVICE_NAME", &cfg.HostTapDeviceName)
	envvar.String("SANDBOX_RUNNER_FIRECRACKER_HOST_TAP_IP_CIDR", &cfg.HostTapIPCIDR)
	envvar.String("SANDBOX_RUNNER_FIRECRACKER_PROXY_LISTEN_IP", &cfg.ProxyListenIP)
	envvar.String("SANDBOX_RUNNER_FIRECRACKER_MANIFEST_PATH", &cfg.ManifestPath)
	envvar.String("SANDBOX_RUNNER_FIRECRACKER_EXPECTED_GIT_SHA", &cfg.ExpectedGitSHA)
	envvar.String("SANDBOX_RUNNER_FIRECRACKER_CREATE_SNAPSHOT_SCRIPT", &cfg.CreateSnapshotScript)
	envvar.String("SANDBOX_RUNNER_FIRECRACKER_DAEMON_BIN", &cfg.DaemonBin)

	if err := envvar.PositiveInt("SANDBOX_RUNNER_FIRECRACKER_DAEMON_PORT", &cfg.DaemonPort); err != nil {
		return Config{}, err
	}
	if err := envvar.PositiveInt("SANDBOX_RUNNER_FIRECRACKER_PROXY_PORT_START", &cfg.ProxyPortStart); err != nil {
		return Config{}, err
	}
	if err := envvar.PositiveInt("SANDBOX_RUNNER_FIRECRACKER_SOCKET_WAIT_ATTEMPTS", &cfg.SocketWaitAttempts); err != nil {
		return Config{}, err
	}
	// Capped so the conversion below cannot overflow into a negative interval.
	socketWaitMs := cfg.SocketWaitInterval.Milliseconds()
	if err := envvar.IntBetween("SANDBOX_RUNNER_FIRECRACKER_SOCKET_WAIT_INTERVAL_MS", &socketWaitMs, 1, math.MaxInt64/int64(time.Millisecond)); err != nil {
		return Config{}, err
	}
	cfg.SocketWaitInterval = time.Duration(socketWaitMs) * time.Millisecond
	if err := envvar.PositiveDuration("SANDBOX_RUNNER_FIRECRACKER_DAEMON_WAIT_TIMEOUT", &cfg.DaemonWaitTimeout); err != nil {
		return Config{}, err
	}
	if err := envvar.PositiveDuration("SANDBOX_RUNNER_FIRECRACKER_VOLUME_BUILD_TIMEOUT", &cfg.VolumeBuildTimeout); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func validateConfig(cfg Config) error {
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

// validateCapacity checks a set slot count against the structural ceilings.
func validateCapacity(cfg Config, capacityTotal int32) error {
	if capacityTotal <= 0 {
		return fmt.Errorf("SANDBOX_RUNNER_CAPACITY_TOTAL must be positive for firecracker backend")
	}
	if int64(cfg.ProxyPortStart)+int64(capacityTotal)-1 > 65535 {
		return fmt.Errorf("firecracker proxy port range starting at %d exceeds 65535 for capacity %d", cfg.ProxyPortStart, capacityTotal)
	}
	if int64(capacityTotal) > fcnetwork.MaxSlots {
		return fmt.Errorf("SANDBOX_RUNNER_CAPACITY_TOTAL %d exceeds the %d slots the firecracker uplink addressing covers", capacityTotal, fcnetwork.MaxSlots)
	}
	return nil
}

// snapshotDirsMatch reports whether mem and state paths share a parent directory.
// Required for auto-create: the script emits both files into one --out dir and the
// runner symlinks the configured names to snapshot_mem/snapshot_state there.
func snapshotDirsMatch(memPath, statePath string) bool {
	return filepath.Clean(filepath.Dir(memPath)) == filepath.Clean(filepath.Dir(statePath))
}
