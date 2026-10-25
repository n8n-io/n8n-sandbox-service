package docker

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"

	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
	"github.com/n8n-io/sandbox-service/internal/runner/runtime/netpolicy"
)

const (
	defaultHost              = "unix:///var/run/docker.sock"
	defaultMemoryMB          = 512
	defaultCPUPercent        = 100
	defaultPidsMax           = 256
	defaultEnableCgroups     = true
	defaultDiskQuotaInactive = false
	defaultEgressProxyPort   = 3128
)

// Config holds configuration for the Docker/sysbox runtime backend.
type Config struct {
	// Host is the daemon endpoint used to manage sandbox containers.
	// Parsed from SANDBOX_RUNNER_DOCKER_HOST (default unix:///var/run/docker.sock).
	Host string

	// SandboxImage is the image used to start sandboxes.
	// Parsed from SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE.
	SandboxImage string

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

	// AllowUnenforcedDiskQuota lets the runner start when a disk quota is
	// configured but DiskQuotaActive is false; the quota is then not applied.
	// Parsed from SANDBOX_RUNNER_ALLOW_UNENFORCED_DISK_QUOTA (default false).
	AllowUnenforcedDiskQuota bool

	// EnableCgroups controls whether cgroup setup is enforced for sandbox creation.
	// Parsed from SANDBOX_RUNNER_ENABLE_CGROUPS (default true).
	EnableCgroups bool

	// DNSServers, when set, are the only resolvers sandbox DNS is forwarded to.
	// Unset, Docker forwards to the runner's own resolver. Parsed from
	// SANDBOX_RUNNER_DOCKER_DNS_SERVERS (comma-separated IPv4, default unset).
	DNSServers []string

	// Templates are the named images served besides the default (the image
	// and limits above). Parsed from SANDBOX_RUNNER_DOCKER_TEMPLATES, a JSON
	// object of name to Template.
	Templates map[string]Template

	// EgressProxyPort is the port of the egress proxy on the proxy network's
	// gateway. Parsed from SANDBOX_EGRESS_PROXY_PORT (default 3128).
	EgressProxyPort int
}

// Template is a named sandbox image with its own limits; a limit left 0 is
// the default one.
type Template struct {
	Image       string `json:"image"`
	MemoryMB    int64  `json:"memory_mb,omitempty"`
	CPUPercent  int    `json:"cpu_percent,omitempty"`
	PidsMax     int    `json:"pids_max,omitempty"`
	DiskQuotaMB int64  `json:"disk_quota_mb,omitempty"`
}

// parseTemplates decodes SANDBOX_RUNNER_DOCKER_TEMPLATES strictly.
func parseTemplates(v string) (map[string]Template, error) {
	dec := json.NewDecoder(strings.NewReader(v))
	dec.DisallowUnknownFields()
	var out map[string]Template
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("trailing data")
	}
	for name, t := range out {
		if err := runnerruntime.ValidateTemplateName(name); err != nil {
			return nil, err
		}
		if name == runnerruntime.DefaultTemplate {
			return nil, fmt.Errorf("%q is set by SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE and the SANDBOX_RUNNER_DEFAULT_* limits", name)
		}
		if strings.TrimSpace(t.Image) == "" {
			return nil, fmt.Errorf("template %q: image must be set", name)
		}
		if t.MemoryMB < 0 || t.CPUPercent < 0 || t.PidsMax < 0 || t.DiskQuotaMB < 0 {
			return nil, fmt.Errorf("template %q: limits must not be negative", name)
		}
	}
	return out, nil
}

func defaultConfig() Config {
	return Config{
		Host:              defaultHost,
		DefaultMemoryMB:   defaultMemoryMB,
		DefaultCPUPercent: defaultCPUPercent,
		DefaultPidsMax:    defaultPidsMax,
		DiskQuotaActive:   defaultDiskQuotaInactive,
		EnableCgroups:     defaultEnableCgroups,
		EgressProxyPort:   defaultEgressProxyPort,
	}
}

