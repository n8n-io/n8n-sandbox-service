package config

import (
	"os"
	"testing"
	"time"
)

func setRequiredGRPCMTLS(t *testing.T) {
	t.Helper()
	t.Setenv("SANDBOX_API_GRPC_TLS_CERT_FILE", "/tmp/api-grpc.crt")
	t.Setenv("SANDBOX_API_GRPC_TLS_KEY_FILE", "/tmp/api-grpc.key")
	t.Setenv("SANDBOX_API_GRPC_TLS_CLIENT_CA_FILE", "/tmp/api-grpc-ca.crt")
	t.Setenv("SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_CA_FILE", "/tmp/control-ca.crt")
	t.Setenv("SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_CERT_FILE", "/tmp/control-client.crt")
	t.Setenv("SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_KEY_FILE", "/tmp/control-client.key")
}

func TestLoadAPIParsesDefaults(t *testing.T) {
	t.Setenv("SANDBOX_API_KEYS", "test-key")
	t.Setenv("SANDBOX_API_RUNNER_REGISTRATION_TOKEN", "reg-token")
	setRequiredGRPCMTLS(t)

	cfg, err := LoadAPI()
	if err != nil {
		t.Fatalf("LoadAPI() failed: %v", err)
	}

	if cfg.ListenAddr != ":8080" {
		t.Errorf("expected ListenAddr :8080, got %s", cfg.ListenAddr)
	}

	if cfg.MaxFileBytes != 10*1024*1024 {
		t.Errorf("expected MaxFileBytes 10MB, got %d", cfg.MaxFileBytes)
	}

	if cfg.GRPCListenAddr != ":9090" {
		t.Errorf("expected GRPCListenAddr :9090, got %s", cfg.GRPCListenAddr)
	}

	if cfg.RegistrationToken != "reg-token" {
		t.Errorf("expected RegistrationToken reg-token, got %q", cfg.RegistrationToken)
	}

	if cfg.DataDir != "/var/lib/n8n-sandbox-api" {
		t.Errorf("expected DataDir /var/lib/n8n-sandbox-api, got %s", cfg.DataDir)
	}

	if _, exists := cfg.APIKeys["test-key"]; !exists {
		t.Error("expected test-key in APIKeys")
	}

	if cfg.HeartbeatGrace != 45*time.Second {
		t.Errorf("expected HeartbeatGrace 45s, got %s", cfg.HeartbeatGrace)
	}

	if cfg.IdleStopAfter != time.Hour {
		t.Errorf("expected IdleStopAfter 1h, got %s", cfg.IdleStopAfter)
	}

	if cfg.IdleDeleteAfter != 24*time.Hour {
		t.Errorf("expected IdleDeleteAfter 24h, got %s", cfg.IdleDeleteAfter)
	}

	if cfg.IdleDeleteSafetyBuffer != time.Minute {
		t.Errorf("expected IdleDeleteSafetyBuffer 1m, got %s", cfg.IdleDeleteSafetyBuffer)
	}
}

func TestLoadAPIHeartbeatGraceFromEnv(t *testing.T) {
	t.Setenv("SANDBOX_API_KEYS", "test-key")
	t.Setenv("SANDBOX_API_RUNNER_REGISTRATION_TOKEN", "reg-token")
	t.Setenv("SANDBOX_API_RUNNER_HEARTBEAT_GRACE", "90s")
	setRequiredGRPCMTLS(t)

	cfg, err := LoadAPI()
	if err != nil {
		t.Fatalf("LoadAPI() failed: %v", err)
	}
	if cfg.HeartbeatGrace != 90*time.Second {
		t.Fatalf("HeartbeatGrace: want 90s, got %s", cfg.HeartbeatGrace)
	}
}

func TestLoadAPIRejectsInvalidHeartbeatGrace(t *testing.T) {
	t.Setenv("SANDBOX_API_KEYS", "test-key")
	t.Setenv("SANDBOX_API_RUNNER_REGISTRATION_TOKEN", "reg-token")
	t.Setenv("SANDBOX_API_RUNNER_HEARTBEAT_GRACE", "0s")
	setRequiredGRPCMTLS(t)

	if _, err := LoadAPI(); err == nil {
		t.Fatal("expected LoadAPI to reject SANDBOX_API_RUNNER_HEARTBEAT_GRACE=0s")
	}
}

func TestLoadAPIRequiresAPIKeys(t *testing.T) {
	os.Unsetenv("SANDBOX_API_KEYS")
	os.Setenv("SANDBOX_API_RUNNER_REGISTRATION_TOKEN", "x")
	defer os.Unsetenv("SANDBOX_API_RUNNER_REGISTRATION_TOKEN")

	if _, err := LoadAPI(); err == nil {
		t.Error("expected LoadAPI() to fail without SANDBOX_API_KEYS")
	}
}

