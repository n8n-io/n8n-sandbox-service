package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	defaultRunnerCapacityTotal int32 = 1000

	defaultIdleTTLSeconds = 3600
	defaultMaxFileBytes   = 10 * 1024 * 1024 // 10 MB
	defaultDataDir        = "/var/sandboxes"
	defaultListenAddr     = ":8080"
	defaultMemoryMB       = 512
	defaultCPUPercent     = 100
	defaultPidsMax        = 256
	defaultEnableCgroups  = true
	defaultDockerHost     = "unix:///var/run/docker.sock"
)

// Config holds all runtime configuration parsed from environment variables.
type Config struct {
	// APIKeys is the set of valid API keys for authenticating requests.
	// Parsed from SANDBOX_RUNNER_API_KEYS (comma-separated).
	APIKeys map[string]struct{}

	// IdleTTLSeconds is how long a sandbox may be idle before it is reaped.
	// Parsed from SANDBOX_RUNNER_IDLE_TTL_SECONDS (default 3600).
	IdleTTLSeconds int

	// MaxFileBytes is the maximum size of a single file that may be written
	// into a sandbox. Parsed from SANDBOX_RUNNER_MAX_FILE_BYTES (default 10 MB).
	MaxFileBytes int64

	// DataDir is the directory under which per-sandbox data is stored.
	// Parsed from SANDBOX_RUNNER_DATA_DIR (default /var/sandboxes).
	DataDir string

	// ListenAddr is the TCP address the HTTP server listens on.
	// Parsed from SANDBOX_RUNNER_LISTEN_ADDR (default :8080).
	ListenAddr string

	// DockerHost is the daemon endpoint used to manage sandbox containers.
	// Parsed from SANDBOX_RUNNER_DOCKER_HOST (default unix:///var/run/docker.sock).
	DockerHost string

	// DockerSandboxImage is the image used to start sandboxes.
	// Parsed from SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE.
	DockerSandboxImage string

	// DefaultMemoryMB is the default memory limit per sandbox in megabytes.
	// Parsed from SANDBOX_RUNNER_DEFAULT_MEMORY_MB (default 512).
	DefaultMemoryMB int64

	// DefaultCPUPercent is the default CPU limit as a percentage of one core.
	// Parsed from SANDBOX_RUNNER_DEFAULT_CPU_PERCENT (default 100).
	DefaultCPUPercent int

	// DefaultPidsMax is the default max process count per sandbox.
	// Parsed from SANDBOX_RUNNER_DEFAULT_PIDS_MAX (default 256).
	DefaultPidsMax int

	// EnableCgroups controls whether cgroup setup is enforced for sandbox creation.
	// Parsed from SANDBOX_RUNNER_ENABLE_CGROUPS (default true).
	EnableCgroups bool

	// InterSandboxNetworkEnabled enables sandbox-to-sandbox traffic on runner-bridge.
	// Parsed from SANDBOX_RUNNER_INTER_SANDBOX_NETWORK_ENABLED (default false).
	InterSandboxNetworkEnabled bool

	// APIGRPCAddr is the host:port of the API's runner registration gRPC listener.
	// Parsed from SANDBOX_RUNNER_API_GRPC_ADDR. When empty, registration is disabled.
	APIGRPCAddr string

	// RegistrationToken authenticates this runner to the API gRPC service.
	// Parsed from SANDBOX_RUNNER_REGISTRATION_TOKEN.
	RegistrationToken string

	// RunnerID is the stable ID advertised to the API (placement / persistence).
	// Parsed from SANDBOX_RUNNER_ID (default: machine hostname).
	RunnerID string

	// RunnerHTTPBaseURL is the base URL the API uses to reach this runner's HTTP API.
	// Parsed from SANDBOX_RUNNER_HTTP_BASE_URL (required when SANDBOX_RUNNER_API_GRPC_ADDR is set).
	RunnerHTTPBaseURL string

	// CapacityTotal is reported to the API for placement (0 means unlimited).
	// Parsed from SANDBOX_RUNNER_CAPACITY_TOTAL (default 1000).
	CapacityTotal int32
}

