package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/n8n-io/sandbox-service/internal/logging"
)

const (
	defaultListenAddr       = ":8080"
	defaultGRPCListenAddr   = ":9090"
	defaultMaxFileBytes     = 10 * 1024 * 1024 // 10 MB
	defaultHeartbeatGrace   = 45 * time.Second
	defaultOrphanReapBuffer = 5 * time.Minute
	defaultIdleStopAfter    = time.Hour
	defaultIdleDeleteAfter  = 24 * time.Hour
	defaultLogLevel         = slog.LevelInfo
	defaultPostgresPort     = 5432
	defaultPostgresSSLMode  = "require"
	defaultMaxSandboxes     = 50
)

// StoreBackend selects the API sandbox store implementation.
type StoreBackend string

const (
	StoreSQLite   StoreBackend = "sqlite"
	StorePostgres StoreBackend = "postgres"
)

// PostgresConfig holds connection settings for the Postgres store backend.
type PostgresConfig struct {
	Host     string
	Port     int
	User     string
	Password string
	Database string
	SSLMode  string
}

// DSN returns a libpq connection string for pgx/stdlib.
func (p PostgresConfig) DSN() string {
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		p.Host, p.Port, p.User, p.Password, p.Database, p.SSLMode,
	)
}

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

	// DefaultMaxSandboxes is the default per-tenant sandbox quota when
	// POST /admin/tenants omits max_sandboxes (0 = unlimited).
	DefaultMaxSandboxes int

	// RunnerAPIKey is the optional API key sent to runner via X-Api-Key.
	RunnerAPIKey string

	// DataDir is the directory for storing API state (SQLite file when Store=sqlite).
	DataDir string

	// Store selects sqlite (default) or postgres for multi-pod deployments.
	Store StoreBackend

	// Postgres connection settings when Store=postgres.
	Postgres PostgresConfig

	// HeartbeatGrace is how long after the last gRPC heartbeat a runner may still be chosen for placement.
	HeartbeatGrace time.Duration

	// EnableCORS enables CORS headers (allow all origins). Default false.
	EnableCORS bool

	// MetricsEnabled controls whether the Prometheus /metrics endpoint is served.
	// Parsed from SANDBOX_API_METRICS_ENABLED (default false). When true, /metrics
	// is exposed on the public listener and bypasses X-Api-Key authentication;
	// operators are expected to firewall the port or front it with a private LB.
	MetricsEnabled bool

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
	// OrphanReapBuffer is how long after a runner deregisters before the idle
	// sweeper removes its orphaned sandbox rows from the store.
	OrphanReapBuffer time.Duration

	// LogLevel controls the minimum log severity.
	// Parsed from SANDBOX_API_LOG_LEVEL (default info).
	LogLevel slog.Level

	// API as mTLS client dialing runner SandboxControl (required). All three must be set.
	RunnerControlGRPCClientCAFile     string
	RunnerControlGRPCClientCertFile   string
	RunnerControlGRPCClientKeyFile    string
	RunnerControlGRPCClientServerName string // optional; defaults to runner dial host
}

// LoadAPI reads API gateway configuration from environment variables.
func LoadAPI() (*APIConfig, error) {
	cfg := &APIConfig{
		ListenAddr:          defaultListenAddr,
		GRPCListenAddr:      defaultGRPCListenAddr,
		MaxFileBytes:        defaultMaxFileBytes,
		DefaultMaxSandboxes: defaultMaxSandboxes,
		DataDir:             "/var/lib/n8n-sandbox-api",
		Store:               StoreSQLite,
		Postgres:            PostgresConfig{Port: defaultPostgresPort, SSLMode: defaultPostgresSSLMode},
		HeartbeatGrace:      defaultHeartbeatGrace,
		OrphanReapBuffer:    defaultOrphanReapBuffer,
		IdleStopAfter:       defaultIdleStopAfter,
		IdleDeleteAfter:     defaultIdleDeleteAfter,
		LogLevel:            defaultLogLevel,
	}

	// SANDBOX_API_LOG_LEVEL (optional)
	if v := os.Getenv("SANDBOX_API_LOG_LEVEL"); v != "" {
		lvl, err := logging.ParseLevel(v)
		if err != nil {
			return nil, fmt.Errorf("SANDBOX_API_LOG_LEVEL: %w", err)
		}
		cfg.LogLevel = lvl
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

	if v := os.Getenv("SANDBOX_API_DEFAULT_MAX_SANDBOXES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("SANDBOX_API_DEFAULT_MAX_SANDBOXES must be an integer >= 0, got %q", v)
		}
		cfg.DefaultMaxSandboxes = n
	}

	cfg.RunnerAPIKey = os.Getenv("SANDBOX_API_RUNNER_API_KEY")

	if v := os.Getenv("SANDBOX_API_DATA_DIR"); v != "" {
		cfg.DataDir = v
	}

	if v := os.Getenv("SANDBOX_API_METRICS_ENABLED"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("SANDBOX_API_METRICS_ENABLED must be a boolean, got %q", v)
		}
		cfg.MetricsEnabled = b
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

	if v := strings.TrimSpace(os.Getenv("SANDBOX_API_ORPHAN_REAP_BUFFER")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("SANDBOX_API_ORPHAN_REAP_BUFFER must be a positive duration, got %q", v)
		}
		cfg.OrphanReapBuffer = d
	}

	if v := strings.TrimSpace(os.Getenv("SANDBOX_API_STORE")); v != "" {
		switch StoreBackend(strings.ToLower(v)) {
		case StoreSQLite, StorePostgres:
			cfg.Store = StoreBackend(strings.ToLower(v))
		default:
			return nil, fmt.Errorf("SANDBOX_API_STORE must be sqlite or postgres, got %q", v)
		}
	}

	if cfg.Store == StorePostgres {
		cfg.Postgres.Host = strings.TrimSpace(os.Getenv("SANDBOX_API_POSTGRES_HOST"))
		cfg.Postgres.User = strings.TrimSpace(os.Getenv("SANDBOX_API_POSTGRES_USER"))
		cfg.Postgres.Password = os.Getenv("SANDBOX_API_POSTGRES_PASSWORD")
		cfg.Postgres.Database = strings.TrimSpace(os.Getenv("SANDBOX_API_POSTGRES_DB"))
		if v := strings.TrimSpace(os.Getenv("SANDBOX_API_POSTGRES_PORT")); v != "" {
			port, err := strconv.Atoi(v)
			if err != nil || port <= 0 {
				return nil, fmt.Errorf("SANDBOX_API_POSTGRES_PORT must be a positive integer, got %q", v)
			}
			cfg.Postgres.Port = port
		}
		if v := strings.TrimSpace(os.Getenv("SANDBOX_API_POSTGRES_SSLMODE")); v != "" {
			cfg.Postgres.SSLMode = v
		}
		if cfg.Postgres.Host == "" {
			return nil, fmt.Errorf("SANDBOX_API_POSTGRES_HOST must be set when SANDBOX_API_STORE=postgres")
		}
		if cfg.Postgres.User == "" {
			return nil, fmt.Errorf("SANDBOX_API_POSTGRES_USER must be set when SANDBOX_API_STORE=postgres")
		}
		if cfg.Postgres.Password == "" {
			return nil, fmt.Errorf("SANDBOX_API_POSTGRES_PASSWORD must be set when SANDBOX_API_STORE=postgres")
		}
		if cfg.Postgres.Database == "" {
			return nil, fmt.Errorf("SANDBOX_API_POSTGRES_DB must be set when SANDBOX_API_STORE=postgres")
		}
	}

	return cfg, nil
}
