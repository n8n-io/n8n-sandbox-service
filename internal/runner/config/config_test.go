package config

import (
	"os"
	"testing"
)

func TestLoadParsesDefaults(t *testing.T) {
	os.Setenv("SANDBOX_RUNNER_API_KEYS", "test-key")
	os.Setenv("SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE", "test-image")
	os.Setenv("SANDBOX_RUNNER_API_GRPC_ADDR", "api:9090")
	os.Setenv("SANDBOX_RUNNER_REGISTRATION_TOKEN", "reg-token")
	os.Setenv("SANDBOX_RUNNER_HTTP_BASE_URL", "http://runner:8080")
	os.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_CA_FILE", "/tmp/reg-ca.crt")
	os.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_CERT_FILE", "/tmp/reg.crt")
	os.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_KEY_FILE", "/tmp/reg.key")
	os.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_CERT_FILE", "/tmp/control.crt")
	os.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_KEY_FILE", "/tmp/control.key")
	os.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_CLIENT_CA_FILE", "/tmp/control-ca.crt")
	defer func() {
		os.Unsetenv("SANDBOX_RUNNER_API_KEYS")
		os.Unsetenv("SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE")
		os.Unsetenv("SANDBOX_RUNNER_API_GRPC_ADDR")
		os.Unsetenv("SANDBOX_RUNNER_REGISTRATION_TOKEN")
		os.Unsetenv("SANDBOX_RUNNER_HTTP_BASE_URL")
		os.Unsetenv("SANDBOX_RUNNER_REGISTRATION_GRPC_CA_FILE")
		os.Unsetenv("SANDBOX_RUNNER_REGISTRATION_GRPC_CERT_FILE")
		os.Unsetenv("SANDBOX_RUNNER_REGISTRATION_GRPC_KEY_FILE")
		os.Unsetenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_CERT_FILE")
		os.Unsetenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_KEY_FILE")
		os.Unsetenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_CLIENT_CA_FILE")
	}()

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

	if cfg.DockerHost != "unix:///var/run/docker.sock" {
		t.Errorf("expected DockerHost unix:///var/run/docker.sock, got %s", cfg.DockerHost)
	}

	if cfg.DockerSandboxImage != "test-image" {
		t.Errorf("expected DockerSandboxImage test-image, got %s", cfg.DockerSandboxImage)
	}

	if cfg.DefaultMemoryMB != 512 {
		t.Errorf("expected DefaultMemoryMB 512, got %d", cfg.DefaultMemoryMB)
	}

	if cfg.DefaultCPUPercent != 100 {
		t.Errorf("expected DefaultCPUPercent 100, got %d", cfg.DefaultCPUPercent)
	}

	if cfg.DefaultPidsMax != 256 {
		t.Errorf("expected DefaultPidsMax 256, got %d", cfg.DefaultPidsMax)
	}

	if cfg.DefaultDiskMB != 0 {
		t.Errorf("expected DefaultDiskMB 0 (no quota by default), got %d", cfg.DefaultDiskMB)
	}

	if cfg.DiskQuotaActive {
		t.Error("expected DiskQuotaActive false by default")
	}

	if !cfg.EnableCgroups {
		t.Error("expected EnableCgroups true")
	}

	if cfg.InterSandboxNetworkEnabled {
		t.Error("expected InterSandboxNetworkEnabled false")
	}

	if cfg.CapacityTotal != defaultRunnerCapacityTotal {
		t.Errorf("expected CapacityTotal %d, got %d", defaultRunnerCapacityTotal, cfg.CapacityTotal)
	}
	if cfg.ControlGRPCListenAddr != ":9091" {
		t.Errorf("expected ControlGRPCListenAddr :9091, got %s", cfg.ControlGRPCListenAddr)
	}

	if _, exists := cfg.APIKeys["test-key"]; !exists {
		t.Error("expected test-key in APIKeys")
	}
}

func TestLoadParsesDiskQuotaEnv(t *testing.T) {
	t.Setenv("SANDBOX_RUNNER_API_KEYS", "test-key")
	t.Setenv("SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE", "test-image")
	t.Setenv("SANDBOX_RUNNER_API_GRPC_ADDR", "api:9090")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_TOKEN", "reg-token")
	t.Setenv("SANDBOX_RUNNER_HTTP_BASE_URL", "http://runner:8080")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_CA_FILE", "/tmp/reg-ca.crt")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_CERT_FILE", "/tmp/reg.crt")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_KEY_FILE", "/tmp/reg.key")
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_CERT_FILE", "/tmp/control.crt")
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_KEY_FILE", "/tmp/control.key")
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_CLIENT_CA_FILE", "/tmp/control-ca.crt")
	t.Setenv("SANDBOX_RUNNER_DEFAULT_DISK_MB", "2048")
	t.Setenv("SANDBOX_RUNNER_DISK_QUOTA_ACTIVE", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if cfg.DefaultDiskMB != 2048 {
		t.Errorf("expected DefaultDiskMB 2048, got %d", cfg.DefaultDiskMB)
	}
	if !cfg.DiskQuotaActive {
		t.Error("expected DiskQuotaActive true")
	}
}

