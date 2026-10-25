package docker

import (
	"reflect"
	"testing"
)

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
	if cfg.DNSServers != nil {
		t.Errorf("expected no DNSServers by default, got %v", cfg.DNSServers)
	}
}

func TestLoadConfigParsesDNSServers(t *testing.T) {
	t.Setenv("SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE", "test-image")
	t.Setenv("SANDBOX_RUNNER_DOCKER_DNS_SERVERS", "8.8.8.8, ::ffff:1.1.1.1")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() failed: %v", err)
	}
	if want := []string{"8.8.8.8", "1.1.1.1"}; !reflect.DeepEqual(cfg.DNSServers, want) {
		t.Errorf("expected DNSServers %v, got %v", want, cfg.DNSServers)
	}
}

// Queries to a configured server leave from the sandbox, so one in a blocked
// range would fail for every sandbox; refusing it at startup says why.
func TestLoadConfigRejectsUnusableDNSServers(t *testing.T) {
	for _, v := range []string{"10.0.0.10", "8.8.8.8,", "dns.example.com", "2001:4860:4860::8888", "168.63.129.16", "0.0.0.0"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE", "test-image")
			t.Setenv("SANDBOX_RUNNER_DOCKER_DNS_SERVERS", v)

			if _, err := LoadConfig(); err == nil {
				t.Fatalf("expected LoadConfig to reject SANDBOX_RUNNER_DOCKER_DNS_SERVERS=%q", v)
			}
		})
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

func TestLoadConfigParsesTemplates(t *testing.T) {
	t.Setenv("SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE", "img/default")
	t.Setenv("SANDBOX_RUNNER_DOCKER_TEMPLATES", `{"python":{"image":"img/python","memory_mb":2048}}`)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Templates["python"].Image != "img/python" || cfg.Templates["python"].MemoryMB != 2048 {
		t.Fatalf("templates = %+v", cfg.Templates)
	}
	for _, bad := range []string{
		`{"default":{"image":"x"}}`,
		`{"Python":{"image":"x"}}`,
		`{"python":{}}`,
		`{"python":{"image":"x","memory":1}}`,
		`{"python":{"image":"x","memory_mb":-1}}`,
		`[]`,
		`{"python":{"image":"x"}} {}`,
	} {
		t.Setenv("SANDBOX_RUNNER_DOCKER_TEMPLATES", bad)
		if _, err := LoadConfig(); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestLoadConfigRefusesUnenforceableDiskQuota(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr bool
	}{
		{name: "default quota, inactive", env: map[string]string{"SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB": "2048"}, wantErr: true},
		{name: "template quota, inactive", env: map[string]string{"SANDBOX_RUNNER_DOCKER_TEMPLATES": `{"big":{"image":"img","disk_quota_mb":4096}}`}, wantErr: true},
		{name: "default quota, active", env: map[string]string{"SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB": "2048", "SANDBOX_RUNNER_DISK_QUOTA_ACTIVE": "true"}},
		{name: "default quota, opted out", env: map[string]string{"SANDBOX_RUNNER_DEFAULT_DISK_QUOTA_MB": "2048", "SANDBOX_RUNNER_ALLOW_UNENFORCED_DISK_QUOTA": "true"}},
		{name: "no quota", env: map[string]string{"SANDBOX_RUNNER_DOCKER_TEMPLATES": `{"big":{"image":"img"}}`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SANDBOX_RUNNER_DOCKER_SANDBOX_IMAGE", "test-image")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if _, err := LoadConfig(); (err != nil) != tc.wantErr {
				t.Fatalf("LoadConfig() err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
