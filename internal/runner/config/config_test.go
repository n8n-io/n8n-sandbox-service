package config

import (
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SANDBOX_RUNNER_API_KEYS", "test-key")
	t.Setenv("SANDBOX_RUNNER_API_GRPC_ADDR", "api:9090")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_TOKEN", "reg-token")
	t.Setenv("SANDBOX_RUNNER_HTTP_BASE_URL", "https://runner:8080")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_CA_FILE", "/tmp/reg-ca.crt")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_CERT_FILE", "/tmp/reg.crt")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_KEY_FILE", "/tmp/reg.key")
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_CERT_FILE", "/tmp/control.crt")
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_KEY_FILE", "/tmp/control.key")
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_CLIENT_CA_FILE", "/tmp/control-ca.crt")
}

func TestLoadParsesDefaults(t *testing.T) {
	setRequiredEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if cfg.MaxFileBytes != 10*1024*1024 {
		t.Errorf("expected MaxFileBytes 10MB, got %d", cfg.MaxFileBytes)
	}
	if cfg.DataDir != "/var/sandboxes" {
		t.Errorf("expected DataDir /var/sandboxes, got %s", cfg.DataDir)
	}
	if cfg.ListenAddr != ":8080" {
		t.Errorf("expected ListenAddr :8080, got %s", cfg.ListenAddr)
	}
	if cfg.CapacityTotal != defaultRunnerCapacityTotal {
		t.Errorf("expected CapacityTotal %d, got %d", defaultRunnerCapacityTotal, cfg.CapacityTotal)
	}
	if cfg.ControlGRPCListenAddr != ":9091" {
		t.Errorf("expected ControlGRPCListenAddr :9091, got %s", cfg.ControlGRPCListenAddr)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("expected LogLevel info, got %v", cfg.LogLevel)
	}
	if _, exists := cfg.APIKeys["test-key"]; !exists {
		t.Error("expected test-key in APIKeys")
	}
}

func TestLoadRequiresAPIKeys(t *testing.T) {
	os.Unsetenv("SANDBOX_RUNNER_API_KEYS")

	if _, err := Load(); err == nil {
		t.Error("expected Load() to fail without SANDBOX_RUNNER_API_KEYS")
	}
}

func TestLoadRejectsPartialGRPCTLS(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_CA_FILE", "/tmp/ca.crt")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_CERT_FILE", "")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_KEY_FILE", "")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_SERVER_NAME", "")

	if _, err := Load(); err == nil {
		t.Fatal("expected Load to reject partial SANDBOX_RUNNER_REGISTRATION_GRPC_*")
	}
}

func TestLoadRejectsPartialControlGRPCTLS(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_LISTEN_ADDR", ":9091")
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_CERT_FILE", "/tmp/srv.crt")
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_KEY_FILE", "")
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_CLIENT_CA_FILE", "")

	if _, err := Load(); err == nil {
		t.Fatal("expected Load to reject partial SANDBOX_RUNNER_CONTROL_GRPC_TLS_*")
	}
}

// Deployments written before the runner's HTTP listener served TLS still set
// http://. Load upgrades the scheme instead of exiting, so those runners keep
// registering; only the scheme changes, so the host the API verifies the
// runner's certificate against is the one that was configured.
func TestLoadUpgradesPlaintextHTTPBaseURL(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("SANDBOX_RUNNER_HTTP_BASE_URL", "http://runner:8080")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("expected Load to upgrade an http:// SANDBOX_RUNNER_HTTP_BASE_URL, got: %v", err)
	}
	if cfg.RunnerHTTPBaseURL != "https://runner:8080" {
		t.Fatalf("expected https://runner:8080, got %q", cfg.RunnerHTTPBaseURL)
	}
	// The control gRPC advertise address is derived from this URL, so the
	// upgrade must not disturb the host or it would be advertised wrong too.
	if got := cfg.ResolvedControlGRPCAdvertiseAddr(); got != "runner:9091" {
		t.Fatalf("expected the upgrade to leave the control gRPC advertise address at runner:9091, got %q", got)
	}
}

