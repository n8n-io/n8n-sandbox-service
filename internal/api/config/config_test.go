package config

import (
	"os"
	"testing"
)

func TestLoadAPIParsesDefaults(t *testing.T) {
	os.Setenv("SANDBOX_API_KEYS", "test-key")
	defer os.Unsetenv("SANDBOX_API_KEYS")

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

	if cfg.RunnerURL != "http://localhost:8081" {
		t.Errorf("expected RunnerURL http://localhost:8081, got %s", cfg.RunnerURL)
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

	if _, err := LoadAPI(); err == nil {
		t.Error("expected LoadAPI() to fail without SANDBOX_API_KEYS")
	}
}
