package firecracker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/n8n-io/sandbox-service/internal/runner/config"
	"github.com/n8n-io/sandbox-service/internal/runner/hostinfo"
	fcnetwork "github.com/n8n-io/sandbox-service/internal/runner/runtime/firecracker.ee/network"
)

const (
	// fixedReserve is host memory kept from guests: the runner, the OS, and the
	// one golden-snapshot file every guest shares. It is not scaled by slots.
	fixedReserve int64 = 4 << 30 // 4 GiB
	// perVMOverhead is host memory each microVM needs on top of its guest RAM.
	perVMOverhead int64 = 64 << 20 // 64 MiB

	memoryRatioEnv = "SANDBOX_RUNNER_FIRECRACKER_MEMORY_RATIO"
)

// hostPaths are the files the host memory budget is read from.
type hostPaths struct {
	meminfo, selfCgroup, cgroupRoot string
}

var defaultHostPaths = hostPaths{hostinfo.MeminfoPath, hostinfo.SelfCgroupPath, hostinfo.CgroupRoot}

// ResolveConfig loads the Firecracker configuration for main. A set
// SANDBOX_RUNNER_CAPACITY_TOTAL is validated like any other setting. When
// boot.json already exists, the memory inputs are checked too, so a bad ratio,
// an unreadable meminfo or a set count above what memory holds stops the runner
// before it listens. The slot count itself is worked out in admission
// (sizeSlots), after Prepare has made sure boot.json exists: on a new VM the
// snapshot, and with it the guest size, does not exist until then.
func ResolveConfig(runnerCfg *config.Config) (Config, error) {
	return resolveConfig(runnerCfg, defaultHostPaths)
}

