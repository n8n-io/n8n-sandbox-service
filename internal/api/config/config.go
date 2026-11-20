// Package config loads the API configuration from SANDBOX_API_* environment
// variables (defaults and semantics: docs/configuration.md).
package config

import (
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/n8n-io/sandbox-service/internal/codetask"
	"github.com/n8n-io/sandbox-service/internal/envvar"
	"github.com/n8n-io/sandbox-service/internal/logging"
)

const (
	defaultListenAddr       = ":8080"
	defaultGRPCListenAddr   = ":9090"
	defaultVolumeLocalAddr  = ":8443"
	defaultMaxFileBytes     = 10 * 1024 * 1024 // 10 MB
	defaultHeartbeatGrace   = 45 * time.Second
	defaultOrphanReapBuffer = 5 * time.Minute
	defaultIdleStopAfter    = time.Hour
	defaultIdleDeleteAfter  = 24 * time.Hour
	defaultLogLevel         = slog.LevelInfo
	defaultPostgresPort     = 5432
	defaultPostgresSSLMode  = "require"
	defaultMaxSandboxes     = 50

	defaultMaxInflightPerTenant = 512

	DefaultActivityWriteInterval = 5 * time.Second
	maxActivityWriteInterval     = time.Minute
	DefaultMaxResponseActivity   = 30 * time.Minute
	minMaxResponseActivity       = time.Minute

	defaultAPIKeyCacheTTL = 15 * time.Second
	maxAPIKeyCacheTTL     = time.Minute
)

const (
	defaultIdleSweepConcurrency = 8
	// Caps sweep connections per replica. The documented two-replica
	// deployment stays under the default max_connections=100 with 8 left for
	// other clients: 2 * (25 store + 5 lock headroom + 16) = 92.
	maxIdleSweepConcurrency = 16
	// Postgres sandbox-lock connections kept free for the request path while
	// the sweeper holds its full concurrency worth.
	sandboxLockRequestHeadroom = 5
)

// StoreBackend selects the API sandbox store implementation.
type StoreBackend string

const (
	StoreSQLite   StoreBackend = "sqlite"
	StorePostgres StoreBackend = "postgres"
)

// VolumeStoreBackend selects where volume objects are stored.
type VolumeStoreBackend string

const (
	VolumeStoreNone   VolumeStoreBackend = ""
	VolumeStoreLocal  VolumeStoreBackend = "local"
	VolumeStoreRemote VolumeStoreBackend = "remote"
)