// url.Parse reports a scheme for several forms that carry no host, so a
// scheme-only check would let the runner boot advertising a base URL nothing
// can dial.
//
// Load rejects every form below at the host check itself. The advertise
// address and the message match are here for the case where that check is
// removed: these inputs are then still refused further down, where the control
// gRPC address can no longer be derived from the URL, and that error names
// SANDBOX_RUNNER_HTTP_BASE_URL as well, so a bare "did it fail" assertion would
// not notice the loss. Setting the address removes that second net, and
// matching the message pins which check spoke. The Helm chart and the e2e
// scripts set the address too, so it also matches how runners really run.
func TestLoadRejectsHTTPBaseURLWithoutHost(t *testing.T) {
	for _, base := range []string{
		"https:runner:8080",
		"https://",
		"https:",
		"https:///files",
		// The http:// upgrade must not become a way around the host check.
		"http:runner:8080",
		"http://",
		"http:",
		"http:///files",
	} {
		t.Run(base, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv("SANDBOX_RUNNER_HTTP_BASE_URL", base)
			t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_ADVERTISE_ADDR", "runner:9091")

			_, err := Load()
			if err == nil {
				t.Fatalf("expected Load to reject host-less SANDBOX_RUNNER_HTTP_BASE_URL %q", base)
			}
			if !strings.Contains(err.Error(), "must be an https:// URL with a host") {
				t.Fatalf("expected the host check to reject %q, got: %v", base, err)
			}
		})
	}
}

func TestLoadRejectsInvalidControlGRPCAdvertiseAddr(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_ADVERTISE_ADDR", "runner-without-port")

	if _, err := Load(); err == nil {
		t.Fatal("expected Load to reject invalid SANDBOX_RUNNER_CONTROL_GRPC_ADVERTISE_ADDR")
	}
}

func TestLoadParsesLogLevel(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("SANDBOX_RUNNER_LOG_LEVEL", "debug")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("expected LogLevel debug, got %v", cfg.LogLevel)
	}
}

func TestLoadRejectsInvalidLogLevel(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("SANDBOX_RUNNER_LOG_LEVEL", "trace")

	if _, err := Load(); err == nil {
		t.Fatal("expected Load to reject invalid SANDBOX_RUNNER_LOG_LEVEL")
	}
}

func TestLoadRejectsInvalidControlGRPCListenAddr(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_LISTEN_ADDR", "not-an-addr")

	if _, err := Load(); err == nil {
		t.Fatal("expected Load to reject invalid SANDBOX_RUNNER_CONTROL_GRPC_LISTEN_ADDR")
	}
}

func TestLoadRequestLimitDefaults(t *testing.T) {
	setRequiredEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if cfg.MaxInflightPerSandbox != 16 {
		t.Errorf("MaxInflightPerSandbox = %d, want 16", cfg.MaxInflightPerSandbox)
	}
	if cfg.MaxExecTimeout != 15*time.Minute {
		t.Errorf("MaxExecTimeout = %s, want 15m", cfg.MaxExecTimeout)
	}
}

func TestLoadRequestLimitsParseAndAllowZero(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("SANDBOX_RUNNER_MAX_INFLIGHT_PER_SANDBOX", "0")
	t.Setenv("SANDBOX_RUNNER_MAX_EXEC_TIMEOUT", "0")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if cfg.MaxInflightPerSandbox != 0 || cfg.MaxExecTimeout != 0 {
		t.Errorf("limits = %d/%s, want 0/0s", cfg.MaxInflightPerSandbox, cfg.MaxExecTimeout)
	}
}

func TestLoadRejectsInvalidRequestLimit(t *testing.T) {
	for _, value := range []string{"-1", "lots", "2147483648"} {
		t.Run(value, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv("SANDBOX_RUNNER_MAX_INFLIGHT_PER_SANDBOX", value)
			if _, err := Load(); err == nil {
				t.Fatalf("expected Load() to reject SANDBOX_RUNNER_MAX_INFLIGHT_PER_SANDBOX=%s", value)
			}
		})
	}
}

// Below the daemon's 5m default, a request that leaves timeout_ms out would
// run longer than the cap allows one that sets it. A positive value that rounds
// down to zero must not be read as 0, which turns the cap off.
func TestLoadRejectsInvalidMaxExecTimeout(t *testing.T) {
	for _, value := range []string{"-1m", "forever", "1s", "4m59s", "0.5ns"} {
		t.Run(value, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv("SANDBOX_RUNNER_MAX_EXEC_TIMEOUT", value)
			if _, err := Load(); err == nil {
				t.Fatalf("expected Load() to reject SANDBOX_RUNNER_MAX_EXEC_TIMEOUT=%s", value)
			}
		})
	}

	for value, want := range map[string]time.Duration{"5m": 5 * time.Minute, "0": 0, "0s": 0} {
		t.Run("accepts "+value, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv("SANDBOX_RUNNER_MAX_EXEC_TIMEOUT", value)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() with %s failed: %v", value, err)
			}
			if cfg.MaxExecTimeout != want {
				t.Errorf("MaxExecTimeout = %s, want %s", cfg.MaxExecTimeout, want)
			}
		})
	}
}
