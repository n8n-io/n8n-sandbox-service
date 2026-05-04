package config

import (
	"os"
	"testing"
)

func TestLoadParsesDefaults(t *testing.T) {
	os.Setenv("SANDBOX_RUNNER_API_KEYS", "test-key")
	os.Setenv("SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE", "test-image")
	defer func() {
		os.Unsetenv("SANDBOX_RUNNER_API_KEYS")
		os.Unsetenv("SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE")
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

	if !cfg.EnableCgroups {
		t.Error("expected EnableCgroups true")
	}

	if cfg.InterSandboxNetworkEnabled {
		t.Error("expected InterSandboxNetworkEnabled false")
	}

	if cfg.CapacityTotal != defaultRunnerCapacityTotal {
		t.Errorf("expected CapacityTotal %d, got %d", defaultRunnerCapacityTotal, cfg.CapacityTotal)
	}

	if _, exists := cfg.APIKeys["test-key"]; !exists {
		t.Error("expected test-key in APIKeys")
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
	os.Setenv("SANDBOX_RUNNER_API_KEYS", "test-key")
	os.Setenv("SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE", "img")
	os.Setenv("SANDBOX_RUNNER_GRPC_TLS_CA_FILE", "/tmp/ca.crt")
	defer func() {
		os.Unsetenv("SANDBOX_RUNNER_API_KEYS")
		os.Unsetenv("SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE")
		os.Unsetenv("SANDBOX_RUNNER_GRPC_TLS_CA_FILE")
	}()

	if _, err := Load(); err == nil {
		t.Fatal("expected Load to reject partial SANDBOX_RUNNER_GRPC_TLS_*")
	}
}
