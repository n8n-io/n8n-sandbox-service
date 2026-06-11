package docker

import (
	"fmt"
	"os"
	"strconv"
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
}

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

	if v := os.Getenv("SANDBOX_RUNNER_ENABLE_CGROUPS"); v != "" {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("SANDBOX_RUNNER_ENABLE_CGROUPS must be a boolean, got %q", v)
		}
		cfg.EnableCgroups = enabled
	}

	return cfg, nil
}
