package config

import "testing"

func TestLoadAPIParsesDefaults(t *testing.T) {
	t.Setenv("SANDBOX_API_KEYS", "test")

	cfg, err := LoadAPI()
	if err != nil {
		t.Fatalf("load api config: %v", err)
	}

	if cfg.ListenAddr != defaultListenAddr {
		t.Fatalf("ListenAddr = %q, want %q", cfg.ListenAddr, defaultListenAddr)
	}
	if cfg.MaxFileBytes != defaultMaxFileBytes {
		t.Fatalf("MaxFileBytes = %d, want %d", cfg.MaxFileBytes, defaultMaxFileBytes)
	}
	if cfg.RunnerURL != defaultRunnerURL {
		t.Fatalf("RunnerURL = %q, want %q", cfg.RunnerURL, defaultRunnerURL)
	}
}

func TestLoadAPIRequiresAPIKeys(t *testing.T) {
	t.Setenv("SANDBOX_API_KEYS", "")

	if _, err := LoadAPI(); err == nil {
		t.Fatal("expected SANDBOX_API_KEYS validation error")
	}
}
