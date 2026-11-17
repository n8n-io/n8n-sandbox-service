package firecracker

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

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

func resolve(h capacityHost, set bool, total int32) (int32, error) {
	cfg := &config.Config{CapacityTotal: total, CapacityTotalSet: set}
	if !set {
		cfg.CapacityTotal = 1000
	}
	err := resolveCapacity(cfg, h)
	return cfg.CapacityTotal, err
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
		{"no boot.json", 64 * gib, 0, "", "boot parameters"},
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

func TestResolveCapacityWithoutBootParams(t *testing.T) {
	h := setupCapacityHost(t, 64*gib, "", 0)
	// Unset: there is no count to allocate, so startup fails naming boot.json.
	if _, err := resolve(h, false, 0); err == nil || !strings.Contains(err.Error(), bootParamsFileName) {
		t.Fatalf("unset: want error naming %s, got %v", bootParamsFileName, err)
	}
	// Set: starts at the explicit count, even one the host could not hold.
	if got, err := resolve(h, true, 500); err != nil || got != 500 {
		t.Fatalf("set: got %d, %v; want 500", got, err)
	}
}

func TestAdmissionChecksCapacityAgainstPinnedSnapshot(t *testing.T) {
	h := setupCapacityHost(t, 64*gib, "", 512)
	boot := testBootParams(defaultConfig())
	boot.MemSizeMiB = 512
	for _, tt := range []struct {
		name    string
		set     bool
		total   int32
		wantErr string
	}{
		{"explicit within memory", true, 100, ""},
		{"explicit above memory", true, 200, "200"},
		{"derived count still fits", false, 106, ""},
		{"derived count from an older smaller guest", false, 1000, "1000"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rt := &Runtime{runnerConfig: &config.Config{CapacityTotal: tt.total, CapacityTotalSet: tt.set}, hostPaths: h}
			err := rt.checkCapacityAtAdmission(&boot)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !strings.Contains(err.Error(), "106") {
				t.Fatalf("error should name %s and 106, got %v", tt.wantErr, err)
			}
		})
	}
}