func resolveConfig(runnerCfg *config.Config, host hostPaths) (Config, error) {
	if _, err := memoryRatio(); err != nil {
		return Config{}, err
	}
	var cfg Config
	var err error
	if runnerCfg.CapacityTotalSet {
		cfg, err = LoadConfig(runnerCfg.CapacityTotal)
	} else {
		cfg, err = loadConfigWithoutCapacity()
	}
	if err != nil {
		return Config{}, err
	}
	boot, err := loadBootParams(bootParamsPath(cfg))
	if errors.Is(err, fs.ErrNotExist) {
		slog.Info("firecracker boot.json not found; admission works out the slot count once the golden snapshot exists")
		return cfg, nil
	}
	if err != nil {
		return Config{}, err
	}
	if _, _, err := slotCount(cfg, runnerCfg, boot, host); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// slotCount is the set SANDBOX_RUNNER_CAPACITY_TOTAL, which may not exceed what
// memory holds, or else the memory-derived count capped by the proxy port range
// and the uplink addressing. limit names what bound a derived count.
func slotCount(cfg Config, runnerCfg *config.Config, boot *bootParams, host hostPaths) (slots int64, limit string, err error) {
	memorySlots, budget, ratio, err := memorySlotsFor(boot, host)
	if err != nil {
		return 0, "", err
	}
	if runnerCfg.CapacityTotalSet {
		if int64(runnerCfg.CapacityTotal) > memorySlots {
			return 0, "", fmt.Errorf("SANDBOX_RUNNER_CAPACITY_TOTAL %d exceeds the %d slots host memory holds (%d MiB budget, %d MiB per guest plus %d MiB overhead, %d MiB reserved, ratio %g); lower it or leave it unset to derive it",
				runnerCfg.CapacityTotal, memorySlots, budget>>20, boot.MemSizeMiB, perVMOverhead>>20, fixedReserve>>20, ratio)
		}
		return int64(runnerCfg.CapacityTotal), "SANDBOX_RUNNER_CAPACITY_TOTAL", nil
	}
	slots, limit = memorySlots, "memory"
	if ports := int64(65535 - cfg.ProxyPortStart + 1); ports < slots {
		slots, limit = ports, "proxy port range"
	}
	if fcnetwork.MaxSlots < slots {
		slots, limit = fcnetwork.MaxSlots, "uplink addressing"
	}
	return slots, limit, nil
}

func memoryRatio() (float64, error) {
	raw := strings.TrimSpace(os.Getenv(memoryRatioEnv))
	if raw == "" {
		return 1.0, nil
	}
	ratio, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio <= 0 {
		return 0, fmt.Errorf("%s must be a finite number above 0, got %q", memoryRatioEnv, raw)
	}
	return ratio, nil
}

// memorySlotCount is how many sandboxes (slots) host memory holds. Every
// microVM is assumed to use all of its guest RAM:
//
//		floor((budget - fixedReserve) * ratio / (memSizeMiB + perVMOverhead))
//
//	  - budget: the host's MemTotal, or the memory limit of the runner's cgroup
//	    when that is lower, in bytes (memorySlotsFor).
//	  - fixedReserve: 4 GiB kept for the runner, the OS, and the shared golden
//	    snapshot. It does not grow with the slot count.
//	  - ratio: SANDBOX_RUNNER_FIRECRACKER_MEMORY_RATIO (default 1.0). Above 1.0
//	    overcommits host memory.
//	  - memSizeMiB: guest RAM from the golden snapshot's boot.json, in MiB,
//	    converted to bytes before it is added to perVMOverhead.
//	  - perVMOverhead: 64 MiB of Firecracker host memory on top of each guest.
//
// The division rounds down. Example: a 64 GiB host with 512 MiB guests gets
// (64 GiB - 4 GiB) / 576 MiB = 106 slots.
//
// It returns an error when budget is not above fixedReserve, and when the
// floored count is below one slot.
func memorySlotCount(budget, memSizeMiB int64, ratio float64) (int64, error) {
	if budget <= fixedReserve {
		return 0, fmt.Errorf("host memory budget %d MiB is not above the %d MiB reserved for the runner and OS", budget>>20, fixedReserve>>20)
	}
	perSlot := memSizeMiB<<20 + perVMOverhead
	slots := math.Floor(float64(budget-fixedReserve) * ratio / float64(perSlot))
	if slots < 1 {
		return 0, fmt.Errorf("host memory budget %d MiB leaves no room for one %d MiB guest plus %d MiB overhead after the %d MiB reserve",
			budget>>20, memSizeMiB, perVMOverhead>>20, fixedReserve>>20)
	}
	return int64(math.Min(slots, math.MaxInt32)), nil
}

// memorySlotsFor returns the slot count host memory holds, and the inputs behind it.
func memorySlotsFor(boot *bootParams, host hostPaths) (slots, budget int64, ratio float64, err error) {
	ratio, err = memoryRatio()
	if err != nil {
		return 0, 0, 0, err
	}
	budget, err = hostinfo.MemoryBudget(host.meminfo, host.selfCgroup, host.cgroupRoot)
	if err != nil {
		return 0, 0, 0, err
	}
	slots, err = memorySlotCount(budget, int64(boot.MemSizeMiB), ratio)
	return slots, budget, ratio, err
}

// sizeSlots allocates the slot array from the boot.json admission pinned and
// starts the slot wirer. Slots back proxy ports, netns and TAP devices, so the
// first admission to get here fixes the count for the life of the process. A
// later one keeps that count when it still fits, and fails when a rebuilt
// snapshot fits fewer slots than the ones already allocated.
func (r *Runtime) sizeSlots(ctx context.Context, boot *bootParams) error {
	host := r.hostPaths
	if host == (hostPaths{}) {
		host = defaultHostPaths
	}
	want, limit, err := slotCount(r.config, r.runnerConfig, boot, host)
	if err != nil {
		return err
	}
	r.mu.Lock()
	have := len(r.slots)
	if have == 0 {
		r.slots = make([]slotState, want)
	}
	r.mu.Unlock()
	if have > 0 && int64(have) > want {
		return fmt.Errorf("firecracker has %d slots, but the golden snapshot rebuilt since then holds %d; restart the runner", have, want)
	}
	if have == 0 {
		slog.Info("firecracker slot count set", "slots", want, "limit", limit, "mem_size_mib", boot.MemSizeMiB)
	}
	r.wireOnce.Do(func() { go r.wireSlots(ctx) })
	return nil
}
