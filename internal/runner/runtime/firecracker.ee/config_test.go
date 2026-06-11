package firecracker

import "testing"

func TestLoadConfigParsesDefaults(t *testing.T) {
	cfg, err := LoadConfig(1)
	if err != nil {
		t.Fatalf("LoadConfig() failed: %v", err)
	}

	if cfg.ProxyListenIP != "127.0.0.1" {
		t.Errorf("expected default proxy listen IP, got %s", cfg.ProxyListenIP)
	}
	if cfg.ProxyPortStart != 18081 {
		t.Errorf("expected default proxy port start 18081, got %d", cfg.ProxyPortStart)
	}
	if cfg.DaemonPort != 8081 {
		t.Errorf("expected default daemon port 8081, got %d", cfg.DaemonPort)
	}
}

func TestLoadConfigParsesOverrides(t *testing.T) {
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

	cfg, err := LoadConfig(4)
	if err != nil {
		t.Fatalf("LoadConfig() failed: %v", err)
	}
	if cfg.JailerBin != "/custom/jailer" {
		t.Errorf("expected custom jailer bin, got %s", cfg.JailerBin)
	}
	if cfg.GuestIP != "10.0.0.2" {
		t.Errorf("expected guest IP 10.0.0.2, got %s", cfg.GuestIP)
	}
	if cfg.ProxyPortStart != 20000 {
		t.Errorf("expected proxy port start 20000, got %d", cfg.ProxyPortStart)
	}
}

func TestLoadConfigRejectsProxyRangeOverflow(t *testing.T) {
	t.Setenv("SANDBOX_RUNNER_FIRECRACKER_PROXY_PORT_START", "65535")

	if _, err := LoadConfig(2); err == nil {
		t.Fatal("expected LoadConfig to reject overflowing proxy port range")
	}
}

func TestLoadConfigRejectsZeroCapacity(t *testing.T) {
	if _, err := LoadConfig(0); err == nil {
		t.Fatal("expected LoadConfig to reject zero capacity")
	}
}
