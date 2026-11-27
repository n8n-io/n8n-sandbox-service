// Package config loads the runner configuration shared by both runtimes from
// SANDBOX_RUNNER_* environment variables (defaults and semantics: docs/configuration.md).
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
	"github.com/n8n-io/sandbox-service/internal/daemon"
	"github.com/n8n-io/sandbox-service/internal/envvar"
	"github.com/n8n-io/sandbox-service/internal/logging"
)

const (
	defaultRunnerCapacityTotal int32 = 1000

	defaultMaxFileBytes          = 10 * 1024 * 1024 // 10 MB
	defaultDataDir               = "/var/sandboxes"
	defaultListenAddr            = ":8080"
	defaultControlGRPCListenAddr = ":9091"
	defaultLogLevel              = slog.LevelInfo

	defaultMaxInflightPerSandbox = 256
	defaultMaxExecTimeout        = 15 * time.Minute

	defaultVolumeCacheMaxBytes = 20 << 30 // 20 GiB
	defaultVolumeFetchTimeout  = 5 * time.Minute

	defaultCodeCPUPercent  = 100
	defaultCodeMaxMemoryMB = 1024
)

// Config holds shared runner configuration parsed from environment variables.
type Config struct {
	// APIKeys is the set of valid API keys for authenticating requests.
	// Parsed from SANDBOX_RUNNER_API_KEYS (comma-separated).
	APIKeys map[string]struct{}

	// MaxFileBytes is the maximum size of a single file that may be written
	// into a sandbox. Parsed from SANDBOX_RUNNER_MAX_FILE_BYTES (default 10 MB).
	MaxFileBytes int64

	// MaxTaskInputBytes is the maximum total size of a code sandbox task's
	// input files. The request body may be codetask.BodyOverheadBytes larger.
	// Parsed from SANDBOX_RUNNER_MAX_TASK_INPUT_BYTES (default 64 MiB, at most 1 GiB).
	MaxTaskInputBytes int64

	// DataDir is the directory under which per-sandbox data is stored.
	// Parsed from SANDBOX_RUNNER_DATA_DIR (default /var/sandboxes).
	DataDir string

	// ListenAddr is the TCP address the HTTP server listens on.
	// Parsed from SANDBOX_RUNNER_LISTEN_ADDR (default :8080).
	ListenAddr string

	// APIGRPCAddr is the host:port of the API's runner registration gRPC listener.
	// Parsed from SANDBOX_RUNNER_API_GRPC_ADDR.
	APIGRPCAddr string

	// RegistrationToken authenticates this runner to the API gRPC service.
	// Parsed from SANDBOX_RUNNER_REGISTRATION_TOKEN.
	RegistrationToken string

	// RunnerID is the stable ID advertised to the API (placement / persistence).
	// Parsed from SANDBOX_RUNNER_ID (default: machine hostname).
	RunnerID string

	// RunnerHTTPBaseURL is the base URL the API uses to reach this runner's HTTP API.
	// Parsed from SANDBOX_RUNNER_HTTP_BASE_URL.
	RunnerHTTPBaseURL string

	// CapacityTotal is reported to the API for placement (0 means unlimited;
	// the Firecracker runtime requires > 0 and derives it from host memory when
	// the variable is unset). Parsed from SANDBOX_RUNNER_CAPACITY_TOTAL (default 1000).
	CapacityTotal int32

	// CapacityTotalSet records that SANDBOX_RUNNER_CAPACITY_TOTAL was set, so
	// the Firecracker runner can tell an explicit cap from the default.
	CapacityTotalSet bool

	// mTLS for registration gRPC client (required).
	GRPCServerCAFile   string
	GRPCClientCertFile string
	GRPCClientKeyFile  string
	GRPCServerName     string

	// LogLevel controls the minimum log severity.
	// Parsed from SANDBOX_RUNNER_LOG_LEVEL (default info).
	LogLevel slog.Level

	// SandboxControl gRPC (API → runner). Always enabled; listen addr defaults to :9091.
	ControlGRPCListenAddr     string
	ControlGRPCAdvertiseAddr  string
	ControlGRPCServerCertFile string
	ControlGRPCServerKeyFile  string
	ControlGRPCClientCAFile   string

	// MetricsEnabled controls whether the Prometheus /metrics endpoint is served.
	// Parsed from SANDBOX_RUNNER_METRICS_ENABLED (default false). When true,
	// /metrics is exposed on the public HTTP listener and bypasses X-Api-Key
	// authentication; operators are expected to firewall the port.
	MetricsEnabled bool

	// MaxInflightPerSandbox caps one sandbox's requests in progress (0 = no
	// cap). Parsed from SANDBOX_RUNNER_MAX_INFLIGHT_PER_SANDBOX.
	MaxInflightPerSandbox int

	// MaxExecTimeout is the largest timeout_ms an exec request may ask for
	// (0 = no limit). Parsed from SANDBOX_RUNNER_MAX_EXEC_TIMEOUT.
	MaxExecTimeout time.Duration

	// VolumeCacheDir holds downloaded volume objects and is wiped at startup.
	// Parsed from SANDBOX_RUNNER_VOLUME_CACHE_DIR (default <DataDir>/volume-cache).
	VolumeCacheDir string

	// VolumeCacheMaxBytes is the volume cache budget. Parsed from
	// SANDBOX_RUNNER_VOLUME_CACHE_MAX_BYTES (default 20 GiB).
	VolumeCacheMaxBytes int64

	// VolumeFetchTimeout bounds one volume object download. Parsed from
	// SANDBOX_RUNNER_VOLUME_FETCH_TIMEOUT (default 5m).
	VolumeFetchTimeout time.Duration

	// CodeSandboxesEnabled lets the runner create code sandboxes. Parsed from
	// SANDBOX_RUNNER_CODE_SANDBOXES_ENABLED (default false). When false, the
	// other Code* fields are not read.
	CodeSandboxesEnabled bool

	// CodeCPUPercent is the CPU limit of a code sandbox as a percentage of one
	// core. Parsed from SANDBOX_RUNNER_CODE_CPU_PERCENT (default 100).
	CodeCPUPercent int

	// CodeMaxMemoryMB is the largest memory limit a code sandbox can ask for,
	// in megabytes. Parsed from SANDBOX_RUNNER_CODE_MAX_MEMORY_MB (default 1024).
	CodeMaxMemoryMB int64
}

