package config

import (
	"log/slog"
	"os"
	"testing"
)

func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SANDBOX_RUNNER_API_KEYS", "test-key")
	t.Setenv("SANDBOX_RUNNER_API_GRPC_ADDR", "api:9090")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_TOKEN", "reg-token")
	t.Setenv("SANDBOX_RUNNER_HTTP_BASE_URL", "http://runner:8080")
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

	if cfg.IdleTTLSeconds != 3600 {
		t.Errorf("expected IdleTTLSeconds 3600, got %d", cfg.IdleTTLSeconds)
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
