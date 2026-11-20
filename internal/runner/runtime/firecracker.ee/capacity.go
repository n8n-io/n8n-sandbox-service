package firecracker

import (
	"context"
	"fmt"
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

// ResolveConfig loads the Firecracker configuration and sets
// runnerCfg.CapacityTotal to the slot count, before the runner listens. The
// count needs the guest size in boot.json, so the golden snapshot is built first
// when any of its files is missing. A set SANDBOX_RUNNER_CAPACITY_TOTAL may only
// lower the count host memory holds. Call it before New.
//
// A stop signal during a build needs no handling here. systemd stops the whole
// service, so the create script gets the signal too and cleans up after itself,
// and reap clears what a hard kill leaves behind on the next start.
func ResolveConfig(runnerCfg *config.Config) (Config, error) {
	return resolveConfig(context.Background(), runnerCfg, hostinfo.MeminfoPath,
		snapshotBuild{check: checkSnapshotBuild, build: buildGoldenSnapshot, reap: reapInterruptedSnapshotBuilds})
}

// snapshotBuild is how ResolveConfig builds a missing golden snapshot. check
// looks for the known problems that would fail every try, so they stop
// startup; build is tried again until it works. reap clears a snapshot build a
// previous process was killed in the middle of. Nil reap skips that.
type snapshotBuild struct {
	check func(Config) error
	build func(context.Context, Config) error
	reap  func(context.Context, Config) error
}

// checkSnapshotBuild runs the checks admission runs before it builds the
// snapshot: the Firecracker binaries, the template kernel and rootfs, who owns
// the kernel, and the release manifest. It also checks that the create script
// and the daemon binary exist.
func checkSnapshotBuild(cfg Config) error {
	r := &Runtime{config: cfg, deps: defaultDependencies(cfg)}
	if err := r.pinBaseAssets(); err != nil {
		return err
	}
	if missing := missingPaths(cfg.CreateSnapshotScript, cfg.DaemonBin); len(missing) > 0 {
		return fmt.Errorf("missing %s", strings.Join(missing, ", "))
	}
	return nil
}

// buildGoldenSnapshot runs the snapshot build Prepare uses, before New exists.
// ensureGoldenSnapshot needs only the config and the dependencies.
func buildGoldenSnapshot(ctx context.Context, cfg Config) error {
	r := &Runtime{config: cfg, deps: defaultDependencies(cfg)}
	return r.ensureGoldenSnapshot(ctx)
}

func resolveConfig(ctx context.Context, runnerCfg *config.Config, meminfo string, snapshot snapshotBuild) (Config, error) {
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
	// Before the build, and also when the files are already complete: a stop
	// during the previous build exits before New's reconcile, so this is the
	// only pass that runs on a start which is itself stopped in the same place.
	if snapshot.reap != nil {
		if err := snapshot.reap(ctx, cfg); err != nil {
			return Config{}, err
		}
	}
	if err := ensureSnapshotForSizing(ctx, cfg, snapshot); err != nil {
		return Config{}, err
	}
	boot, err := loadBootParams(bootParamsPath(cfg))
	if err != nil {
		return Config{}, err
	}
	// Admission rejects a snapshot that does not match the runner's settings, and
	// the fix is to delete it so it is rebuilt. Stop here instead, so the slot
	// count never comes from a snapshot that will be replaced.
	if err := boot.matchesConfig(cfg); err != nil {
		return Config{}, err
	}
	slots, limit, err := slotCount(cfg, runnerCfg, boot, meminfo)
	if err != nil {
		return Config{}, err
	}
	runnerCfg.CapacityTotal = int32(slots)
	slog.Info("firecracker slot count set", "slots", slots, "limit", limit, "mem_size_mib", boot.MemSizeMiB)
	return cfg, nil
}

// ensureSnapshotForSizing builds the golden snapshot when any of its files is
// missing. It first runs snapshot.check and stops with an error if that fails:
// those problems would fail every try, and the runner is not listening yet, so
// nothing but the log would show them. A failed build is then tried again,
// waiting longer each time, instead of exiting. An exit would make systemd
// restart the runner within seconds and start a new build each time.
func ensureSnapshotForSizing(ctx context.Context, cfg Config, snapshot snapshotBuild) error {
	missing := missingPaths(cfg.SnapshotMemPath, cfg.SnapshotStatePath, bootParamsPath(cfg))
	if len(missing) == 0 {
		return nil
	}
	if cfg.CreateSnapshotScript == "" {
		return fmt.Errorf("golden snapshot is missing %s and SANDBOX_RUNNER_FIRECRACKER_CREATE_SNAPSHOT_SCRIPT is not set; the slot count needs its boot.json",
			strings.Join(missing, ", "))
	}
	if err := snapshot.check(cfg); err != nil {
		return fmt.Errorf("cannot build the golden snapshot: %w", err)
	}
	slog.Info("firecracker building the golden snapshot before startup; the slot count needs its boot.json", "missing", missing)
	backoff := admissionRetryInitial
	for {
		err := snapshot.build(ctx, cfg)
		if err == nil {
			return nil
		}
		slog.Error("firecracker golden snapshot build failed; retrying", "error", err, "retry_in", backoff)
		if !sleepContext(ctx, backoff) {
			return fmt.Errorf("build golden snapshot: %w", err)
		}
		backoff = min(backoff*2, admissionRetryMax)
	}
}

// missingPaths returns the paths that do not exist.
func missingPaths(paths ...string) []string {
	var missing []string
	for _, path := range paths {
		if !pathExists(path) {
			missing = append(missing, path)
		}
	}
	return missing
}

// slotCount is the set SANDBOX_RUNNER_CAPACITY_TOTAL, which may not exceed what
// memory holds, or else the memory-derived count capped by the proxy port range
// and the uplink addressing. limit names what bound the count.
func slotCount(cfg Config, runnerCfg *config.Config, boot *bootParams, meminfo string) (slots int64, limit string, err error) {
	memorySlots, budget, ratio, err := memorySlotsFor(boot, meminfo)
	if err != nil {
		return 0, "", err
	}
	if runnerCfg.CapacityTotalSet {
		if int64(runnerCfg.CapacityTotal) > memorySlots {
			return 0, "", fmt.Errorf("SANDBOX_RUNNER_CAPACITY_TOTAL %d exceeds the %d slots host memory holds (%d MiB MemTotal, %d MiB per guest plus %d MiB overhead, %d MiB reserved, ratio %g); lower it or leave it unset to derive it",
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
//		floor((budget - fixedReserve) * ratio / (memSizeMiB<<20 + perVMOverhead))
//
//	  - budget: the host's MemTotal, in bytes (memorySlotsFor).
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
// It returns an error when budget is not above fixedReserve, when memSizeMiB
// does not fit in a byte length next to perVMOverhead (the shift would wrap
// and the floored count would be the structural ceiling), and when the floored
// count is below one slot.
func memorySlotCount(budget, memSizeMiB int64, ratio float64) (int64, error) {
	if budget <= fixedReserve {
		return 0, fmt.Errorf("host memory budget %d MiB is not above the %d MiB reserved for the runner and OS", budget>>20, fixedReserve>>20)
	}
	// memSizeMiB<<20 wraps once the guest is larger than what int64 can hold in
	// bytes. The wrapped per-slot size is small, so the count hits the proxy and
	// uplink ceilings and the admission canary then asks Firecracker for the
	// real, enormous guest.
	if memSizeMiB <= 0 || memSizeMiB > (math.MaxInt64-perVMOverhead)>>20 {
		return 0, fmt.Errorf("mem_size_mib %d is not a guest size host memory can budget", memSizeMiB)
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
func memorySlotsFor(boot *bootParams, meminfo string) (slots, budget int64, ratio float64, err error) {
	ratio, err = memoryRatio()
	if err != nil {
		return 0, 0, 0, err
	}
	budget, err = hostinfo.MemTotal(meminfo)
	if err != nil {
		return 0, 0, 0, err
	}
	slots, err = memorySlotCount(budget, int64(boot.MemSizeMiB), ratio)
	return slots, budget, ratio, err
}
