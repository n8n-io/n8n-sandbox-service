package firecracker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/runner/config"
	fcnetwork "github.com/n8n-io/sandbox-service/internal/runner/runtime/firecracker.ee/network"
)

type capacityHost = hostPaths

// setupCapacityHost points the runner at a temp snapshot dir holding a
// boot.json with memMiB (0 writes none) and fake /proc/meminfo and cgroup files.
func setupCapacityHost(t *testing.T, memTotalBytes int64, cgroupLimit string, memMiB int) capacityHost {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SANDBOX_RUNNER_FIRECRACKER_SNAPSHOT_MEM_PATH", filepath.Join(dir, "mem"))
	t.Setenv("SANDBOX_RUNNER_FIRECRACKER_SNAPSHOT_STATE_PATH", filepath.Join(dir, "state"))
	t.Setenv(memoryRatioEnv, "")
	if memMiB > 0 {
		p := testBootParams(defaultConfig())
		p.MemSizeMiB = memMiB
		writeBootParams(t, filepath.Join(dir, bootParamsFileName), p)
	}
	return fakeHostMemory(t, memTotalBytes, cgroupLimit)
}

// An empty cgroupLimit leaves the cgroup unlimited, so the budget is MemTotal.
func fakeHostMemory(t *testing.T, memTotalBytes int64, cgroupLimit string) hostPaths {
	t.Helper()
	h, err := writeHostMemory(t.TempDir(), memTotalBytes, cgroupLimit)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// Shared fake host so tests do not read /proc/meminfo.
func testHostPaths() hostPaths {
	testHostOnce.Do(func() {
		dir, err := os.MkdirTemp("", "fc-hostmem-")
		if err != nil {
			testHostErr = err
			return
		}
		testHost, testHostErr = writeHostMemory(dir, 64*gib, "")
	})
	if testHostErr != nil {
		panic(testHostErr)
	}
	return testHost
}

var (
	testHostOnce sync.Once
	testHost     hostPaths
	testHostErr  error
)

func writeHostMemory(dir string, memTotalBytes int64, cgroupLimit string) (hostPaths, error) {
	h := hostPaths{meminfo: filepath.Join(dir, "meminfo"), selfCgroup: filepath.Join(dir, "cgroup"), cgroupRoot: filepath.Join(dir, "cg")}
	if err := os.WriteFile(h.meminfo, []byte("MemTotal: "+strconv.FormatInt(memTotalBytes/1024, 10)+" kB\n"), 0o600); err != nil {
		return hostPaths{}, err
	}
	if err := os.MkdirAll(filepath.Join(h.cgroupRoot, "runner.slice"), 0o755); err != nil {
		return hostPaths{}, err
	}
	// The root file says max; the process's own cgroup carries the limit.
	for rel, v := range map[string]string{"": "max", "runner.slice": cgroupLimit} {
		if v == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(h.cgroupRoot, rel, "memory.max"), []byte(v+"\n"), 0o600); err != nil {
			return hostPaths{}, err
		}
	}
	if err := os.WriteFile(h.selfCgroup, []byte("0::/runner.slice\n"), 0o600); err != nil {
		return hostPaths{}, err
	}
	return h, nil
}

// resolve runs the startup checks, then works out the slot count the way
// admission does from the boot.json that exists.
func resolve(h capacityHost, set bool, total int32) (int32, error) {
	rc := &config.Config{CapacityTotal: total, CapacityTotalSet: set}
	if !set {
		rc.CapacityTotal = 1000
	}
	cfg, err := resolveConfig(rc, h)
	if err != nil {
		return 0, err
	}
	boot, err := loadBootParams(bootParamsPath(cfg))
	if err != nil {
		return 0, err
	}
	slots, _, err := slotCount(cfg, rc, boot, h)
	return int32(slots), err
}

const gib = int64(1) << 30

func TestResolveCapacityDerivesFromHostMemory(t *testing.T) {
	h := setupCapacityHost(t, 64*gib, "max", 512)
	if got, err := resolve(h, false, 0); err != nil || got != 106 {
		t.Fatalf("got %d, %v; want 106", got, err)
	}
}

func TestResolveCapacityExplicitMayOnlyLower(t *testing.T) {
	h := setupCapacityHost(t, 64*gib, "", 512)
	if got, err := resolve(h, true, 100); err != nil || got != 100 {
		t.Fatalf("explicit 100: got %d, %v", got, err)
	}
	_, err := resolve(h, true, 200)
	if err == nil || !strings.Contains(err.Error(), "200") || !strings.Contains(err.Error(), "106") {
		t.Fatalf("explicit 200: want error naming 200 and 106, got %v", err)
	}
}

func TestResolveCapacityUsesFiniteCgroupLimit(t *testing.T) {
	h := setupCapacityHost(t, 64*gib, strconv.FormatInt(8*gib, 10), 512)
	if got, err := resolve(h, false, 0); err != nil || got != 7 {
		t.Fatalf("got %d, %v; want 7", got, err)
	}
	// A limit above MemTotal does not raise the budget.
	h = setupCapacityHost(t, 64*gib, strconv.FormatInt(128*gib, 10), 512)
	if got, err := resolve(h, false, 0); err != nil || got != 106 {
		t.Fatalf("got %d, %v; want 106", got, err)
	}
}