func TestLoadAPIRejectsPartialGRPCTLS(t *testing.T) {
	t.Setenv("SANDBOX_API_KEYS", "test-key")
	t.Setenv("SANDBOX_API_RUNNER_REGISTRATION_TOKEN", "reg-token")
	t.Setenv("SANDBOX_API_GRPC_TLS_CERT_FILE", "/tmp/x.crt")
	t.Setenv("SANDBOX_API_GRPC_TLS_KEY_FILE", "")
	t.Setenv("SANDBOX_API_GRPC_TLS_CLIENT_CA_FILE", "")
	t.Setenv("SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_CA_FILE", "/tmp/control-ca.crt")
	t.Setenv("SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_CERT_FILE", "/tmp/control-client.crt")
	t.Setenv("SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_KEY_FILE", "/tmp/control-client.key")

	if _, err := LoadAPI(); err == nil {
		t.Fatal("expected LoadAPI to reject partial SANDBOX_API_GRPC_TLS_*")
	}
}

func TestLoadAPIRejectsPartialRunnerControlGRPCTLS(t *testing.T) {
	t.Setenv("SANDBOX_API_KEYS", "test-key")
	t.Setenv("SANDBOX_API_RUNNER_REGISTRATION_TOKEN", "reg-token")
	t.Setenv("SANDBOX_API_GRPC_TLS_CERT_FILE", "/tmp/api-grpc.crt")
	t.Setenv("SANDBOX_API_GRPC_TLS_KEY_FILE", "/tmp/api-grpc.key")
	t.Setenv("SANDBOX_API_GRPC_TLS_CLIENT_CA_FILE", "/tmp/api-grpc-ca.crt")
	t.Setenv("SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_CA_FILE", "/tmp/ca.crt")
	t.Setenv("SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_CERT_FILE", "")
	t.Setenv("SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_KEY_FILE", "")

	if _, err := LoadAPI(); err == nil {
		t.Fatal("expected LoadAPI to reject partial SANDBOX_API_RUNNER_CONTROL_GRPC_TLS_*")
	}
}

func TestLoadAPIRequiresRegistrationToken(t *testing.T) {
	os.Setenv("SANDBOX_API_KEYS", "test-key")
	os.Unsetenv("SANDBOX_API_RUNNER_REGISTRATION_TOKEN")
	defer os.Unsetenv("SANDBOX_API_KEYS")

	if _, err := LoadAPI(); err == nil {
		t.Error("expected LoadAPI() to fail without SANDBOX_API_RUNNER_REGISTRATION_TOKEN")
	}
}

func TestLoadAPIIdleDeleteAfterZeroDisablesDelete(t *testing.T) {
	t.Setenv("SANDBOX_API_KEYS", "test-key")
	t.Setenv("SANDBOX_API_RUNNER_REGISTRATION_TOKEN", "reg-token")
	t.Setenv("SANDBOX_API_IDLE_STOP_AFTER", "1h")
	t.Setenv("SANDBOX_API_IDLE_DELETE_AFTER", "0")
	setRequiredGRPCMTLS(t)

	cfg, err := LoadAPI()
	if err != nil {
		t.Fatalf("LoadAPI() failed: %v", err)
	}
	if cfg.IdleStopAfter != time.Hour {
		t.Fatalf("IdleStopAfter: want 1h, got %s", cfg.IdleStopAfter)
	}
	if cfg.IdleDeleteAfter != 0 {
		t.Fatalf("IdleDeleteAfter: want 0, got %s", cfg.IdleDeleteAfter)
	}
}

func TestLoadAPIIdleTTLZeroDisablesDefaults(t *testing.T) {
	t.Setenv("SANDBOX_API_KEYS", "test-key")
	t.Setenv("SANDBOX_API_RUNNER_REGISTRATION_TOKEN", "reg-token")
	t.Setenv("SANDBOX_API_IDLE_STOP_AFTER", "0")
	t.Setenv("SANDBOX_API_IDLE_DELETE_AFTER", "0")
	setRequiredGRPCMTLS(t)

	cfg, err := LoadAPI()
	if err != nil {
		t.Fatalf("LoadAPI() failed: %v", err)
	}
	if cfg.IdleStopAfter != 0 {
		t.Fatalf("IdleStopAfter: want 0, got %s", cfg.IdleStopAfter)
	}
	if cfg.IdleDeleteAfter != 0 {
		t.Fatalf("IdleDeleteAfter: want 0, got %s", cfg.IdleDeleteAfter)
	}
	if cfg.IdleDeleteSafetyBuffer != 0 {
		t.Fatalf("IdleDeleteSafetyBuffer: want 0, got %s", cfg.IdleDeleteSafetyBuffer)
	}
}

func TestLoadAPIRejectsNegativeIdleDeleteAfter(t *testing.T) {
	t.Setenv("SANDBOX_API_KEYS", "test-key")
	t.Setenv("SANDBOX_API_RUNNER_REGISTRATION_TOKEN", "reg-token")
	t.Setenv("SANDBOX_API_IDLE_DELETE_AFTER", "-1s")
	setRequiredGRPCMTLS(t)

	if _, err := LoadAPI(); err == nil {
		t.Fatal("expected LoadAPI to reject negative SANDBOX_API_IDLE_DELETE_AFTER")
	}
}
