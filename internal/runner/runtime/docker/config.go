package docker

import (
	"fmt"
	"net"
	"strings"

	"github.com/n8n-io/sandbox-service/internal/envvar"
	"github.com/n8n-io/sandbox-service/internal/runner/runtime/netpolicy"
)

const (
	defaultHost              = "unix:///var/run/docker.sock"
	defaultMemoryMB          = 512
	defaultCPUPercent        = 100
	defaultPidsMax           = 256
	defaultEnableCgroups     = true
	defaultDiskQuotaInactive = false
)

// Config holds configuration for the Docker/sysbox runtime backend.
type Config struct {
	// Host is the daemon endpoint used to manage sandbox containers.
	// Parsed from SANDBOX_RUNNER_DOCKER_HOST (default unix:///var/run/docker.sock).
	Host string

	// SandboxImage is the image used to start sandboxes.
	// Parsed from SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE.
	SandboxImage string

	// CodeImage is the image used to start code sandboxes. Parsed from
	// SANDBOX_RUNNER_DOCKER_CODE_IMAGE. New requires it when code sandboxes
	// are enabled.
	CodeImage string

	// DefaultMemoryMB is the default memory limit per sandbox in megabytes.
	// Parsed from SANDBOX_RUNNER_DEFAULT_MEMORY_MB (default 512).
	DefaultMemoryMB int64

	// DefaultCPUPercent is the default CPU limit as a percentage of one core.
	// Parsed from SANDBOX_RUNNER_DEFAULT_CPU_PERCENT (default 100).
	DefaultCPUPercent int

	// DefaultPidsMax is the default max process count per sandbox.
	// Parsed from SANDBOX_RUNNER_DEFAULT_PIDS_MAX (default 256).
	DefaultPidsMax int

	// DefaultDiskQuotaMB is the default writable-layer disk quota in megabytes.
	// Parsed from SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB (default 0, meaning no quota).
	// Only applied when DiskQuotaActive is true.
	DefaultDiskQuotaMB int64

	// DiskQuotaActive indicates that the runner's inner dockerd is configured
	// against an xfs+prjquota data root and can honor `--storage-opt size=`.
	// Set by scripts/start-runner.sh after a successful storage-pool mount.
	// Parsed from SANDBOX_RUNNER_DISK_QUOTA_ACTIVE (default false).
	DiskQuotaActive bool

	// EnableCgroups controls whether cgroup setup is enforced for sandbox creation.
	// Parsed from SANDBOX_RUNNER_ENABLE_CGROUPS (default true).
	EnableCgroups bool

	// DNSServers, when set, are the only resolvers sandbox DNS is forwarded to.
	// Unset, Docker forwards to the runner's own resolver. Parsed from
	// SANDBOX_RUNNER_DOCKER_DNS_SERVERS (comma-separated IPv4, default unset).
	DNSServers []string

	// AckBridgeNetfilterLoaded is the operator's statement that the host has
	// br_netfilter loaded with bridge-nf-call-iptables=1, for kernels before 5.3
	// where the runner cannot read the sysctl. No effect on newer kernels.
	// Parsed from SANDBOX_RUNNER_DOCKER_ACK_BR_NETFILTER_IS_LOADED (default false).
	AckBridgeNetfilterLoaded bool
}

const envAckBridgeNetfilter = "SANDBOX_RUNNER_DOCKER_ACK_BR_NETFILTER_IS_LOADED"

func defaultConfig() Config {
	return Config{
		Host:              defaultHost,
		DefaultMemoryMB:   defaultMemoryMB,
		DefaultCPUPercent: defaultCPUPercent,
		DefaultPidsMax:    defaultPidsMax,
		DiskQuotaActive:   defaultDiskQuotaInactive,
		EnableCgroups:     defaultEnableCgroups,
	}
}

// LoadConfig reads Docker runtime configuration from environment variables.
func LoadConfig() (Config, error) {
	cfg := defaultConfig()

	envvar.String("SANDBOX_RUNNER_DOCKER_HOST", &cfg.Host)

	cfg.SandboxImage = envvar.Get("SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE")
	if cfg.SandboxImage == "" {
		return Config{}, fmt.Errorf("SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE must be set")
	}
	cfg.CodeImage = envvar.Get("SANDBOX_RUNNER_DOCKER_CODE_IMAGE")

	if err := envvar.PositiveInt("SANDBOX_RUNNER_DEFAULT_MEMORY_MB", &cfg.DefaultMemoryMB); err != nil {
		return Config{}, err
	}
	if err := envvar.PositiveInt("SANDBOX_RUNNER_DEFAULT_CPU_PERCENT", &cfg.DefaultCPUPercent); err != nil {
		return Config{}, err
	}
	if err := envvar.PositiveInt("SANDBOX_RUNNER_DEFAULT_PIDS_MAX", &cfg.DefaultPidsMax); err != nil {
		return Config{}, err
	}
	if err := envvar.NonNegativeInt("SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB", &cfg.DefaultDiskQuotaMB); err != nil {
		return Config{}, err
	}
	if err := envvar.Bool("SANDBOX_RUNNER_DISK_QUOTA_ACTIVE", &cfg.DiskQuotaActive); err != nil {
		return Config{}, err
	}
	if err := envvar.Bool("SANDBOX_RUNNER_ENABLE_CGROUPS", &cfg.EnableCgroups); err != nil {
		return Config{}, err
	}
	if err := envvar.Bool(envAckBridgeNetfilter, &cfg.AckBridgeNetfilterLoaded); err != nil {
		return Config{}, err
	}

	if v := envvar.Get("SANDBOX_RUNNER_DOCKER_DNS_SERVERS"); v != "" {
		servers, err := parseDNSServers(v)
		if err != nil {
			return Config{}, fmt.Errorf("SANDBOX_RUNNER_DOCKER_DNS_SERVERS: %w", err)
		}
		cfg.DNSServers = servers
	}

	return cfg, nil
}

// parseDNSServers rejects a server inside the egress denylist: queries to an
// explicit server leave from the sandbox, so one there would leave every
// sandbox without DNS.
func parseDNSServers(v string) ([]string, error) {
	var servers []string
	for s := range strings.SplitSeq(v, ",") {
		s = strings.TrimSpace(s)
		ip := net.ParseIP(s)
		if ip == nil || ip.To4() == nil || ip.IsUnspecified() {
			return nil, fmt.Errorf("%q is not a usable IPv4 address", s)
		}
		for _, cidr := range netpolicy.PrivateRangesV4 {
			if _, blocked, err := net.ParseCIDR(cidr); err == nil && blocked.Contains(ip) {
				return nil, fmt.Errorf("%s is in the blocked range %s, so sandboxes could not reach it", s, cidr)
			}
		}
		servers = append(servers, ip.To4().String())
	}
	return servers, nil
}