func validateHostPort(v string) error {
	host, port, err := net.SplitHostPort(strings.TrimSpace(v))
	if err != nil {
		return err
	}
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("host is empty")
	}
	p, err := strconv.Atoi(port)
	if err != nil || p <= 0 || p > 65535 {
		return fmt.Errorf("invalid port %q", port)
	}
	return nil
}

func validateListenAddr(v string) error {
	host, port, err := net.SplitHostPort(strings.TrimSpace(v))
	if err != nil {
		return err
	}
	// listen host may be empty (":9091"), loopback, or explicit interface
	_ = host
	p, err := strconv.Atoi(port)
	if err != nil || p <= 0 || p > 65535 {
		return fmt.Errorf("invalid port %q", port)
	}
	return nil
}

// ResolvedControlGRPCAdvertiseAddr returns the SandboxControl host:port sent in heartbeats.
func (c *Config) ResolvedControlGRPCAdvertiseAddr() string {
	if strings.TrimSpace(c.ControlGRPCListenAddr) == "" {
		return ""
	}
	if adv := strings.TrimSpace(c.ControlGRPCAdvertiseAddr); adv != "" {
		return adv
	}
	u, err := url.Parse(c.RunnerHTTPBaseURL)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	host := u.Hostname()
	port := "9091"
	if _, p, err := net.SplitHostPort(c.ControlGRPCListenAddr); err == nil && p != "" {
		port = p
	}
	return net.JoinHostPort(host, port)
}

