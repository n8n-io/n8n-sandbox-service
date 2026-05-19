package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultListenAddr      = ":8080"
	defaultGRPCListenAddr  = ":9090"
	defaultMaxFileBytes    = 10 * 1024 * 1024 // 10 MB
	defaultHeartbeatGrace  = 45 * time.Second
	defaultIdleStopAfter   = time.Hour
	defaultIdleDeleteAfter = 24 * time.Hour
)

// APIConfig contains configuration for the public API gateway.
type APIConfig struct {
	// APIKeys is the set of valid API keys for authenticating public requests.
	APIKeys map[string]struct{}

	// ListenAddr is the TCP address the API gateway listens on.
	ListenAddr string

	// GRPCListenAddr is the TCP address for the private runner registration gRPC server.
	GRPCListenAddr string

	// RegistrationToken is the shared secret runners present as Bearer token on the gRPC stream.
	RegistrationToken string

	// MaxFileBytes is the maximum size of file upload requests accepted by API.
	MaxFileBytes int64

	// RunnerAPIKey is the optional API key sent to runner via X-Api-Key.
	RunnerAPIKey string

	// DataDir is the directory for storing API state.
	DataDir string

	// HeartbeatGrace is how long after the last gRPC heartbeat a runner may still be chosen for placement.
	HeartbeatGrace time.Duration

	// EnableCORS enables CORS headers (allow all origins). Default false.
	EnableCORS bool

	// Runner registration gRPC mTLS (required). All three must be set.
	GRPCServerCertFile string
	GRPCServerKeyFile  string
	GRPCClientCAFile   string

	// IdleStopAfter is how long after last activity the API asks the runner to stop
	// the container (0 = disabled).
	IdleStopAfter time.Duration
	// IdleDeleteAfter is how long after last activity the API deletes the sandbox
	// (0 = disabled). Wakes are refused after this window until the row is removed.
	IdleDeleteAfter time.Duration
	// IdleDeleteSafetyBuffer is added to IdleDeleteAfter before deletion (race guard).
	// When IdleDeleteAfter > 0 and this is unset, it defaults to 1m.
	IdleDeleteSafetyBuffer time.Duration
	// IdleSweepInterval is how often the idle stop/delete sweeper runs (default 1m).
	IdleSweepInterval time.Duration

	// API as mTLS client dialing runner SandboxControl (required). All three must be set.
	RunnerControlGRPCClientCAFile     string
	RunnerControlGRPCClientCertFile   string
	RunnerControlGRPCClientKeyFile    string
	RunnerControlGRPCClientServerName string // optional; defaults to runner dial host
}

