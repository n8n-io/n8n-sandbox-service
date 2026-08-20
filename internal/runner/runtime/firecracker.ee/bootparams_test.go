package firecracker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testBootParams mirrors what create-golden-snapshot.sh writes for a default
// build, agreeing with testConfig so admission passes unless a test says
// otherwise.
func testBootParams(cfg Config) bootParams {
	return bootParams{
		SchemaVersion:     bootParamsSchemaVersion,
		VCPUCount:         1,
		MemSizeMiB:        512,
		KernelImagePath:   "/vmlinux",
		BootArgs:          "console=ttyS0 reboot=k panic=1 pci=off ipv6.disable=1 init=/sandbox-daemon ip=172.16.0.10::172.16.0.1:255.255.255.0::eth0:off",
		RootfsDrivePath:   "/rootfs.ext4",
		GuestMAC:          "AA:FC:00:00:00:01",
		GuestIP:           cfg.GuestIP,
		HostTapDeviceName: cfg.HostTapDeviceName,
		DaemonPort:        cfg.DaemonPort,
	}
}

func writeBootParams(t *testing.T, path string, p bootParams) {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// useTestGoldenSnapshotDir repoints the runtime at a temp snapshot directory
// holding a valid boot.json, which admission requires.
func useTestGoldenSnapshotDir(t *testing.T, rt *Runtime) string {
	t.Helper()
	dir := t.TempDir()
	rt.config.SnapshotMemPath = filepath.Join(dir, "mem")
	rt.config.SnapshotStatePath = filepath.Join(dir, "state")
	writeBootParams(t, bootParamsPath(rt.config), testBootParams(rt.config))
	return dir
}

func TestBootParamsPathSitsBesideTheSnapshot(t *testing.T) {
	cfg := testConfig()
	want := "/srv/firecracker/snapshots/boot.json"
	if got := bootParamsPath(cfg); got != want {
		t.Fatalf("bootParamsPath() = %q, want %q", got, want)
	}
}

func TestLoadBootParamsAcceptsAValidSidecar(t *testing.T) {
	cfg := testConfig()
	path := filepath.Join(t.TempDir(), bootParamsFileName)
	writeBootParams(t, path, testBootParams(cfg))

	got, err := loadBootParams(path)
	if err != nil {
		t.Fatalf("loadBootParams() failed: %v", err)
	}
	if got.VCPUCount != 1 || got.MemSizeMiB != 512 {
		t.Fatalf("loadBootParams() = %+v, want vcpu 1 and 512 MiB", got)
	}
	if !strings.Contains(got.BootArgs, "init=/sandbox-daemon") {
		t.Fatalf("boot_args = %q, want the daemon init argument preserved verbatim", got.BootArgs)
	}
}

func TestLoadBootParamsRejectsUnusableSidecars(t *testing.T) {
	cfg := testConfig()

	tests := []struct {
		name    string
		mutate  func(*bootParams)
		wantErr string
	}{
		{"future schema", func(p *bootParams) { p.SchemaVersion = bootParamsSchemaVersion + 1 }, "schema_version"},
		{"no vcpus", func(p *bootParams) { p.VCPUCount = 0 }, "vcpu_count"},
		{"no memory", func(p *bootParams) { p.MemSizeMiB = 0 }, "mem_size_mib"},
		{"relative kernel", func(p *bootParams) { p.KernelImagePath = "vmlinux" }, "kernel_image_path"},
		{"relative rootfs", func(p *bootParams) { p.RootfsDrivePath = "rootfs.ext4" }, "rootfs_drive_path"},
		{"empty boot args", func(p *bootParams) { p.BootArgs = "  " }, "boot_args"},
		{"bad guest ip", func(p *bootParams) { p.GuestIP = "not-an-ip" }, "guest_ip"},
		{"bad guest mac", func(p *bootParams) { p.GuestMAC = "zz" }, "guest_mac"},
		{"no tap device", func(p *bootParams) { p.HostTapDeviceName = "" }, "host_tap_device_name"},
		{"bad daemon port", func(p *bootParams) { p.DaemonPort = 70000 }, "daemon_port"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := testBootParams(cfg)
			tt.mutate(&p)
			path := filepath.Join(t.TempDir(), bootParamsFileName)
			writeBootParams(t, path, p)

			_, err := loadBootParams(path)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("loadBootParams() error = %v, want one mentioning %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadBootParamsReportsMissingAndMalformedFiles(t *testing.T) {
	dir := t.TempDir()

	if _, err := loadBootParams(filepath.Join(dir, bootParamsFileName)); err == nil ||
		!strings.Contains(err.Error(), "read golden snapshot boot parameters") {
		t.Fatalf("loadBootParams() on missing file error = %v", err)
	}

	malformed := filepath.Join(dir, "malformed.json")
	if err := os.WriteFile(malformed, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadBootParams(malformed); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("loadBootParams() on malformed file error = %v", err)
	}
}

// Each of these is baked into the guest, so config that disagrees produces a
// sandbox that never answers rather than one that fails visibly.
func TestBootParamsMatchesConfigRejectsContradictions(t *testing.T) {
	cfg := testConfig()

	tests := []struct {
		name    string
		mutate  func(*bootParams)
		wantErr string
	}{
		{"guest ip", func(p *bootParams) { p.GuestIP = "10.0.0.5" }, "guest_ip"},
		{"tap device", func(p *bootParams) { p.HostTapDeviceName = "fc-tap-9" }, "host_tap_device_name"},
		{"daemon port", func(p *bootParams) { p.DaemonPort = 9999 }, "daemon_port"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := testBootParams(cfg)
			tt.mutate(&p)
			err := p.matchesConfig(cfg)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("matchesConfig() error = %v, want one mentioning %q", err, tt.wantErr)
			}
		})
	}

	agreeing := testBootParams(cfg)
	if err := agreeing.matchesConfig(cfg); err != nil {
		t.Fatalf("matchesConfig() on agreeing config: %v", err)
	}
}

func TestPinSnapshotAssetsRequiresBootParams(t *testing.T) {
	rt := testRuntime(1)
	rt.deps.pathExists = func(string) bool { return true }
	dir := useTestGoldenSnapshotDir(t, rt)

	if err := rt.pinSnapshotAssets(); err != nil {
		t.Fatalf("pinSnapshotAssets() with a valid sidecar: %v", err)
	}

	if err := os.Remove(filepath.Join(dir, bootParamsFileName)); err != nil {
		t.Fatal(err)
	}
	if err := rt.pinSnapshotAssets(); err == nil || !strings.Contains(err.Error(), "boot parameters") {
		t.Fatalf("pinSnapshotAssets() without a sidecar error = %v", err)
	}
}

func TestPinSnapshotAssetsRejectsSnapshotBuiltForAnotherGuestIP(t *testing.T) {
	rt := testRuntime(1)
	rt.deps.pathExists = func(string) bool { return true }
	useTestGoldenSnapshotDir(t, rt)

	p := testBootParams(rt.config)
	p.GuestIP = "10.9.9.9"
	writeBootParams(t, bootParamsPath(rt.config), p)

	err := rt.pinSnapshotAssets()
	if err == nil || !strings.Contains(err.Error(), "guest_ip") {
		t.Fatalf("pinSnapshotAssets() error = %v, want a guest_ip mismatch", err)
	}
}

// A runner upgraded onto a host whose snapshot predates the sidecar has mem and
// state but no boot.json. The three have to describe the same build, so the
// whole set is rebuilt rather than the sidecar being invented from config.
func TestEnsureGoldenSnapshotRebuildsWhenOnlyBootParamsMissing(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "create.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	daemon := filepath.Join(dir, "sandbox-daemon")
	if err := os.WriteFile(daemon, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "snapshots")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}

	rt := testRuntime(1)
	rt.config.CreateSnapshotScript = script
	rt.config.DaemonBin = daemon
	rt.config.SnapshotMemPath = filepath.Join(outDir, "snapshot_mem")
	rt.config.SnapshotStatePath = filepath.Join(outDir, "snapshot_state")
	bootPath := bootParamsPath(rt.config)

	for _, path := range []string{rt.config.SnapshotMemPath, rt.config.SnapshotStatePath} {
		if err := os.WriteFile(path, []byte{1}, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	rt.deps.pathExists = func(path string) bool {
		if path == script || path == daemon {
			return true
		}
		_, err := os.Stat(path)
		return err == nil
	}
	var ran bool
	rt.deps.run = func(context.Context, string, ...string) error {
		ran = true
		writeBootParams(t, bootPath, testBootParams(rt.config))
		return nil
	}

	if err := rt.ensureGoldenSnapshot(context.Background()); err != nil {
		t.Fatalf("ensureGoldenSnapshot() failed: %v", err)
	}
	if !ran {
		t.Fatal("expected the create script to run when only boot.json is missing")
	}
}

func TestEnsureGoldenSnapshotWithoutScriptNamesTheSidecar(t *testing.T) {
	rt := testRuntime(1)
	rt.config.CreateSnapshotScript = ""
	rt.deps.pathExists = func(path string) bool {
		return path != bootParamsPath(rt.config)
	}

	err := rt.ensureGoldenSnapshot(context.Background())
	if err == nil || !strings.Contains(err.Error(), bootParamsFileName) {
		t.Fatalf("ensureGoldenSnapshot() error = %v, want it to name %s", err, bootParamsFileName)
	}
	if !strings.Contains(err.Error(), "create-golden-snapshot.sh") {
		t.Fatalf("ensureGoldenSnapshot() error = %v, want it to name the script to re-run", err)
	}
}