// PostgresConfig holds connection settings for the Postgres store backend.
type PostgresConfig struct {
	Host     string
	Port     int
	User     string
	Password string
	Database string
	SSLMode  string
	// LockPoolSize caps the per-sandbox advisory lock pool (0 = store default).
	LockPoolSize int
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
	// APIKeys is the set of admin API keys: full access to every route.
	APIKeys map[string]struct{}

	// ProvisionerKeys is parsed from SANDBOX_API_PROVISIONER_KEYS; may be
	// empty. See docs/security-model.md, "Provisioner keys".
	ProvisionerKeys map[string]struct{}

	// ScalerKeys is parsed from SANDBOX_API_SCALER_KEYS; may be empty.
	ScalerKeys map[string]struct{}

	// ListenAddr is the TCP address the API gateway listens on.
	ListenAddr string

	// GRPCListenAddr is the TCP address for the private runner registration gRPC server.
	GRPCListenAddr string

	// RegistrationToken is the shared secret runners present as Bearer token on the gRPC stream.
	RegistrationToken string

	// MaxFileBytes is the maximum size of file upload requests accepted by API.
	MaxFileBytes int64

	// MaxTaskInputBytes is the maximum total size of a code sandbox task's
	// input files. The request body may be codetask.BodyOverheadBytes larger.
	// Parsed from SANDBOX_API_MAX_TASK_INPUT_BYTES (default 64 MiB, at most 1 GiB).
	MaxTaskInputBytes int64

	// DefaultMaxSandboxes is the default per-tenant sandbox quota when
	// POST /admin/tenants omits max_sandboxes (0 = unlimited).
	DefaultMaxSandboxes int

	// MaxInflightPerTenant caps a tenant's sandbox requests in progress on this
	// replica (0 = no cap). Parsed from SANDBOX_API_MAX_INFLIGHT_PER_TENANT.
	MaxInflightPerTenant int

	// RunnerAPIKey is the optional API key sent to runner via X-Api-Key.
	RunnerAPIKey string

	// DataDir is the directory for storing API state (SQLite file when Store=sqlite).
	DataDir string

	// SQLiteWAL opens the SQLite file in WAL mode with synchronous=NORMAL.
	// Parsed from SANDBOX_API_SQLITE_WAL (default true).
	SQLiteWAL bool

	// Store selects sqlite (default) or postgres for multi-pod deployments.
	Store StoreBackend

	// Postgres connection settings when Store=postgres.
	Postgres PostgresConfig

	// HeartbeatGrace is how long after the last gRPC heartbeat a runner may still be chosen for placement.
	HeartbeatGrace time.Duration

	// EnableCORS enables CORS headers (allow all origins). Default false.
	EnableCORS bool

	// CodeSandboxesEnabled serves the /code/sandboxes routes. Parsed from
	// SANDBOX_API_CODE_SANDBOXES_ENABLED (default false).
	CodeSandboxesEnabled bool

	// MetricsEnabled controls whether the Prometheus /metrics endpoint is served.
	// Parsed from SANDBOX_API_METRICS_ENABLED (default false). When true, /metrics
	// bypasses X-Api-Key authentication on whichever listener serves it; operators
	// are expected to firewall that port or front it with a private LB.
	MetricsEnabled bool

	// MetricsListenAddr is the TCP address for a dedicated Prometheus listener.
	// Parsed from SANDBOX_API_METRICS_LISTEN_ADDR (default empty). Empty, or the
	// same port as ListenAddr, keeps /metrics on the public API listener.
	// Ignored when MetricsEnabled is false.
	MetricsListenAddr string

	// Runner registration gRPC mTLS (required). All three must be set.
	GRPCServerCertFile string
	GRPCServerKeyFile  string
	GRPCClientCAFile   string

	// IdleStopAfter is how long after last activity the API asks the runner to stop
	// the sandbox (0 = disabled).
	IdleStopAfter time.Duration
	// IdleDeleteAfter is how long after last activity the API deletes the sandbox
	// (0 = disabled). Wakes are refused after this window until the row is removed.
	IdleDeleteAfter time.Duration
	// IdleDeleteSafetyBuffer is added to a sandbox's idle window before the sweeper
	// deletes it (race guard). Ephemeral sandboxes use IdleStopAfter as their window
	// (IdleDeleteAfter when idle stop is disabled), so when either window is > 0 and
	// this is unset, it defaults to 1m.
	IdleDeleteSafetyBuffer time.Duration
	// IdleSweepInterval is how often the idle stop/delete sweeper runs (default 1m).
	IdleSweepInterval time.Duration
	// IdleSweepConcurrency is how many runner calls one sweep keeps in flight (default 8).
	IdleSweepConcurrency int
	// OrphanReapBuffer is how long after a runner deregisters before the idle
	// sweeper removes its orphaned sandbox rows from the store.
	OrphanReapBuffer time.Duration
	// ActivityWriteInterval is how often each API pod writes the
	// last_active_at of the sandboxes it served, in one statement.
	// last_active_at can trail the last request by twice this, so the stop
	// window and the request-path fence are extended by twice this, and deletes
	// by four times (docs/architecture.md, "Sandbox activity").
	// Parsed from SANDBOX_API_ACTIVITY_WRITE_INTERVAL (1s–1m, default 5s).
	ActivityWriteInterval time.Duration
	// MaxResponseActivity is how long a response that is still open, such as an
	// exec stream, keeps its sandbox from going idle. Keep it at least the
	// runner's max exec timeout. Parsed from SANDBOX_API_MAX_RESPONSE_ACTIVITY
	// (at least 1m, default 30m).
	MaxResponseActivity time.Duration

	// APIKeyCacheTTL is how long each API pod remembers a tenant key lookup,
	// unknown keys included, so a revoked key keeps working on other pods for
	// up to this long (docs/architecture.md, "Tenant key cache"). Parsed from
	// SANDBOX_API_KEY_CACHE_TTL (0–1m, default 15s; 0 = no cache).
	APIKeyCacheTTL time.Duration

	// LogLevel controls the minimum log severity.
	// Parsed from SANDBOX_API_LOG_LEVEL (default info).
	LogLevel slog.Level

	// API as mTLS client dialing runner SandboxControl (required). All three must be set.
	RunnerControlGRPCClientCAFile     string
	RunnerControlGRPCClientCertFile   string
	RunnerControlGRPCClientKeyFile    string
	RunnerControlGRPCClientServerName string // optional; defaults to runner dial host

	// ScalerURL is the base URL of the externally-operated Runner Scaler.
	// Empty disables GET /admin/scaler (it returns 503).
	ScalerURL string

	// VolumeStore selects where volume objects are stored; empty means no
	// store. Parsed from SANDBOX_API_VOLUME_STORE.
	VolumeStore VolumeStoreBackend
	// VolumeLocalListenAddr is the TCP address of the mTLS listener that
	// serves volume objects to runners (VolumeStoreLocal). Parsed from
	// SANDBOX_API_VOLUME_LOCAL_LISTEN_ADDR (default :8443).
	VolumeLocalListenAddr string
	// VolumeLocalURL is the https base URL at which runners reach
	// VolumeLocalListenAddr, which signed object URLs point at
	// (VolumeStoreLocal).
	VolumeLocalURL string
	// VolumeRemoteSocket is the Unix socket of the store helper
	// (VolumeStoreRemote).
	VolumeRemoteSocket string
}

