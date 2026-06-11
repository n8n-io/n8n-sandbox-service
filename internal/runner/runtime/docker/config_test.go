package docker

import "testing"

func TestLoadConfigParsesDefaults(t *testing.T) {
	t.Setenv("SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE", "test-image")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() failed: %v", err)
	}

	if cfg.Host != "unix:///var/run/docker.sock" {
		t.Errorf("expected Host unix:///var/run/docker.sock, got %s", cfg.Host)
	}
	if cfg.SandboxImage != "test-image" {
		t.Errorf("expected SandboxImage test-image, got %s", cfg.SandboxImage)
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
	if cfg.DefaultDiskQuotaMB != 0 {
		t.Errorf("expected DefaultDiskQuotaMB 0, got %d", cfg.DefaultDiskQuotaMB)
	}
	if cfg.DiskQuotaActive {
		t.Error("expected DiskQuotaActive false by default")
	}
	if !cfg.EnableCgroups {
		t.Error("expected EnableCgroups true")
	}
}

func TestLoadConfigParsesDiskQuotaEnv(t *testing.T) {
	t.Setenv("SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE", "test-image")
	t.Setenv("SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB", "2048")
	t.Setenv("SANDBOX_RUNNER_DISK_QUOTA_ACTIVE", "true")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() failed: %v", err)
	}

	if cfg.DefaultDiskQuotaMB != 2048 {
		t.Errorf("expected DefaultDiskQuotaMB 2048, got %d", cfg.DefaultDiskQuotaMB)
	}
	if !cfg.DiskQuotaActive {
		t.Error("expected DiskQuotaActive true")
	}
}

func TestLoadConfigAcceptsZeroDefaultDiskQuotaMB(t *testing.T) {
	t.Setenv("SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE", "test-image")
	t.Setenv("SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB", "0")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() rejected explicit 0: %v", err)
	}
	if cfg.DefaultDiskQuotaMB != 0 {
		t.Errorf("expected DefaultDiskQuotaMB 0, got %d", cfg.DefaultDiskQuotaMB)
	}
}

func TestLoadConfigRequiresSandboxImage(t *testing.T) {
	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected LoadConfig to require SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE")
	}
}

func TestLoadConfigRejectsNegativeDefaultDiskQuotaMB(t *testing.T) {
	t.Setenv("SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE", "test-image")
	t.Setenv("SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB", "-1")

	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected LoadConfig to reject negative SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB")
	}
}

func TestLoadConfigRejectsInvalidDefaultDiskQuotaMB(t *testing.T) {
	t.Setenv("SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE", "test-image")
	t.Setenv("SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB", "not-a-number")

	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected LoadConfig to reject non-numeric SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB")
	}
}