// loadCode reads the code sandbox settings. It reads the limits only when
// code sandboxes are enabled.
func loadCode(cfg *Config) error {
	if err := envvar.Bool("SANDBOX_RUNNER_CODE_SANDBOXES_ENABLED", &cfg.CodeSandboxesEnabled); err != nil {
		return err
	}
	if !cfg.CodeSandboxesEnabled {
		return nil
	}
	cfg.CodeCPUPercent = defaultCodeCPUPercent
	cfg.CodeMaxMemoryMB = defaultCodeMaxMemoryMB
	if err := envvar.PositiveInt("SANDBOX_RUNNER_CODE_CPU_PERCENT", &cfg.CodeCPUPercent); err != nil {
		return err
	}
	return envvar.IntBetween("SANDBOX_RUNNER_CODE_MAX_MEMORY_MB", &cfg.CodeMaxMemoryMB, codetask.MinSandboxMemoryMB, math.MaxInt32)
}

// Load reads configuration from environment variables and returns a Config.
// It returns an error if any required variable is missing or malformed.
func Load() (*Config, error) {
	cfg := &Config{
		MaxFileBytes:          defaultMaxFileBytes,
		MaxTaskInputBytes:     codetask.DefaultMaxInputBytes,
		DataDir:               defaultDataDir,
		ListenAddr:            defaultListenAddr,
		CapacityTotal:         defaultRunnerCapacityTotal,
		ControlGRPCListenAddr: defaultControlGRPCListenAddr,
		LogLevel:              defaultLogLevel,
		MaxInflightPerSandbox: defaultMaxInflightPerSandbox,
		MaxExecTimeout:        defaultMaxExecTimeout,
		VolumeCacheMaxBytes:   defaultVolumeCacheMaxBytes,
		VolumeFetchTimeout:    defaultVolumeFetchTimeout,
	}

	if h, err := os.Hostname(); err == nil && h != "" {
		cfg.RunnerID = h
	}

	// SANDBOX_RUNNER_LOG_LEVEL (optional)
	if v := envvar.Get("SANDBOX_RUNNER_LOG_LEVEL"); v != "" {
		lvl, err := logging.ParseLevel(v)
		if err != nil {
			return nil, fmt.Errorf("SANDBOX_RUNNER_LOG_LEVEL: %w", err)
		}
		cfg.LogLevel = lvl
	}

	// SANDBOX_RUNNER_API_KEYS (required)
	rawKeys := envvar.Get("SANDBOX_RUNNER_API_KEYS")
	if rawKeys == "" {
		return nil, fmt.Errorf("SANDBOX_RUNNER_API_KEYS must be set")
	}
	cfg.APIKeys = make(map[string]struct{})
	for k := range strings.SplitSeq(rawKeys, ",") {
		k = strings.TrimSpace(k)
		if k != "" {
			cfg.APIKeys[k] = struct{}{}
		}
	}
	if len(cfg.APIKeys) == 0 {
		return nil, fmt.Errorf("SANDBOX_RUNNER_API_KEYS contains no valid keys")
	}

	if err := envvar.PositiveInt("SANDBOX_RUNNER_MAX_FILE_BYTES", &cfg.MaxFileBytes); err != nil {
		return nil, err
	}

	if err := envvar.IntBetween("SANDBOX_RUNNER_MAX_TASK_INPUT_BYTES", &cfg.MaxTaskInputBytes, 1, codetask.MaxInputBytesCeiling); err != nil {
		return nil, err
	}

	envvar.String("SANDBOX_RUNNER_DATA_DIR", &cfg.DataDir)

	// The volume cache checks the dir itself before wiping it, and refuses a
	// relative one, so a relative data dir is resolved here.
	cacheDir, err := filepath.Abs(filepath.Join(cfg.DataDir, "volume-cache"))
	if err != nil {
		return nil, fmt.Errorf("SANDBOX_RUNNER_DATA_DIR: %w", err)
	}
	cfg.VolumeCacheDir = cacheDir
	envvar.String("SANDBOX_RUNNER_VOLUME_CACHE_DIR", &cfg.VolumeCacheDir)
	if err := envvar.PositiveInt("SANDBOX_RUNNER_VOLUME_CACHE_MAX_BYTES", &cfg.VolumeCacheMaxBytes); err != nil {
		return nil, err
	}
	if err := envvar.PositiveDuration("SANDBOX_RUNNER_VOLUME_FETCH_TIMEOUT", &cfg.VolumeFetchTimeout); err != nil {
		return nil, err
	}

	if err := loadCode(cfg); err != nil {
		return nil, err
	}

	envvar.String("SANDBOX_RUNNER_LISTEN_ADDR", &cfg.ListenAddr)

	if err := envvar.Bool("SANDBOX_RUNNER_METRICS_ENABLED", &cfg.MetricsEnabled); err != nil {
		return nil, err
	}

	if err := envvar.IntBetween("SANDBOX_RUNNER_MAX_INFLIGHT_PER_SANDBOX", &cfg.MaxInflightPerSandbox, 0, math.MaxInt32); err != nil {
		return nil, err
	}

	// A request that leaves timeout_ms out runs for the daemon's default, so a
	// cap below that default would not hold for it.
	if v := envvar.Get("SANDBOX_RUNNER_MAX_EXEC_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		// A value that rounds down to zero, such as 0.5ns, is not a request for no limit.
		roundedToZero := d == 0 && strings.ContainsAny(v, "123456789")
		if err != nil || d < 0 || roundedToZero || (d > 0 && d < daemon.DefaultExecTimeout) {
			return nil, fmt.Errorf("SANDBOX_RUNNER_MAX_EXEC_TIMEOUT must be 0 or a duration of at least %s (the default exec timeout), got %q", daemon.DefaultExecTimeout, v)
		}
		cfg.MaxExecTimeout = d
	}

	cfg.APIGRPCAddr = envvar.Get("SANDBOX_RUNNER_API_GRPC_ADDR")
	cfg.RegistrationToken = envvar.Get("SANDBOX_RUNNER_REGISTRATION_TOKEN")
	cfg.RunnerHTTPBaseURL = envvar.Get("SANDBOX_RUNNER_HTTP_BASE_URL")
	envvar.String("SANDBOX_RUNNER_ID", &cfg.RunnerID)

	if err := envvar.NonNegativeInt("SANDBOX_RUNNER_CAPACITY_TOTAL", &cfg.CapacityTotal); err != nil {
		return nil, err
	}
	cfg.CapacityTotalSet = envvar.Get("SANDBOX_RUNNER_CAPACITY_TOTAL") != ""

	cfg.GRPCServerCAFile = envvar.Get("SANDBOX_RUNNER_REGISTRATION_GRPC_CA_FILE")
	cfg.GRPCClientCertFile = envvar.Get("SANDBOX_RUNNER_REGISTRATION_GRPC_CERT_FILE")
	cfg.GRPCClientKeyFile = envvar.Get("SANDBOX_RUNNER_REGISTRATION_GRPC_KEY_FILE")
	cfg.GRPCServerName = envvar.Get("SANDBOX_RUNNER_REGISTRATION_GRPC_SERVER_NAME")

	tlsN := 0
	if cfg.GRPCServerCAFile != "" {
		tlsN++
	}
	if cfg.GRPCClientCertFile != "" {
		tlsN++
	}
	if cfg.GRPCClientKeyFile != "" {
		tlsN++
	}
	if tlsN != 3 {
		return nil, fmt.Errorf("SANDBOX_RUNNER_REGISTRATION_GRPC_CA_FILE, SANDBOX_RUNNER_REGISTRATION_GRPC_CERT_FILE, and SANDBOX_RUNNER_REGISTRATION_GRPC_KEY_FILE are required for registration mTLS")
	}

	if cfg.APIGRPCAddr == "" {
		return nil, fmt.Errorf("SANDBOX_RUNNER_API_GRPC_ADDR must be set")
	}
	if cfg.RegistrationToken == "" {
		return nil, fmt.Errorf("SANDBOX_RUNNER_REGISTRATION_TOKEN must be set")
	}
	if cfg.RunnerHTTPBaseURL == "" {
		return nil, fmt.Errorf("SANDBOX_RUNNER_HTTP_BASE_URL must be set")
	}
	// The HTTP listener serves TLS, and this URL is what the runner advertises
	// to the API in heartbeats, so it has to name https or the API would send
	// every request to the wrong scheme. The host has to be checked
	// separately: url.Parse reads
	// "https:runner:8080" as an opaque path and "https://" as empty, both with
	// scheme https and no host, and either would leave the runner advertising
	// an address nothing can dial and no host to derive the control gRPC
	// advertise address from.
	u, err := url.Parse(cfg.RunnerHTTPBaseURL)
	// http:// is upgraded rather than refused. Deployments written before the
	// listener served TLS still carry it, and refusing here exits before
	// registration, so the operator sees a runner missing from the API rather
	// than a scheme they can fix. Only the scheme changes, so certificate
	// coverage is unaffected: a host the runner's certificate does not cover
	// fails the first request with a TLS error either way.
	if err == nil && u.Host != "" && strings.EqualFold(u.Scheme, "http") {
		u.Scheme = "https"
		slog.Warn(
			"SANDBOX_RUNNER_HTTP_BASE_URL uses http://, which the runner HTTP listener does not serve; advertising https:// instead. Update the configuration.",
			"configured", cfg.RunnerHTTPBaseURL,
			"advertising", u.String(),
		)
		cfg.RunnerHTTPBaseURL = u.String()
	}
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Hostname() == "" {
		return nil, fmt.Errorf("SANDBOX_RUNNER_HTTP_BASE_URL must be an https:// URL with a host, got %q", cfg.RunnerHTTPBaseURL)
	}

	envvar.String("SANDBOX_RUNNER_CONTROL_GRPC_LISTEN_ADDR", &cfg.ControlGRPCListenAddr)
	cfg.ControlGRPCAdvertiseAddr = envvar.Get("SANDBOX_RUNNER_CONTROL_GRPC_ADVERTISE_ADDR")
	cfg.ControlGRPCServerCertFile = envvar.Get("SANDBOX_RUNNER_CONTROL_GRPC_TLS_CERT_FILE")
	cfg.ControlGRPCServerKeyFile = envvar.Get("SANDBOX_RUNNER_CONTROL_GRPC_TLS_KEY_FILE")
	cfg.ControlGRPCClientCAFile = envvar.Get("SANDBOX_RUNNER_CONTROL_GRPC_TLS_CLIENT_CA_FILE")
	if err := validateListenAddr(cfg.ControlGRPCListenAddr); err != nil {
		return nil, fmt.Errorf("SANDBOX_RUNNER_CONTROL_GRPC_LISTEN_ADDR must be host:port (e.g. :9091), got %q: %w", cfg.ControlGRPCListenAddr, err)
	}
	if cfg.ControlGRPCAdvertiseAddr != "" {
		if err := validateHostPort(cfg.ControlGRPCAdvertiseAddr); err != nil {
			return nil, fmt.Errorf("SANDBOX_RUNNER_CONTROL_GRPC_ADVERTISE_ADDR must be host:port, got %q: %w", cfg.ControlGRPCAdvertiseAddr, err)
		}
	}

	if cfg.ResolvedControlGRPCAdvertiseAddr() == "" {
		return nil, fmt.Errorf("SANDBOX_RUNNER_CONTROL_GRPC_ADVERTISE_ADDR or SANDBOX_RUNNER_HTTP_BASE_URL must resolve a control gRPC host:port")
	}
	ctlN := 0
	if cfg.ControlGRPCServerCertFile != "" {
		ctlN++
	}
	if cfg.ControlGRPCServerKeyFile != "" {
		ctlN++
	}
	if cfg.ControlGRPCClientCAFile != "" {
		ctlN++
	}
	if ctlN != 3 {
		return nil, fmt.Errorf("SANDBOX_RUNNER_CONTROL_GRPC_TLS_CERT_FILE, SANDBOX_RUNNER_CONTROL_GRPC_TLS_KEY_FILE, and SANDBOX_RUNNER_CONTROL_GRPC_TLS_CLIENT_CA_FILE are required for control-plane mTLS")
	}

	return cfg, nil
}