func TestResolveCapacityNamesMissingInputs(t *testing.T) {
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
		{"budget at the reserve", 4 * gib, 512, "", "reserved"},
		{"budget below one guest", 4*gib + 100<<20, 512, "", "no room"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			memMiB := tt.memMiB
			if memMiB < 0 {
				memMiB = 512
			}
			h := setupCapacityHost(t, tt.memory, "", memMiB)
			if tt.memMiB < 0 {
				p := testBootParams(defaultConfig())
				p.MemSizeMiB = 0
				writeBootParams(t, filepath.Join(filepath.Dir(os.Getenv("SANDBOX_RUNNER_FIRECRACKER_SNAPSHOT_MEM_PATH")), bootParamsFileName), p)
			}
			if tt.ratio != "" {
				t.Setenv(memoryRatioEnv, tt.ratio)
			}
			if _, err := resolve(h, false, 0); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestResolveCapacityLimitedByStructuralCeilings(t *testing.T) {
	// Large enough that the port range and MaxSlots bind before memory.
	perSlot := int64(512<<20 + 64<<20)
	h := setupCapacityHost(t, (int64(fcnetwork.MaxSlots)+1)*perSlot+4*gib, "", 512)
	t.Setenv("SANDBOX_RUNNER_FIRECRACKER_PROXY_PORT_START", "60000")
	if got, err := resolve(h, false, 0); err != nil || got != 65535-60000+1 {
		t.Fatalf("port range: got %d, %v", got, err)
	}
	t.Setenv("SANDBOX_RUNNER_FIRECRACKER_PROXY_PORT_START", "1024")
	if got, err := resolve(h, false, 0); err != nil || int64(got) != fcnetwork.MaxSlots {
		t.Fatalf("MaxSlots: got %d, %v", got, err)
	}
}

func TestResolveCapacityRatioScalesMemorySlots(t *testing.T) {
	h := setupCapacityHost(t, 64*gib, "", 512)
	t.Setenv(memoryRatioEnv, "1.5")
	// floor(60 GiB * 1.5 / 576 MiB): scaled before the floor, not after.
	if got, err := resolve(h, false, 0); err != nil || got != 160 {
		t.Fatalf("got %d, %v; want 160", got, err)
	}
}

// A new VM has no boot.json until Prepare builds the snapshot, so startup must
// not need it, whether or not a count is set.
func TestResolveConfigWithoutBootParams(t *testing.T) {
	h := setupCapacityHost(t, 64*gib, "", 0)
	for _, set := range []bool{false, true} {
		rc := &config.Config{CapacityTotal: 500, CapacityTotalSet: set}
		if _, err := resolveConfig(rc, h); err != nil {
			t.Fatalf("set=%v: startup without boot.json failed: %v", set, err)
		}
	}
	// A bad ratio still fails at startup.
	t.Setenv(memoryRatioEnv, "0")
	if _, err := resolveConfig(&config.Config{}, h); err == nil || !strings.Contains(err.Error(), memoryRatioEnv) {
		t.Fatalf("want ratio error, got %v", err)
	}
}

func TestSizeSlotsAtAdmission(t *testing.T) {
	h := fakeHostMemory(t, 64*gib, "")
	boot := testBootParams(defaultConfig())
	boot.MemSizeMiB = 512
	for _, tt := range []struct {
		name      string
		set       bool
		total     int32
		allocated int32
		want      int
		wantErr   string
	}{
		{"derived on a new runtime", false, 1000, 0, 106, ""},
		{"explicit within memory", true, 100, 0, 100, ""},
		{"explicit above memory", true, 200, 0, 0, "200"},
		{"already sized and still fits", false, 1000, 50, 50, ""},
		{"already sized for a larger snapshot's guests", false, 1000, 200, 200, "200 slots"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rt := newTestRuntime(tt.allocated)
			rt.runnerConfig = &config.Config{CapacityTotal: tt.total, CapacityTotalSet: tt.set}
			rt.hostPaths = h
			// A cancelled context makes the wirer sizeSlots starts return at once.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			err := rt.sizeSlots(ctx, &boot)
			if got := len(rt.slotList()); got != tt.want {
				t.Fatalf("slots = %d, want %d", got, tt.want)
			}
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			var sizing sizingError
			if !errors.As(err, &sizing) || !strings.Contains(err.Error(), tt.wantErr) || !strings.Contains(err.Error(), "106") {
				t.Fatalf("want a sizingError naming %s and 106, got %v", tt.wantErr, err)
			}
		})
	}
}

// A slot count admission cannot fix stops the process, so a restart sizes the
// slots from the current snapshot instead of the runner staying unhealthy.
func TestPrepareExitsWhenSlotsCannotBeSized(t *testing.T) {
	// 200 slots from an earlier, smaller guest; the snapshot now fits 106.
	rt := testRuntimeT(t, 200)
	stubCreateDeps(rt)
	useTestGoldenSnapshotDir(t, rt)
	exitCode := -1
	rt.deps.exit = func(code int) { exitCode = code }

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rt.Prepare(ctx)
	if ctx.Err() != nil {
		t.Fatal("Prepare kept retrying instead of exiting")
	}
	if exitCode != 1 {
		t.Fatalf("exit code = %d, want 1", exitCode)
	}
	if rt.admissionOK.Load() {
		t.Fatal("admission passed with slots the snapshot cannot back")
	}
}