// LoadConfig reads Docker runtime configuration from environment variables.
func LoadConfig() (Config, error) {
	cfg := defaultConfig()

	if v := os.Getenv("SANDBOX_RUNNER_DOCKER_HOST"); v != "" {
		cfg.Host = v
	}

	cfg.SandboxImage = os.Getenv("SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE")
	if cfg.SandboxImage == "" {
		return Config{}, fmt.Errorf("SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE must be set")
	}

	if v := os.Getenv("SANDBOX_RUNNER_DEFAULT_MEMORY_MB"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("SANDBOX_RUNNER_DEFAULT_MEMORY_MB must be a positive integer, got %q", v)
		}
		cfg.DefaultMemoryMB = n
	}

	if v := os.Getenv("SANDBOX_RUNNER_DEFAULT_CPU_PERCENT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("SANDBOX_RUNNER_DEFAULT_CPU_PERCENT must be a positive integer, got %q", v)
		}
		cfg.DefaultCPUPercent = n
	}

	if v := os.Getenv("SANDBOX_RUNNER_DEFAULT_PIDS_MAX"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("SANDBOX_RUNNER_DEFAULT_PIDS_MAX must be a positive integer, got %q", v)
		}
		cfg.DefaultPidsMax = n
	}

	if v := os.Getenv("SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return Config{}, fmt.Errorf("SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB must be a non-negative integer, got %q", v)
		}
		cfg.DefaultDiskQuotaMB = n
	}

	if v := os.Getenv("SANDBOX_RUNNER_DISK_QUOTA_ACTIVE"); v != "" {
		active, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("SANDBOX_RUNNER_DISK_QUOTA_ACTIVE must be a boolean, got %q", v)
		}
		cfg.DiskQuotaActive = active
	}

	if v := os.Getenv("SANDBOX_RUNNER_ALLOW_UNENFORCED_DISK_QUOTA"); v != "" {
		allow, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("SANDBOX_RUNNER_ALLOW_UNENFORCED_DISK_QUOTA must be a boolean, got %q", v)
		}
		cfg.AllowUnenforcedDiskQuota = allow
	}

	if v := os.Getenv("SANDBOX_RUNNER_ENABLE_CGROUPS"); v != "" {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("SANDBOX_RUNNER_ENABLE_CGROUPS must be a boolean, got %q", v)
		}
		cfg.EnableCgroups = enabled
	}

	if v := os.Getenv("SANDBOX_RUNNER_DOCKER_DNS_SERVERS"); v != "" {
		servers, err := parseDNSServers(v)
		if err != nil {
			return Config{}, fmt.Errorf("SANDBOX_RUNNER_DOCKER_DNS_SERVERS: %w", err)
		}
		cfg.DNSServers = servers
	}

	if v := strings.TrimSpace(os.Getenv("SANDBOX_EGRESS_PROXY_PORT")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65535 {
			return Config{}, fmt.Errorf("SANDBOX_EGRESS_PROXY_PORT must be a port number, got %q", v)
		}
		cfg.EgressProxyPort = n
	}
	if v := strings.TrimSpace(os.Getenv("SANDBOX_RUNNER_DOCKER_TEMPLATES")); v != "" {
		templates, err := parseTemplates(v)
		if err != nil {
			return Config{}, fmt.Errorf("SANDBOX_RUNNER_DOCKER_TEMPLATES: %w", err)
		}
		cfg.Templates = templates
	}

	if name := cfg.unenforcedDiskQuota(); name != "" && !cfg.AllowUnenforcedDiskQuota {
		return Config{}, fmt.Errorf("a disk quota is configured for %s but the runner cannot enforce it (the xfs+prjquota storage pool is not active); fix the storage or set SANDBOX_RUNNER_ALLOW_UNENFORCED_DISK_QUOTA=true to run without quotas", name)
	}

	return cfg, nil
}

// unenforcedDiskQuota names the first template with a disk quota that the
// runner cannot enforce, or "" when there is none.
func (c Config) unenforcedDiskQuota() string {
	if c.DiskQuotaActive {
		return ""
	}
	if c.DefaultDiskQuotaMB > 0 {
		return "the default template"
	}
	names := make([]string, 0, len(c.Templates))
	for name, t := range c.Templates {
		if t.DiskQuotaMB > 0 {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) > 0 {
		return fmt.Sprintf("template %q", names[0])
	}
	return ""
}

// parseDNSServers rejects a server inside the egress denylist: queries to an
// explicit server leave from the sandbox, so one there would leave every
// sandbox without DNS.
func parseDNSServers(v string) ([]string, error) {
	var servers []string
	for _, s := range strings.Split(v, ",") {
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
