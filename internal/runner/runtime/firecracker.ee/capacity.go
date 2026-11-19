package firecracker

import (
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

// ResolveCapacity sets runnerCfg.CapacityTotal: derived from host memory when
// SANDBOX_RUNNER_CAPACITY_TOTAL is unset, otherwise the set value, which may
// only lower the derived count. Call it before LoadConfig and New.
func ResolveCapacity(runnerCfg *config.Config) error {
	return resolveCapacity(runnerCfg, defaultHostPaths)
}

func resolveCapacity(runnerCfg *config.Config, host hostPaths) error {
	if _, err := memoryRatio(); err != nil {
		return err
	}
	cfg, err := parseConfig()
	if err != nil {
		return err
	}
	boot, err := loadBootParams(bootParamsPath(cfg))
	if err != nil {
		if runnerCfg.CapacityTotalSet && errors.Is(err, fs.ErrNotExist) {
			// Prepare builds the snapshot; admission checks the count then.
			slog.Info("boot.json not found; starting at the explicit slot count", "slots", runnerCfg.CapacityTotal)
			return nil
		}
		return fmt.Errorf("derive slot count from guest memory: %w", err)
	}
	memorySlots, budget, ratio, err := memorySlotsFor(boot, host)
	if err != nil {
		return err
	}

	if runnerCfg.CapacityTotalSet {
		return slotCountError(runnerCfg.CapacityTotal, memorySlots, budget, boot, ratio, true)
	}
	slots, binding := memorySlots, "memory"
	if cfg.ProxyPortStart > 0 && cfg.ProxyPortStart <= 65535 {
		if ports := int64(65535 - cfg.ProxyPortStart + 1); ports < slots {
			slots, binding = ports, "proxy port range"
		}
	}
	if fcnetwork.MaxSlots < slots {
		slots, binding = fcnetwork.MaxSlots, "uplink addressing"
	}
	if binding != "memory" {
		slog.Info("firecracker slot count limited below what host memory holds", "slots", slots, "memory_slots", memorySlots, "limit", binding)
	}
	runnerCfg.CapacityTotal = int32(slots)
	slog.Info("firecracker slot count derived", "slots", slots, "memory_budget_bytes", budget, "mem_size_mib", boot.MemSizeMiB, "ratio", ratio)
	return nil
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
	budget, err = hostinfo.MemTotal(host.meminfo)
	if err != nil {
		return 0, 0, 0, err
	}
	limit, ok, err := hostinfo.CgroupMemoryLimit(host.selfCgroup, host.cgroupRoot)
	if err != nil {
		return 0, 0, 0, err
	}
	if ok && limit < budget {
		budget = limit
	}
	slots, err = memorySlotCount(budget, int64(boot.MemSizeMiB), ratio)
	return slots, budget, ratio, err
}

func slotCountError(slots int32, memorySlots, budget int64, boot *bootParams, ratio float64, explicit bool) error {
	if int64(slots) <= memorySlots {
		return nil
	}
	detail := fmt.Sprintf("%d exceeds the %d slots host memory holds (%d MiB budget, %d MiB per guest plus %d MiB overhead, %d MiB reserved, ratio %g)",
		slots, memorySlots, budget>>20, boot.MemSizeMiB, perVMOverhead>>20, fixedReserve>>20, ratio)
	if explicit {
		return fmt.Errorf("SANDBOX_RUNNER_CAPACITY_TOTAL %s; lower it or leave it unset to derive it", detail)
	}
	return fmt.Errorf("firecracker slot count %s; it was fixed at startup from an older boot.json, so restart the runner", detail)
}

// checkCapacityAtAdmission compares the allocated slot count with the boot.json
// admission pinned. Prepare may have just created it, and a derived count is
// never revised in place.
func (r *Runtime) checkCapacityAtAdmission(boot *bootParams) error {
	host := r.hostPaths
	if host == (hostPaths{}) {
		host = defaultHostPaths
	}
	memorySlots, budget, ratio, err := memorySlotsFor(boot, host)
	if err != nil {
		return err
	}
	return slotCountError(r.runnerConfig.CapacityTotal, memorySlots, budget, boot, ratio, r.runnerConfig.CapacityTotalSet)
}