// Load reads configuration from environment variables and returns a Config.
// It returns an error if any required variable is missing or malformed.
func Load() (*Config, error) {
	cfg := &Config{
		IdleTTLSeconds:    defaultIdleTTLSeconds,
		MaxFileBytes:      defaultMaxFileBytes,
		DataDir:           defaultDataDir,
		ListenAddr:        defaultListenAddr,
		DockerHost:        defaultDockerHost,
		DefaultMemoryMB:   defaultMemoryMB,
		DefaultCPUPercent: defaultCPUPercent,
		DefaultPidsMax:    defaultPidsMax,
		EnableCgroups:     defaultEnableCgroups,
		CapacityTotal:     defaultRunnerCapacityTotal,
	}

	if h, err := os.Hostname(); err == nil && h != "" {
		cfg.RunnerID = h
	}

	// SANDBOX_RUNNER_API_KEYS (required)
	rawKeys := os.Getenv("SANDBOX_RUNNER_API_KEYS")
	if rawKeys == "" {
		return nil, fmt.Errorf("SANDBOX_RUNNER_API_KEYS must be set")
	}
	cfg.APIKeys = make(map[string]struct{})
	for _, k := range strings.Split(rawKeys, ",") {
		k = strings.TrimSpace(k)
		if k != "" {
			cfg.APIKeys[k] = struct{}{}
		}
	}
	if len(cfg.APIKeys) == 0 {
		return nil, fmt.Errorf("SANDBOX_RUNNER_API_KEYS contains no valid keys")
	}

	// SANDBOX_RUNNER_IDLE_TTL_SECONDS (optional)
	if v := os.Getenv("SANDBOX_RUNNER_IDLE_TTL_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("SANDBOX_RUNNER_IDLE_TTL_SECONDS must be a positive integer, got %q", v)
		}
		cfg.IdleTTLSeconds = n
	}

	// SANDBOX_RUNNER_MAX_FILE_BYTES (optional)
	if v := os.Getenv("SANDBOX_RUNNER_MAX_FILE_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("SANDBOX_RUNNER_MAX_FILE_BYTES must be a positive integer, got %q", v)
		}
		cfg.MaxFileBytes = n
	}

	// SANDBOX_RUNNER_DATA_DIR (optional)
	if v := os.Getenv("SANDBOX_RUNNER_DATA_DIR"); v != "" {
		cfg.DataDir = v
	}

	// SANDBOX_RUNNER_LISTEN_ADDR (optional)
	if v := os.Getenv("SANDBOX_RUNNER_LISTEN_ADDR"); v != "" {
		cfg.ListenAddr = v
	}

	// SANDBOX_RUNNER_DOCKER_HOST (optional)
	if v := os.Getenv("SANDBOX_RUNNER_DOCKER_HOST"); v != "" {
		cfg.DockerHost = v
	}

	// SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE (required)
	cfg.DockerSandboxImage = os.Getenv("SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE")
	if cfg.DockerSandboxImage == "" {
		return nil, fmt.Errorf("SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE must be set")
	}

	// SANDBOX_RUNNER_DEFAULT_MEMORY_MB (optional)
	if v := os.Getenv("SANDBOX_RUNNER_DEFAULT_MEMORY_MB"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("SANDBOX_RUNNER_DEFAULT_MEMORY_MB must be a positive integer, got %q", v)
		}
		cfg.DefaultMemoryMB = n
	}

	// SANDBOX_RUNNER_DEFAULT_CPU_PERCENT (optional)
	if v := os.Getenv("SANDBOX_RUNNER_DEFAULT_CPU_PERCENT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("SANDBOX_RUNNER_DEFAULT_CPU_PERCENT must be a positive integer, got %q", v)
		}
		cfg.DefaultCPUPercent = n
	}

	// SANDBOX_RUNNER_DEFAULT_PIDS_MAX (optional)
	if v := os.Getenv("SANDBOX_RUNNER_DEFAULT_PIDS_MAX"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("SANDBOX_RUNNER_DEFAULT_PIDS_MAX must be a positive integer, got %q", v)
		}
		cfg.DefaultPidsMax = n
	}

	// SANDBOX_RUNNER_ENABLE_CGROUPS (optional)
	if v := os.Getenv("SANDBOX_RUNNER_ENABLE_CGROUPS"); v != "" {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("SANDBOX_RUNNER_ENABLE_CGROUPS must be a boolean, got %q", v)
		}
		cfg.EnableCgroups = enabled
	}

	// SANDBOX_RUNNER_INTER_SANDBOX_NETWORK_ENABLED (optional)
	if v := os.Getenv("SANDBOX_RUNNER_INTER_SANDBOX_NETWORK_ENABLED"); v != "" {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("SANDBOX_RUNNER_INTER_SANDBOX_NETWORK_ENABLED must be a boolean, got %q", v)
		}
		cfg.InterSandboxNetworkEnabled = enabled
	}

	cfg.APIGRPCAddr = strings.TrimSpace(os.Getenv("SANDBOX_RUNNER_API_GRPC_ADDR"))
	cfg.RegistrationToken = strings.TrimSpace(os.Getenv("SANDBOX_RUNNER_REGISTRATION_TOKEN"))
	cfg.RunnerHTTPBaseURL = strings.TrimSpace(os.Getenv("SANDBOX_RUNNER_HTTP_BASE_URL"))

	if v := os.Getenv("SANDBOX_RUNNER_ID"); strings.TrimSpace(v) != "" {
		cfg.RunnerID = strings.TrimSpace(v)
	}

	if v := os.Getenv("SANDBOX_RUNNER_CAPACITY_TOTAL"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("SANDBOX_RUNNER_CAPACITY_TOTAL must be a non-negative integer, got %q", v)
		}
		cfg.CapacityTotal = int32(n)
	}

	if cfg.APIGRPCAddr != "" {
		if cfg.RegistrationToken == "" {
			return nil, fmt.Errorf("SANDBOX_RUNNER_REGISTRATION_TOKEN must be set when SANDBOX_RUNNER_API_GRPC_ADDR is set")
		}
		if cfg.RunnerHTTPBaseURL == "" {
			return nil, fmt.Errorf("SANDBOX_RUNNER_HTTP_BASE_URL must be set when SANDBOX_RUNNER_API_GRPC_ADDR is set")
		}
	}

	return cfg, nil
}
