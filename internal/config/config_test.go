package config

import "testing"

func TestLoadParsesDockerConfigDefaults(t *testing.T) {
	t.Setenv("SANDBOX_API_KEYS", "test")
	t.Setenv("SANDBOX_DOCKER_SANDBOX_IMAGE", "registry.example/sandbox:latest")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.DockerHost != defaultDockerHost {
		t.Fatalf("DockerHost = %q, want %q", cfg.DockerHost, defaultDockerHost)
	}
	if cfg.DockerSandboxImage != "registry.example/sandbox:latest" {
		t.Fatalf("DockerSandboxImage = %q", cfg.DockerSandboxImage)
	}
	if cfg.InterSandboxNetworkEnabled {
		t.Fatal("expected InterSandboxNetworkEnabled to default false")
	}
}

func TestLoadRequiresSandboxImage(t *testing.T) {
	t.Setenv("SANDBOX_API_KEYS", "test")
	t.Setenv("SANDBOX_DOCKER_SANDBOX_IMAGE", "")

	if _, err := Load(); err == nil {
		t.Fatal("expected SANDBOX_DOCKER_SANDBOX_IMAGE validation error")
	}
}
