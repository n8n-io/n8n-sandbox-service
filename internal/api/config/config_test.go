package config

import (
	"os"
	"testing"
)

func TestLoadAPIParsesDefaults(t *testing.T) {
	os.Setenv("SANDBOX_API_KEYS", "test-key")
	os.Setenv("SANDBOX_RUNNER_REGISTRATION_TOKEN", "reg-token")
	defer func() {
		os.Unsetenv("SANDBOX_API_KEYS")
		os.Unsetenv("SANDBOX_RUNNER_REGISTRATION_TOKEN")
	}()

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

	if cfg.DataDir != "/tmp/sandbox-api" {
		t.Errorf("expected DataDir /tmp/sandbox-api, got %s", cfg.DataDir)
	}

	if _, exists := cfg.APIKeys["test-key"]; !exists {
		t.Error("expected test-key in APIKeys")
	}
}

func TestLoadAPIRequiresAPIKeys(t *testing.T) {
	os.Unsetenv("SANDBOX_API_KEYS")
	os.Setenv("SANDBOX_RUNNER_REGISTRATION_TOKEN", "x")
	defer os.Unsetenv("SANDBOX_RUNNER_REGISTRATION_TOKEN")

	if _, err := LoadAPI(); err == nil {
		t.Error("expected LoadAPI() to fail without SANDBOX_API_KEYS")
	}
}

func TestLoadAPIRequiresRegistrationToken(t *testing.T) {
	os.Setenv("SANDBOX_API_KEYS", "test-key")
	os.Unsetenv("SANDBOX_RUNNER_REGISTRATION_TOKEN")
	defer os.Unsetenv("SANDBOX_API_KEYS")

	if _, err := LoadAPI(); err == nil {
		t.Error("expected LoadAPI() to fail without SANDBOX_RUNNER_REGISTRATION_TOKEN")
	}
}
