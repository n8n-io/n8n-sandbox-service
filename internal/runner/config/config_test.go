package config

import (
	"log/slog"
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

	if cfg.Backend != BackendDocker {
		t.Errorf("expected Backend docker, got %s", cfg.Backend)
	}

	if cfg.Docker.Host != "unix:///var/run/docker.sock" {
		t.Errorf("expected DockerHost unix:///var/run/docker.sock, got %s", cfg.Docker.Host)
	}

	if cfg.Docker.SandboxImage != "test-image" {
		t.Errorf("expected DockerSandboxImage test-image, got %s", cfg.Docker.SandboxImage)
	}

	if cfg.Docker.DefaultMemoryMB != 512 {
		t.Errorf("expected DefaultMemoryMB 512, got %d", cfg.Docker.DefaultMemoryMB)
	}

	if cfg.Docker.DefaultCPUPercent != 100 {
		t.Errorf("expected DefaultCPUPercent 100, got %d", cfg.Docker.DefaultCPUPercent)
	}

	if cfg.Docker.DefaultPidsMax != 256 {
		t.Errorf("expected DefaultPidsMax 256, got %d", cfg.Docker.DefaultPidsMax)
	}

	if cfg.Docker.DefaultDiskQuotaMB != 0 {
		t.Errorf("expected DefaultDiskQuotaMB 0 (no quota by default), got %d", cfg.Docker.DefaultDiskQuotaMB)
	}

	if cfg.Docker.DiskQuotaActive {
		t.Error("expected DiskQuotaActive false by default")
	}

	if !cfg.Docker.EnableCgroups {
		t.Error("expected EnableCgroups true")
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
	t.Setenv("SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB", "2048")
	t.Setenv("SANDBOX_RUNNER_DISK_QUOTA_ACTIVE", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if cfg.Docker.DefaultDiskQuotaMB != 2048 {
		t.Errorf("expected DefaultDiskQuotaMB 2048, got %d", cfg.Docker.DefaultDiskQuotaMB)
	}
	if !cfg.Docker.DiskQuotaActive {
		t.Error("expected DiskQuotaActive true")
	}
}

func TestLoadAcceptsZeroDefaultDiskQuotaMB(t *testing.T) {
	// Explicit 0 means "no quota", matching how scripts/start-runner.sh
	// treats an unset value. Rejecting 0 would make the two layers disagree.
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
	t.Setenv("SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB", "0")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() rejected explicit 0: %v", err)
	}
	if cfg.Docker.DefaultDiskQuotaMB != 0 {
		t.Errorf("expected DefaultDiskQuotaMB 0, got %d", cfg.Docker.DefaultDiskQuotaMB)
	}
}

func TestLoadRejectsNegativeDefaultDiskQuotaMB(t *testing.T) {
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
	t.Setenv("SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB", "-1")

	if _, err := Load(); err == nil {
		t.Fatal("expected Load to reject negative SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB")
	}
}

func TestLoadRejectsInvalidDefaultDiskQuotaMB(t *testing.T) {
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
	t.Setenv("SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB", "not-a-number")

	if _, err := Load(); err == nil {
		t.Fatal("expected Load to reject non-numeric SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB")
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

func TestLoadParsesFirecrackerBackendWithoutDockerImage(t *testing.T) {
	t.Setenv("SANDBOX_RUNNER_BACKEND", "firecracker")
	t.Setenv("SANDBOX_RUNNER_API_KEYS", "test-key")
	t.Setenv("SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE", "")
	t.Setenv("SANDBOX_RUNNER_API_GRPC_ADDR", "api:9090")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_TOKEN", "reg-token")
	t.Setenv("SANDBOX_RUNNER_HTTP_BASE_URL", "http://runner:8080")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_CA_FILE", "/tmp/reg-ca.crt")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_CERT_FILE", "/tmp/reg.crt")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_KEY_FILE", "/tmp/reg.key")
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_CERT_FILE", "/tmp/control.crt")
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_KEY_FILE", "/tmp/control.key")
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_CLIENT_CA_FILE", "/tmp/control-ca.crt")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if cfg.Backend != BackendFirecracker {
		t.Errorf("expected Backend firecracker, got %s", cfg.Backend)
	}
	if cfg.Firecracker.ProxyListenIP != "127.0.0.1" {
		t.Errorf("expected default Firecracker proxy listen IP, got %s", cfg.Firecracker.ProxyListenIP)
	}
	if cfg.Firecracker.ProxyPortStart != 18081 {
		t.Errorf("expected default Firecracker proxy port start 18081, got %d", cfg.Firecracker.ProxyPortStart)
	}
	if cfg.Firecracker.DaemonPort != 8081 {
		t.Errorf("expected default Firecracker daemon port 8081, got %d", cfg.Firecracker.DaemonPort)
	}
}

func TestLoadParsesFirecrackerConfig(t *testing.T) {
	t.Setenv("SANDBOX_RUNNER_BACKEND", "firecracker")
	t.Setenv("SANDBOX_RUNNER_API_KEYS", "test-key")
	t.Setenv("SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE", "")
	t.Setenv("SANDBOX_RUNNER_API_GRPC_ADDR", "api:9090")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_TOKEN", "reg-token")
	t.Setenv("SANDBOX_RUNNER_HTTP_BASE_URL", "http://runner:8080")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_CA_FILE", "/tmp/reg-ca.crt")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_CERT_FILE", "/tmp/reg.crt")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_KEY_FILE", "/tmp/reg.key")
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_CERT_FILE", "/tmp/control.crt")
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_KEY_FILE", "/tmp/control.key")
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_CLIENT_CA_FILE", "/tmp/control-ca.crt")
	t.Setenv("SANDBOX_RUNNER_CAPACITY_TOTAL", "4")
	t.Setenv("SANDBOX_RUNNER_FIRECRACKER_JAILER_BIN", "/custom/jailer")
	t.Setenv("SANDBOX_RUNNER_FIRECRACKER_BIN", "/custom/firecracker")
	t.Setenv("SANDBOX_RUNNER_FIRECRACKER_JAILER_BASE_DIR", "/custom/jailer-base")
	t.Setenv("SANDBOX_RUNNER_FIRECRACKER_TEMPLATE_DIR", "/custom/template")
	t.Setenv("SANDBOX_RUNNER_FIRECRACKER_SNAPSHOT_MEM_PATH", "/custom/mem")
	t.Setenv("SANDBOX_RUNNER_FIRECRACKER_SNAPSHOT_STATE_PATH", "/custom/state")
	t.Setenv("SANDBOX_RUNNER_FIRECRACKER_SNAPSHOT_VIRTIO_BLOCK_PATH", "/custom/rootfs.ext4")
	t.Setenv("SANDBOX_RUNNER_FIRECRACKER_GUEST_IP", "10.0.0.2")
	t.Setenv("SANDBOX_RUNNER_FIRECRACKER_HOST_TAP_DEVICE_NAME", "tap0")
	t.Setenv("SANDBOX_RUNNER_FIRECRACKER_HOST_TAP_IP_CIDR", "10.0.0.1/24")
	t.Setenv("SANDBOX_RUNNER_FIRECRACKER_DAEMON_PORT", "9090")
	t.Setenv("SANDBOX_RUNNER_FIRECRACKER_PROXY_LISTEN_IP", "127.0.0.2")
	t.Setenv("SANDBOX_RUNNER_FIRECRACKER_PROXY_PORT_START", "20000")
	t.Setenv("SANDBOX_RUNNER_FIRECRACKER_SOCKET_WAIT_ATTEMPTS", "9")
	t.Setenv("SANDBOX_RUNNER_FIRECRACKER_SOCKET_WAIT_INTERVAL_MS", "50")
	t.Setenv("SANDBOX_RUNNER_FIRECRACKER_DAEMON_WAIT_TIMEOUT", "10s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if cfg.Firecracker.JailerBin != "/custom/jailer" {
		t.Errorf("expected custom jailer bin, got %s", cfg.Firecracker.JailerBin)
	}
	if cfg.Firecracker.GuestIP != "10.0.0.2" {
		t.Errorf("expected guest IP 10.0.0.2, got %s", cfg.Firecracker.GuestIP)
	}
	if cfg.Firecracker.ProxyPortStart != 20000 {
		t.Errorf("expected proxy port start 20000, got %d", cfg.Firecracker.ProxyPortStart)
	}
}