// LoadAPI reads API gateway configuration from environment variables.
func LoadAPI() (*APIConfig, error) {
	cfg := &APIConfig{
		ListenAddr:            defaultListenAddr,
		GRPCListenAddr:        defaultGRPCListenAddr,
		MaxFileBytes:          defaultMaxFileBytes,
		MaxTaskInputBytes:     codetask.DefaultMaxInputBytes,
		DefaultMaxSandboxes:   defaultMaxSandboxes,
		MaxInflightPerTenant:  defaultMaxInflightPerTenant,
		DataDir:               "/var/lib/n8n-sandbox-api",
		SQLiteWAL:             true,
		Store:                 StoreSQLite,
		Postgres:              PostgresConfig{Port: defaultPostgresPort, SSLMode: defaultPostgresSSLMode},
		HeartbeatGrace:        defaultHeartbeatGrace,
		OrphanReapBuffer:      defaultOrphanReapBuffer,
		IdleStopAfter:         defaultIdleStopAfter,
		IdleDeleteAfter:       defaultIdleDeleteAfter,
		ActivityWriteInterval: DefaultActivityWriteInterval,
		MaxResponseActivity:   DefaultMaxResponseActivity,
		APIKeyCacheTTL:        defaultAPIKeyCacheTTL,
		LogLevel:              defaultLogLevel,
	}

	// SANDBOX_API_LOG_LEVEL (optional)
	if v := envvar.Get("SANDBOX_API_LOG_LEVEL"); v != "" {
		lvl, err := logging.ParseLevel(v)
		if err != nil {
			return nil, fmt.Errorf("SANDBOX_API_LOG_LEVEL: %w", err)
		}
		cfg.LogLevel = lvl
	}

	rawKeys := envvar.Get("SANDBOX_API_KEYS")
	if rawKeys == "" {
		return nil, fmt.Errorf("SANDBOX_API_KEYS must be set")
	}
	cfg.APIKeys = parseKeySet(rawKeys)
	if len(cfg.APIKeys) == 0 {
		return nil, fmt.Errorf("SANDBOX_API_KEYS contains no valid keys")
	}

	// A key in two sets would silently get the role checked first: admin,
	// then provisioner, then scaler.
	cfg.ProvisionerKeys = parseKeySet(envvar.Get("SANDBOX_API_PROVISIONER_KEYS"))
	for k := range cfg.ProvisionerKeys {
		if _, isAdmin := cfg.APIKeys[k]; isAdmin {
			return nil, fmt.Errorf("SANDBOX_API_PROVISIONER_KEYS must not share a key with SANDBOX_API_KEYS")
		}
	}
	cfg.ScalerKeys = parseKeySet(envvar.Get("SANDBOX_API_SCALER_KEYS"))
	for k := range cfg.ScalerKeys {
		if _, isAdmin := cfg.APIKeys[k]; isAdmin {
			return nil, fmt.Errorf("SANDBOX_API_SCALER_KEYS must not share a key with SANDBOX_API_KEYS")
		}
		if _, isProvisioner := cfg.ProvisionerKeys[k]; isProvisioner {
			return nil, fmt.Errorf("SANDBOX_API_SCALER_KEYS must not share a key with SANDBOX_API_PROVISIONER_KEYS")
		}
	}

	envvar.String("SANDBOX_API_LISTEN_ADDR", &cfg.ListenAddr)
	envvar.String("SANDBOX_API_GRPC_LISTEN_ADDR", &cfg.GRPCListenAddr)

	if v := envvar.Get("SANDBOX_API_METRICS_LISTEN_ADDR"); v != "" {
		if err := validateListenAddr(v); err != nil {
			return nil, fmt.Errorf("SANDBOX_API_METRICS_LISTEN_ADDR must be host:port (e.g. :9100), got %q: %w", v, err)
		}
		cfg.MetricsListenAddr = v
	}
	if cfg.MetricsListenAddr != "" {
		// Caught here rather than as an EADDRINUSE crash loop that names neither var.
		if listenPort(cfg.MetricsListenAddr) == listenPort(cfg.GRPCListenAddr) {
			return nil, fmt.Errorf("SANDBOX_API_METRICS_LISTEN_ADDR (%q) must not use the same port as SANDBOX_API_GRPC_LISTEN_ADDR (%q)", cfg.MetricsListenAddr, cfg.GRPCListenAddr)
		}
		// The API listener wins the shared port, so accepting a different host
		// here would quietly bind metrics somewhere the operator did not ask for.
		if cfg.MetricsOnMainListener() && !sameListenHost(cfg.MetricsListenAddr, cfg.ListenAddr) {
			return nil, fmt.Errorf("SANDBOX_API_METRICS_LISTEN_ADDR (%q) uses the port of SANDBOX_API_LISTEN_ADDR (%q) but a different host; leave it unset to serve /metrics on the API listener", cfg.MetricsListenAddr, cfg.ListenAddr)
		}
	}

	cfg.RegistrationToken = envvar.Get("SANDBOX_API_RUNNER_REGISTRATION_TOKEN")
	if cfg.RegistrationToken == "" {
		return nil, fmt.Errorf("SANDBOX_API_RUNNER_REGISTRATION_TOKEN must be set")
	}

	if err := envvar.PositiveInt("SANDBOX_API_MAX_FILE_BYTES", &cfg.MaxFileBytes); err != nil {
		return nil, err
	}

	if err := envvar.IntBetween("SANDBOX_API_MAX_TASK_INPUT_BYTES", &cfg.MaxTaskInputBytes, 1, codetask.MaxInputBytesCeiling); err != nil {
		return nil, err
	}

	// tenants.max_sandboxes is INTEGER (signed 32-bit) in SQLite/Postgres.
	if err := envvar.IntBetween("SANDBOX_API_DEFAULT_MAX_SANDBOXES", &cfg.DefaultMaxSandboxes, 0, math.MaxInt32); err != nil {
		return nil, err
	}
	// Provisioner tenants are bounded to 1..DefaultMaxSandboxes; with 0
	// (unlimited) an omitted max_sandboxes would yield an unlimited tenant.
	if len(cfg.ProvisionerKeys) > 0 && cfg.DefaultMaxSandboxes == 0 {
		return nil, fmt.Errorf("SANDBOX_API_PROVISIONER_KEYS requires SANDBOX_API_DEFAULT_MAX_SANDBOXES to be at least 1")
	}

	if err := envvar.IntBetween("SANDBOX_API_MAX_INFLIGHT_PER_TENANT", &cfg.MaxInflightPerTenant, 0, math.MaxInt32); err != nil {
		return nil, err
	}

	cfg.RunnerAPIKey = envvar.Get("SANDBOX_API_RUNNER_API_KEY")
	envvar.String("SANDBOX_API_DATA_DIR", &cfg.DataDir)

	if err := envvar.Bool("SANDBOX_API_SQLITE_WAL", &cfg.SQLiteWAL); err != nil {
		return nil, err
	}
	if err := envvar.Bool("SANDBOX_API_METRICS_ENABLED", &cfg.MetricsEnabled); err != nil {
		return nil, err
	}
	if err := envvar.Bool("SANDBOX_API_ENABLE_CORS", &cfg.EnableCORS); err != nil {
		return nil, err
	}
	if err := envvar.Bool("SANDBOX_API_CODE_SANDBOXES_ENABLED", &cfg.CodeSandboxesEnabled); err != nil {
		return nil, err
	}

	if err := envvar.PositiveDuration("SANDBOX_API_RUNNER_HEARTBEAT_GRACE", &cfg.HeartbeatGrace); err != nil {
		return nil, err
	}

	cfg.GRPCServerCertFile = envvar.Get("SANDBOX_API_GRPC_TLS_CERT_FILE")
	cfg.GRPCServerKeyFile = envvar.Get("SANDBOX_API_GRPC_TLS_KEY_FILE")
	cfg.GRPCClientCAFile = envvar.Get("SANDBOX_API_GRPC_TLS_CLIENT_CA_FILE")

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

	cfg.RunnerControlGRPCClientCAFile = envvar.Get("SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_CA_FILE")
	cfg.RunnerControlGRPCClientCertFile = envvar.Get("SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_CERT_FILE")
	cfg.RunnerControlGRPCClientKeyFile = envvar.Get("SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_KEY_FILE")
	cfg.RunnerControlGRPCClientServerName = envvar.Get("SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_SERVER_NAME")

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

	if err := envvar.NonNegativeDuration("SANDBOX_API_IDLE_STOP_AFTER", &cfg.IdleStopAfter); err != nil {
		return nil, err
	}
	if err := envvar.NonNegativeDuration("SANDBOX_API_IDLE_DELETE_AFTER", &cfg.IdleDeleteAfter); err != nil {
		return nil, err
	}
	if err := envvar.NonNegativeDuration("SANDBOX_API_IDLE_DELETE_SAFETY_BUFFER", &cfg.IdleDeleteSafetyBuffer); err != nil {
		return nil, err
	}
	if (cfg.IdleDeleteAfter > 0 || cfg.IdleStopAfter > 0) && cfg.IdleDeleteSafetyBuffer <= 0 {
		cfg.IdleDeleteSafetyBuffer = time.Minute
	}

	cfg.IdleSweepInterval = time.Minute
	if err := envvar.PositiveDuration("SANDBOX_API_IDLE_SWEEP_INTERVAL", &cfg.IdleSweepInterval); err != nil {
		return nil, err
	}

	cfg.IdleSweepConcurrency = defaultIdleSweepConcurrency
	if err := envvar.IntBetween("SANDBOX_API_IDLE_SWEEP_CONCURRENCY", &cfg.IdleSweepConcurrency, 1, maxIdleSweepConcurrency); err != nil {
		return nil, err
	}
	cfg.Postgres.LockPoolSize = cfg.IdleSweepConcurrency + sandboxLockRequestHeadroom

	if err := envvar.PositiveDuration("SANDBOX_API_ORPHAN_REAP_BUFFER", &cfg.OrphanReapBuffer); err != nil {
		return nil, err
	}

	if err := envvar.DurationBetween("SANDBOX_API_ACTIVITY_WRITE_INTERVAL", &cfg.ActivityWriteInterval, time.Second, maxActivityWriteInterval); err != nil {
		return nil, err
	}

	if v := envvar.Get("SANDBOX_API_MAX_RESPONSE_ACTIVITY"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < minMaxResponseActivity {
			return nil, fmt.Errorf("SANDBOX_API_MAX_RESPONSE_ACTIVITY must be a duration of at least %s, got %q", minMaxResponseActivity, v)
		}
		cfg.MaxResponseActivity = d
	}

	if err := envvar.DurationBetween("SANDBOX_API_KEY_CACHE_TTL", &cfg.APIKeyCacheTTL, 0, maxAPIKeyCacheTTL); err != nil {
		return nil, err
	}

	if v := envvar.Get("SANDBOX_API_STORE"); v != "" {
		switch StoreBackend(strings.ToLower(v)) {
		case StoreSQLite, StorePostgres:
			cfg.Store = StoreBackend(strings.ToLower(v))
		default:
			return nil, fmt.Errorf("SANDBOX_API_STORE must be sqlite or postgres, got %q", v)
		}
	}

	if cfg.Store == StorePostgres {
		cfg.Postgres.Host = envvar.Get("SANDBOX_API_POSTGRES_HOST")
		cfg.Postgres.User = envvar.Get("SANDBOX_API_POSTGRES_USER")
		// Untrimmed: a password may start or end with a space.
		cfg.Postgres.Password = os.Getenv("SANDBOX_API_POSTGRES_PASSWORD")
		cfg.Postgres.Database = envvar.Get("SANDBOX_API_POSTGRES_DB")
		if err := envvar.PositiveInt("SANDBOX_API_POSTGRES_PORT", &cfg.Postgres.Port); err != nil {
			return nil, err
		}
		envvar.String("SANDBOX_API_POSTGRES_SSLMODE", &cfg.Postgres.SSLMode)
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

	cfg.ScalerURL = strings.TrimRight(envvar.Get("SANDBOX_API_SCALER_URL"), "/")

	if err := loadVolumeStore(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

func loadVolumeStore(cfg *APIConfig) error {
	backend := envvar.Get("SANDBOX_API_VOLUME_STORE")
	switch VolumeStoreBackend(strings.ToLower(backend)) {
	case VolumeStoreNone:
		return nil
	case VolumeStoreLocal:
		cfg.VolumeStore = VolumeStoreLocal
		localURL, err := parseLocalURL()
		if err != nil {
			return err
		}
		cfg.VolumeLocalURL = localURL
		listenAddr, err := parseVolumeLocalListenAddr(cfg)
		if err != nil {
			return err
		}
		cfg.VolumeLocalListenAddr = listenAddr
	case VolumeStoreRemote:
		cfg.VolumeStore = VolumeStoreRemote
		socket := envvar.Get("SANDBOX_API_VOLUME_REMOTE_SOCKET")
		// 107 bytes is the most a Unix socket address holds on Linux.
		if !filepath.IsAbs(socket) || len(socket) > 107 {
			return fmt.Errorf("SANDBOX_API_VOLUME_REMOTE_SOCKET must be an absolute path of at most 107 bytes when SANDBOX_API_VOLUME_STORE=remote, got %q", socket)
		}
		cfg.VolumeRemoteSocket = socket
	default:
		return fmt.Errorf("SANDBOX_API_VOLUME_STORE must be empty, local or remote, got %q", backend)
	}
	return nil
}

// parseLocalURL reads SANDBOX_API_VOLUME_LOCAL_URL without its trailing
// slash. It takes no path: the object route is served at the root. Errors
// leave the value out, since a rejected one may hold a secret.
func parseLocalURL() (string, error) {
	raw := envvar.Get("SANDBOX_API_VOLUME_LOCAL_URL")
	if raw == "" {
		return "", fmt.Errorf("SANDBOX_API_VOLUME_LOCAL_URL must be set when SANDBOX_API_VOLUME_STORE=local")
	}
	parsed, err := url.Parse(raw)
	if err == nil && parsed.Scheme == "http" {
		return "", fmt.Errorf("SANDBOX_API_VOLUME_LOCAL_URL must be an https URL: volume objects are served only on the listener at SANDBOX_API_VOLUME_LOCAL_LISTEN_ADDR, which requires TLS with a client certificate")
	}
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" ||
		parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || strings.ContainsAny(raw, "?#") {
		return "", fmt.Errorf("SANDBOX_API_VOLUME_LOCAL_URL must be an https URL with a host and port only: no credentials, path, query or fragment")
	}
	return strings.TrimRight(raw, "/"), nil
}

// parseVolumeLocalListenAddr reads SANDBOX_API_VOLUME_LOCAL_LISTEN_ADDR. One
// port is one socket, so it refuses the port of any other listener here
// rather than as an EADDRINUSE crash loop that names no variable.
func parseVolumeLocalListenAddr(cfg *APIConfig) (string, error) {
	const name = "SANDBOX_API_VOLUME_LOCAL_LISTEN_ADDR"
	addr := defaultVolumeLocalAddr
	if v := envvar.Get(name); v != "" {
		addr = v
	}
	if err := validateListenAddr(addr); err != nil {
		return "", fmt.Errorf("%s must be host:port (e.g. %s), got %q: %w", name, defaultVolumeLocalAddr, addr, err)
	}
	for _, other := range []struct{ name, addr string }{
		{"SANDBOX_API_LISTEN_ADDR", cfg.ListenAddr},
		{"SANDBOX_API_GRPC_LISTEN_ADDR", cfg.GRPCListenAddr},
		{"SANDBOX_API_METRICS_LISTEN_ADDR", cfg.MetricsListenAddr},
	} {
		if other.addr != "" && listenPort(addr) == listenPort(other.addr) {
			return "", fmt.Errorf("%s (%q) must not use the same port as %s (%q)", name, addr, other.name, other.addr)
		}
	}
	return addr, nil
}

// MetricsOnMainListener reports whether /metrics is served by the public API
// listener rather than by a dedicated one. One port is one socket, so naming
// the ListenAddr port counts as sharing it.
func (c *APIConfig) MetricsOnMainListener() bool {
	if c.MetricsListenAddr == "" {
		return true
	}
	return listenPort(c.MetricsListenAddr) == listenPort(c.ListenAddr)
}

// ResolvedMetricsListenAddr returns the address /metrics is reachable on.
func (c *APIConfig) ResolvedMetricsListenAddr() string {
	if c.MetricsOnMainListener() {
		return c.ListenAddr
	}
	return c.MetricsListenAddr
}

// validateListenAddr leaves the host unconstrained: empty (":9100"), loopback
// and an explicit interface are all bindable.
func validateListenAddr(v string) error {
	_, port, err := net.SplitHostPort(strings.TrimSpace(v))
	if err != nil {
		return err
	}
	p, err := strconv.Atoi(port)
	if err != nil || p <= 0 || p > 65535 {
		return fmt.Errorf("invalid port %q", port)
	}
	return nil
}

// listenPort resolves a port the way net.Listen will, so addresses that name
// one socket compare equal. That covers a padded number (":08080") and a
// service name (":http"), either of which would otherwise look like a port of
// its own and start a second server that cannot bind. Metrics and volume
// addresses are numeric by validateListenAddr, but ListenAddr and
// GRPCListenAddr are not.
func listenPort(addr string) string {
	_, port, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return ""
	}
	if n, err := net.LookupPort("tcp", port); err == nil {
		return strconv.Itoa(n)
	}
	return port
}

// sameListenHost treats every spelling of "all interfaces" as one host, so
// ":8080" and "[::]:8080" are not a conflict.
func sameListenHost(a, b string) bool {
	hostA, _, errA := net.SplitHostPort(strings.TrimSpace(a))
	hostB, _, errB := net.SplitHostPort(strings.TrimSpace(b))
	if errA != nil || errB != nil {
		return false
	}
	if isWildcardHost(hostA) && isWildcardHost(hostB) {
		return true
	}
	return hostA == hostB
}

func isWildcardHost(host string) bool {
	switch strings.TrimSpace(host) {
	case "", "0.0.0.0", "::":
		return true
	}
	return false
}

// parseKeySet splits a comma-separated list of API keys, dropping blanks.
func parseKeySet(raw string) map[string]struct{} {
	keys := make(map[string]struct{})
	for k := range strings.SplitSeq(raw, ",") {
		if k = strings.TrimSpace(k); k != "" {
			keys[k] = struct{}{}
		}
	}
	return keys
}