// LoadAPI reads API gateway configuration from environment variables.
func LoadAPI() (*APIConfig, error) {
	cfg := &APIConfig{
		ListenAddr:      defaultListenAddr,
		GRPCListenAddr:  defaultGRPCListenAddr,
		MaxFileBytes:    defaultMaxFileBytes,
		DataDir:         "/tmp/sandbox-api",
		HeartbeatGrace:  defaultHeartbeatGrace,
		IdleStopAfter:   defaultIdleStopAfter,
		IdleDeleteAfter: defaultIdleDeleteAfter,
	}

	rawKeys := os.Getenv("SANDBOX_API_KEYS")
	if rawKeys == "" {
		return nil, fmt.Errorf("SANDBOX_API_KEYS must be set")
	}
	cfg.APIKeys = make(map[string]struct{})
	for _, k := range strings.Split(rawKeys, ",") {
		k = strings.TrimSpace(k)
		if k != "" {
			cfg.APIKeys[k] = struct{}{}
		}
	}
	if len(cfg.APIKeys) == 0 {
		return nil, fmt.Errorf("SANDBOX_API_KEYS contains no valid keys")
	}

	if v := os.Getenv("SANDBOX_API_LISTEN_ADDR"); v != "" {
		cfg.ListenAddr = v
	}

	if v := os.Getenv("SANDBOX_API_GRPC_LISTEN_ADDR"); v != "" {
		cfg.GRPCListenAddr = v
	}

	cfg.RegistrationToken = os.Getenv("SANDBOX_API_RUNNER_REGISTRATION_TOKEN")
	if cfg.RegistrationToken == "" {
		return nil, fmt.Errorf("SANDBOX_API_RUNNER_REGISTRATION_TOKEN must be set")
	}

	if v := os.Getenv("SANDBOX_API_MAX_FILE_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("SANDBOX_API_MAX_FILE_BYTES must be a positive integer, got %q", v)
		}
		cfg.MaxFileBytes = n
	}

	cfg.RunnerAPIKey = os.Getenv("SANDBOX_API_RUNNER_API_KEY")

	if v := os.Getenv("SANDBOX_API_DATA_DIR"); v != "" {
		cfg.DataDir = v
	}

	if v := os.Getenv("SANDBOX_API_ENABLE_CORS"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("SANDBOX_API_ENABLE_CORS must be a boolean, got %q", v)
		}
		cfg.EnableCORS = b
	}

	if v := os.Getenv("SANDBOX_API_RUNNER_HEARTBEAT_GRACE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("SANDBOX_API_RUNNER_HEARTBEAT_GRACE must be a positive duration (e.g. 45s, 2m), got %q", v)
		}
		cfg.HeartbeatGrace = d
	}

	cfg.GRPCServerCertFile = strings.TrimSpace(os.Getenv("SANDBOX_API_GRPC_TLS_CERT_FILE"))
	cfg.GRPCServerKeyFile = strings.TrimSpace(os.Getenv("SANDBOX_API_GRPC_TLS_KEY_FILE"))
	cfg.GRPCClientCAFile = strings.TrimSpace(os.Getenv("SANDBOX_API_GRPC_TLS_CLIENT_CA_FILE"))

	tlsN := 0
	if cfg.GRPCServerCertFile != "" {
		tlsN++
	}
	if cfg.GRPCServerKeyFile != "" {
		tlsN++
	}
	if cfg.GRPCClientCAFile != "" {
		tlsN++
	}
	if tlsN != 3 {
		return nil, fmt.Errorf("SANDBOX_API_GRPC_TLS_CERT_FILE, SANDBOX_API_GRPC_TLS_KEY_FILE, and SANDBOX_API_GRPC_TLS_CLIENT_CA_FILE are required for runner-registry mTLS")
	}

	cfg.RunnerControlGRPCClientCAFile = strings.TrimSpace(os.Getenv("SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_CA_FILE"))
	cfg.RunnerControlGRPCClientCertFile = strings.TrimSpace(os.Getenv("SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_CERT_FILE"))
	cfg.RunnerControlGRPCClientKeyFile = strings.TrimSpace(os.Getenv("SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_KEY_FILE"))
	cfg.RunnerControlGRPCClientServerName = strings.TrimSpace(os.Getenv("SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_SERVER_NAME"))

	ctlN := 0
	if cfg.RunnerControlGRPCClientCAFile != "" {
		ctlN++
	}
	if cfg.RunnerControlGRPCClientCertFile != "" {
		ctlN++
	}
	if cfg.RunnerControlGRPCClientKeyFile != "" {
		ctlN++
	}
	if ctlN != 3 {
		return nil, fmt.Errorf("SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_CA_FILE, SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_CERT_FILE, and SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_KEY_FILE are required for control-plane mTLS")
	}

	if v := strings.TrimSpace(os.Getenv("SANDBOX_API_IDLE_STOP_AFTER")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return nil, fmt.Errorf("SANDBOX_API_IDLE_STOP_AFTER must be unset, 0, or a positive duration, got %q", v)
		}
		cfg.IdleStopAfter = d
	}
	if v := strings.TrimSpace(os.Getenv("SANDBOX_API_IDLE_DELETE_AFTER")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return nil, fmt.Errorf("SANDBOX_API_IDLE_DELETE_AFTER must be unset, 0, or a positive duration, got %q", v)
		}
		cfg.IdleDeleteAfter = d
	}
	if v := strings.TrimSpace(os.Getenv("SANDBOX_API_IDLE_DELETE_SAFETY_BUFFER")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return nil, fmt.Errorf("SANDBOX_API_IDLE_DELETE_SAFETY_BUFFER must be a non-negative duration, got %q", v)
		}
		cfg.IdleDeleteSafetyBuffer = d
	}
	if cfg.IdleDeleteAfter > 0 && cfg.IdleDeleteSafetyBuffer <= 0 {
		cfg.IdleDeleteSafetyBuffer = time.Minute
	}

	cfg.IdleSweepInterval = time.Minute
	if v := strings.TrimSpace(os.Getenv("SANDBOX_API_IDLE_SWEEP_INTERVAL")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("SANDBOX_API_IDLE_SWEEP_INTERVAL must be a positive duration, got %q", v)
		}
		cfg.IdleSweepInterval = d
	}

	return cfg, nil
}
