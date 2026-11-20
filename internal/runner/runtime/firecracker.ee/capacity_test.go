package firecracker

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/runner/config"
	fcnetwork "github.com/n8n-io/sandbox-service/internal/runner/runtime/firecracker.ee/network"
)

const gib = int64(1) << 30

// setupCapacityHost points the runner at a temp snapshot dir and returns a fake
// /proc/meminfo reporting memTotalBytes. With memMiB above 0 the dir holds a
// complete golden snapshot whose boot.json records memMiB; with 0 it is empty.
func setupCapacityHost(t *testing.T, memTotalBytes int64, memMiB int) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SANDBOX_RUNNER_FIRECRACKER_SNAPSHOT_MEM_PATH", filepath.Join(dir, "mem"))
	t.Setenv("SANDBOX_RUNNER_FIRECRACKER_SNAPSHOT_STATE_PATH", filepath.Join(dir, "state"))
	t.Setenv("SANDBOX_RUNNER_FIRECRACKER_CREATE_SNAPSHOT_SCRIPT", "")
	t.Setenv(memoryRatioEnv, "")
	if memMiB > 0 {
		writeTestSnapshot(t, dir, memMiB)
	}
	meminfo := filepath.Join(t.TempDir(), "meminfo")
	if err := os.WriteFile(meminfo, []byte("MemTotal: "+strconv.FormatInt(memTotalBytes/1024, 10)+" kB\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return meminfo
}

// writeTestSnapshot writes the three files of a golden snapshot into dir.
func writeTestSnapshot(t *testing.T, dir string, memMiB int) {
	t.Helper()
	for _, name := range []string{"mem", "state"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p := testBootParams(defaultConfig())
	p.MemSizeMiB = memMiB
	writeBootParams(t, filepath.Join(dir, bootParamsFileName), p)
}

// withCreateScript turns on snapshot builds. The tests stub the build, so the
// script does not need to exist.
func withCreateScript(t *testing.T) {
	t.Helper()
	t.Setenv("SANDBOX_RUNNER_FIRECRACKER_CREATE_SNAPSHOT_SCRIPT", "/opt/create-golden-snapshot.sh")
}

func checkPasses(Config) error { return nil }

// building returns a snapshotBuild whose check passes and whose build is build.
func building(build func(context.Context, Config) error) snapshotBuild {
	return snapshotBuild{check: checkPasses, build: build}
}

// noBuild fails the test path that expects no build to run.
var noBuild = building(func(context.Context, Config) error {
	return errors.New("unexpected snapshot build")
})

// resolve runs resolveConfig with a complete snapshot already in place and
// returns the slot count it set.
func resolve(meminfo string, set bool, total int32) (int32, error) {
	rc := &config.Config{CapacityTotal: total, CapacityTotalSet: set}
	if !set {
		rc.CapacityTotal = 1000
	}
	_, err := resolveConfig(context.Background(), rc, meminfo, noBuild)
	return rc.CapacityTotal, err
}

func TestResolveCapacityDerivesFromHostMemory(t *testing.T) {
	meminfo := setupCapacityHost(t, 64*gib, 512)
	if got, err := resolve(meminfo, false, 0); err != nil || got != 106 {
		t.Fatalf("got %d, %v; want 106", got, err)
	}
}

func TestResolveCapacityExplicitMayOnlyLower(t *testing.T) {
	meminfo := setupCapacityHost(t, 64*gib, 512)
	if got, err := resolve(meminfo, true, 100); err != nil || got != 100 {
		t.Fatalf("explicit 100: got %d, %v", got, err)
	}
	_, err := resolve(meminfo, true, 200)
	if err == nil || !strings.Contains(err.Error(), "200") || !strings.Contains(err.Error(), "106") {
		t.Fatalf("explicit 200: want error naming 200 and 106, got %v", err)
	}
}

func TestResolveCapacityNamesBadInputs(t *testing.T) {
	tests := []struct {
		name    string
		memory  int64
		memMiB  int
		ratio   string
		wantErr string
	}{
		{"zero mem_size_mib", 64 * gib, -1, "", "mem_size_mib"},
		{"zero ratio", 64 * gib, 512, "0", memoryRatioEnv},
		{"negative ratio", 64 * gib, 512, "-1", memoryRatioEnv},
		{"non-numeric ratio", 64 * gib, 512, "lots", memoryRatioEnv},
		{"NaN ratio", 64 * gib, 512, "NaN", memoryRatioEnv},
		{"MemTotal at the reserve", 4 * gib, 512, "", "reserved"},
		{"MemTotal below one guest", 4*gib + 100<<20, 512, "", "no room"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			memMiB := tt.memMiB
			if memMiB < 0 {
				memMiB = 512
			}
			meminfo := setupCapacityHost(t, tt.memory, memMiB)
			if tt.memMiB < 0 {
				p := testBootParams(defaultConfig())
				p.MemSizeMiB = 0
				writeBootParams(t, filepath.Join(filepath.Dir(os.Getenv("SANDBOX_RUNNER_FIRECRACKER_SNAPSHOT_MEM_PATH")), bootParamsFileName), p)
			}
			if tt.ratio != "" {
				t.Setenv(memoryRatioEnv, tt.ratio)
			}
			if _, err := resolve(meminfo, false, 0); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
	t.Run("unreadable meminfo", func(t *testing.T) {
		setupCapacityHost(t, 64*gib, 512)
		if _, err := resolve(filepath.Join(t.TempDir(), "missing"), false, 0); err == nil || !strings.Contains(err.Error(), "host memory") {
			t.Fatalf("want a host memory error, got %v", err)
		}
	})
}

func TestResolveCapacityLimitedByStructuralCeilings(t *testing.T) {
	// Large enough that the port range and MaxSlots bind before memory.
	perSlot := int64(512<<20 + 64<<20)
	meminfo := setupCapacityHost(t, (int64(fcnetwork.MaxSlots)+1)*perSlot+4*gib, 512)
	t.Setenv("SANDBOX_RUNNER_FIRECRACKER_PROXY_PORT_START", "60000")
	if got, err := resolve(meminfo, false, 0); err != nil || got != 65535-60000+1 {
		t.Fatalf("port range: got %d, %v", got, err)
	}
	t.Setenv("SANDBOX_RUNNER_FIRECRACKER_PROXY_PORT_START", "1024")
	if got, err := resolve(meminfo, false, 0); err != nil || int64(got) != fcnetwork.MaxSlots {
		t.Fatalf("MaxSlots: got %d, %v", got, err)
	}
}

func TestResolveCapacityRatioScalesMemorySlots(t *testing.T) {
	meminfo := setupCapacityHost(t, 64*gib, 512)
	t.Setenv(memoryRatioEnv, "1.5")
	// floor(60 GiB * 1.5 / 576 MiB): scaled before the floor, not after.
	if got, err := resolve(meminfo, false, 0); err != nil || got != 160 {
		t.Fatalf("got %d, %v; want 160", got, err)
	}
}

// A new VM has no snapshot yet: startup builds it, then sizes from its boot.json.
func TestResolveConfigBuildsMissingSnapshot(t *testing.T) {
	meminfo := setupCapacityHost(t, 64*gib, 0)
	withCreateScript(t)
	builds := 0
	build := func(_ context.Context, cfg Config) error {
		builds++
		writeTestSnapshot(t, filepath.Dir(cfg.SnapshotMemPath), 512)
		return nil
	}
	rc := &config.Config{CapacityTotal: 1000}
	if _, err := resolveConfig(context.Background(), rc, meminfo, building(build)); err != nil {
		t.Fatal(err)
	}
	if builds != 1 || rc.CapacityTotal != 106 {
		t.Fatalf("builds = %d, slots = %d; want 1 build and 106 slots", builds, rc.CapacityTotal)
	}
}

// A failed build is retried in-process, so systemd does not restart the runner
// into another build every few seconds.
func TestResolveConfigRetriesFailedBuild(t *testing.T) {
	meminfo := setupCapacityHost(t, 64*gib, 0)
	withCreateScript(t)
	builds := 0
	build := func(_ context.Context, cfg Config) error {
		builds++
		if builds == 1 {
			return errors.New("guest daemon did not answer")
		}
		writeTestSnapshot(t, filepath.Dir(cfg.SnapshotMemPath), 512)
		return nil
	}
	rc := &config.Config{CapacityTotal: 1000}
	if _, err := resolveConfig(context.Background(), rc, meminfo, building(build)); err != nil {
		t.Fatal(err)
	}
	if builds != 2 || rc.CapacityTotal != 106 {
		t.Fatalf("builds = %d, slots = %d; want 2 builds and 106 slots", builds, rc.CapacityTotal)
	}

	// A cancelled context ends the retries with the build error.
	setupCapacityHost(t, 64*gib, 0)
	withCreateScript(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	failing := func(context.Context, Config) error { return errors.New("guest daemon did not answer") }
	if _, err := resolveConfig(ctx, &config.Config{}, meminfo, building(failing)); err == nil || !strings.Contains(err.Error(), "guest daemon") {
		t.Fatalf("want the build error once the context ends, got %v", err)
	}
}

func TestResolveConfigNeedsCreateScriptForMissingSnapshot(t *testing.T) {
	meminfo := setupCapacityHost(t, 64*gib, 0)
	_, err := resolveConfig(context.Background(), &config.Config{}, meminfo, noBuild)
	if err == nil || !strings.Contains(err.Error(), bootParamsFileName) || !strings.Contains(err.Error(), "SANDBOX_RUNNER_FIRECRACKER_CREATE_SNAPSHOT_SCRIPT") {
		t.Fatalf("want an error naming boot.json and the create script, got %v", err)
	}
}

// A failed check (a missing file, a kernel anyone can write) fails every try,
// so it stops startup at once, before any build, instead of being retried out
// of sight.
func TestResolveConfigStopsWhenBuildCheckFails(t *testing.T) {
	meminfo := setupCapacityHost(t, 64*gib, 0)
	withCreateScript(t)
	builds := 0
	snapshot := snapshotBuild{
		check: func(Config) error { return errors.New("template kernel is writable by group") },
		build: func(context.Context, Config) error { builds++; return nil },
	}
	_, err := resolveConfig(context.Background(), &config.Config{}, meminfo, snapshot)
	if err == nil || !strings.Contains(err.Error(), "writable by group") || builds != 0 {
		t.Fatalf("want the check error and no build, got %v after %d builds", err, builds)
	}
}

// A build killed before New leaves its microVM and mounts. The next start has
// to clear them before it builds again, including a start that will itself be
// stopped before New, and a reap failure must not go on to build on top of them.
func TestResolveConfigReapsInterruptedSnapshotBeforeBuild(t *testing.T) {
	meminfo := setupCapacityHost(t, 64*gib, 0)
	withCreateScript(t)
	var order []string
	snapshot := building(func(_ context.Context, cfg Config) error {
		order = append(order, "build")
		writeTestSnapshot(t, filepath.Dir(cfg.SnapshotMemPath), 512)
		return nil
	})
	snapshot.reap = func(context.Context, Config) error {
		order = append(order, "reap")
		return nil
	}
	if _, err := resolveConfig(context.Background(), &config.Config{}, meminfo, snapshot); err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "reap,build" {
		t.Fatalf("order = %v, want reap then build", order)
	}

	meminfo = setupCapacityHost(t, 64*gib, 0)
	withCreateScript(t)
	builds := 0
	snapshot = building(func(context.Context, Config) error {
		builds++
		return nil
	})
	snapshot.reap = func(context.Context, Config) error { return errors.New("stale snapshot vm") }
	if _, err := resolveConfig(context.Background(), &config.Config{}, meminfo, snapshot); err == nil || !strings.Contains(err.Error(), "stale snapshot vm") || builds != 0 {
		t.Fatalf("want the reap error and no build, got %v after %d builds", err, builds)
	}

	meminfo = setupCapacityHost(t, 64*gib, 512)
	reaped := false
	snapshot = noBuild
	snapshot.reap = func(context.Context, Config) error {
		reaped = true
		return nil
	}
	if _, err := resolveConfig(context.Background(), &config.Config{}, meminfo, snapshot); err != nil {
		t.Fatal(err)
	}
	if !reaped {
		t.Fatal("reap did not run when the snapshot was already complete")
	}
}

// A guest larger than int64 can hold in bytes must not wrap the per-slot divisor.
// The wrapped size is small, so the count would be the proxy or uplink ceiling
// and the admission canary would then ask Firecracker for the real guest.
func TestResolveCapacityRejectsGuestRAMThatOverflows(t *testing.T) {
	meminfo := setupCapacityHost(t, 64*gib, 512)
	dir := filepath.Dir(os.Getenv("SANDBOX_RUNNER_FIRECRACKER_SNAPSHOT_MEM_PATH"))
	p := testBootParams(defaultConfig())
	p.MemSizeMiB = int((math.MaxInt64-perVMOverhead)>>20) + 1
	writeBootParams(t, filepath.Join(dir, bootParamsFileName), p)
	if _, err := resolve(meminfo, false, 0); err == nil || !strings.Contains(err.Error(), "mem_size_mib") {
		t.Fatalf("want an error naming mem_size_mib, got %v", err)
	}
}

// A snapshot built for other runner settings is rejected by admission and then
// rebuilt, so startup does not size the slots from it.
func TestResolveConfigRejectsSnapshotThatDoesNotMatch(t *testing.T) {
	meminfo := setupCapacityHost(t, 64*gib, 512)
	t.Setenv("SANDBOX_RUNNER_FIRECRACKER_GUEST_IP", "172.16.0.99")
	if _, err := resolve(meminfo, false, 0); err == nil || !strings.Contains(err.Error(), "guest_ip") {
		t.Fatalf("want a guest_ip mismatch, got %v", err)
	}
}