func TestLoadRejectsInvalidDefaultDiskMB(t *testing.T) {
	t.Setenv("SANDBOX_RUNNER_API_KEYS", "test-key")
	t.Setenv("SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE", "test-image")
	t.Setenv("SANDBOX_RUNNER_API_GRPC_ADDR", "api:9090")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_TOKEN", "reg-token")
	t.Setenv("SANDBOX_RUNNER_HTTP_BASE_URL", "http://runner:8080")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_CA_FILE", "/tmp/reg-ca.crt")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_CERT_FILE", "/tmp/reg.crt")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_KEY_FILE", "/tmp/reg.key")
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_CERT_FILE", "/tmp/control.crt")
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_KEY_FILE", "/tmp/control.key")
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_CLIENT_CA_FILE", "/tmp/control-ca.crt")
	t.Setenv("SANDBOX_RUNNER_DEFAULT_DISK_MB", "not-a-number")

	if _, err := Load(); err == nil {
		t.Fatal("expected Load to reject non-numeric SANDBOX_RUNNER_DEFAULT_DISK_MB")
	}
}

func TestLoadRequiresAPIKeys(t *testing.T) {
	os.Unsetenv("SANDBOX_RUNNER_API_KEYS")
	os.Setenv("SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE", "test-image")
	defer os.Unsetenv("SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE")

	if _, err := Load(); err == nil {
		t.Error("expected Load() to fail without SANDBOX_RUNNER_API_KEYS")
	}
}

func TestLoadRequiresSandboxImage(t *testing.T) {
	os.Setenv("SANDBOX_RUNNER_API_KEYS", "test-key")
	os.Unsetenv("SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE")
	defer os.Unsetenv("SANDBOX_RUNNER_API_KEYS")

	if _, err := Load(); err == nil {
		t.Error("expected Load() to fail without SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE")
	}
}

func TestLoadRejectsPartialGRPCTLS(t *testing.T) {
	t.Setenv("SANDBOX_RUNNER_API_KEYS", "test-key")
	t.Setenv("SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE", "img")
	t.Setenv("SANDBOX_RUNNER_API_GRPC_ADDR", "api:9090")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_TOKEN", "reg-token")
	t.Setenv("SANDBOX_RUNNER_HTTP_BASE_URL", "http://runner:8080")
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_CERT_FILE", "/tmp/control.crt")
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_KEY_FILE", "/tmp/control.key")
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_CLIENT_CA_FILE", "/tmp/control-ca.crt")
	// Isolate from the process environment: inherited CERT+KEY would satisfy mTLS and make this test fail.
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_CA_FILE", "/tmp/ca.crt")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_CERT_FILE", "")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_KEY_FILE", "")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_SERVER_NAME", "")

	if _, err := Load(); err == nil {
		t.Fatal("expected Load to reject partial SANDBOX_RUNNER_REGISTRATION_GRPC_*")
	}
}

func TestLoadRejectsPartialControlGRPCTLS(t *testing.T) {
	t.Setenv("SANDBOX_RUNNER_API_KEYS", "test-key")
	t.Setenv("SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE", "img")
	t.Setenv("SANDBOX_RUNNER_API_GRPC_ADDR", "api:9090")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_TOKEN", "reg-token")
	t.Setenv("SANDBOX_RUNNER_HTTP_BASE_URL", "http://runner:8080")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_CA_FILE", "/tmp/reg-ca.crt")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_CERT_FILE", "/tmp/reg.crt")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_KEY_FILE", "/tmp/reg.key")
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_LISTEN_ADDR", ":9091")
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_CERT_FILE", "/tmp/srv.crt")
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_KEY_FILE", "")
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_CLIENT_CA_FILE", "")

	if _, err := Load(); err == nil {
		t.Fatal("expected Load to reject partial SANDBOX_RUNNER_CONTROL_GRPC_TLS_*")
	}
}

func TestLoadRejectsInvalidControlGRPCAdvertiseAddr(t *testing.T) {
	t.Setenv("SANDBOX_RUNNER_API_KEYS", "test-key")
	t.Setenv("SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE", "img")
	t.Setenv("SANDBOX_RUNNER_API_GRPC_ADDR", "api:9090")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_TOKEN", "reg-token")
	t.Setenv("SANDBOX_RUNNER_HTTP_BASE_URL", "http://runner:8080")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_CA_FILE", "/tmp/reg-ca.crt")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_CERT_FILE", "/tmp/reg.crt")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_KEY_FILE", "/tmp/reg.key")
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_CERT_FILE", "/tmp/control.crt")
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_KEY_FILE", "/tmp/control.key")
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_CLIENT_CA_FILE", "/tmp/control-ca.crt")
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_ADVERTISE_ADDR", "runner-without-port")

	if _, err := Load(); err == nil {
		t.Fatal("expected Load to reject invalid SANDBOX_RUNNER_CONTROL_GRPC_ADVERTISE_ADDR")
	}
}

func TestLoadRejectsInvalidControlGRPCListenAddr(t *testing.T) {
	t.Setenv("SANDBOX_RUNNER_API_KEYS", "test-key")
	t.Setenv("SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE", "img")
	t.Setenv("SANDBOX_RUNNER_API_GRPC_ADDR", "api:9090")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_TOKEN", "reg-token")
	t.Setenv("SANDBOX_RUNNER_HTTP_BASE_URL", "http://runner:8080")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_CA_FILE", "/tmp/reg-ca.crt")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_CERT_FILE", "/tmp/reg.crt")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_KEY_FILE", "/tmp/reg.key")
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_CERT_FILE", "/tmp/control.crt")
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_KEY_FILE", "/tmp/control.key")
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_CLIENT_CA_FILE", "/tmp/control-ca.crt")
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_LISTEN_ADDR", "not-an-addr")

	if _, err := Load(); err == nil {
		t.Fatal("expected Load to reject invalid SANDBOX_RUNNER_CONTROL_GRPC_LISTEN_ADDR")
	}
}