func TestLoadRejectsFirecrackerProxyRangeOverflow(t *testing.T) {
	t.Setenv("SANDBOX_RUNNER_BACKEND", "firecracker")
	t.Setenv("SANDBOX_RUNNER_API_KEYS", "test-key")
	t.Setenv("SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE", "")
	t.Setenv("SANDBOX_RUNNER_API_GRPC_ADDR", "api:9090")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_TOKEN", "reg-token")
	t.Setenv("SANDBOX_RUNNER_HTTP_BASE_URL", "http://runner:8080")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_CA_FILE", "/tmp/reg-ca.crt")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_CERT_FILE", "/tmp/reg.crt")
	t.Setenv("SANDBOX_RUNNER_REGISTRATION_GRPC_KEY_FILE", "/tmp/reg.key")
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_CERT_FILE", "/tmp/control.crt")
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_KEY_FILE", "/tmp/control.key")
	t.Setenv("SANDBOX_RUNNER_CONTROL_GRPC_TLS_CLIENT_CA_FILE", "/tmp/control-ca.crt")
	t.Setenv("SANDBOX_RUNNER_CAPACITY_TOTAL", "2")
	t.Setenv("SANDBOX_RUNNER_FIRECRACKER_PROXY_PORT_START", "65535")

	if _, err := Load(); err == nil {
		t.Fatal("expected Load to reject overflowing Firecracker proxy port range")
	}
}

func TestLoadRejectsInvalidBackend(t *testing.T) {
	t.Setenv("SANDBOX_RUNNER_BACKEND", "unknown")
	t.Setenv("SANDBOX_RUNNER_API_KEYS", "test-key")
	t.Setenv("SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE", "test-image")

	if _, err := Load(); err == nil {
		t.Fatal("expected Load to reject invalid SANDBOX_RUNNER_BACKEND")
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

func TestLoadParsesLogLevel(t *testing.T) {
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
	t.Setenv("SANDBOX_RUNNER_LOG_LEVEL", "trace")

	if _, err := Load(); err == nil {
		t.Fatal("expected Load to reject invalid SANDBOX_RUNNER_LOG_LEVEL")
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
