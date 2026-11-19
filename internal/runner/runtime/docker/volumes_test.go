package docker

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/runner/config"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
	"github.com/n8n-io/sandbox-service/internal/safetar"
	"github.com/n8n-io/sandbox-service/internal/volume"
)

// fakeVolumeCache hands out a tree per sha256, or the error set for it, and
// records pins and the order of calls in the backend's events.
type fakeVolumeCache struct {
	mu     sync.Mutex
	record func(event string)
	errs   map[string]error
	// block makes Acquire of that sha256 wait for its context.
	block  map[string]bool
	pinned map[string][]string
}

func (f *fakeVolumeCache) Acquire(ctx context.Context, vol runnerruntime.Volume, holder string) (string, error) {
	if f.block[vol.SHA256] {
		<-ctx.Done()
		return "", ctx.Err()
	}
	f.record("acquire")
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs[vol.SHA256]; err != nil {
		return "", err
	}
	if f.pinned == nil {
		f.pinned = map[string][]string{}
	}
	f.pinned[holder] = append(f.pinned[holder], vol.SHA256)
	return "/cache/shared." + vol.SHA256 + ".1", nil
}

func (f *fakeVolumeCache) ReleaseAll(holder string) {
	f.record("release")
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.pinned, holder)
}

func (f *fakeVolumeCache) pins(holder string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Sorted(slices.Values(f.pinned[holder]))
}

const volumeSandbox = "sandbox-with-volumes"

func tarVolume(digit, mountPath string, optional bool) runnerruntime.Volume {
	return runnerruntime.Volume{
		Scope: volume.SharedScope, SHA256: strings.Repeat(digit, 64), Size: 10240, Kind: volume.KindTar,
		URL: "https://objects.example/v?sig=token", MountPath: mountPath, Optional: optional,
		Tar: volume.TarCounts{Entries: 3, ContentBytes: 100, DecompressedBytes: 10240},
	}
}

// volumeRuntime is a runtime whose Docker, network policy, daemon, mount flags
// and tree check all succeed unless a test says otherwise.
type volumeRuntime struct {
	*Runtime
	events  *[]string
	backend *fakeDockerBackend
	cache   *fakeVolumeCache
	rec     *metrics.RunnerRecorder
}

func newVolumeRuntime(t *testing.T) volumeRuntime {
	t.Helper()
	events := &[]string{}
	backend := &fakeDockerBackend{events: events, containerID: "container-1", ip: "172.18.0.2"}
	cache := &fakeVolumeCache{record: backend.record, errs: map[string]error{}, block: map[string]bool{}}
	runtime := newRuntime(&config.Config{VolumeCacheDir: "/cache"}, Config{}, backend)
	runtime.imageReady.Store(true)
	runtime.applyPolicy = func(string, string, string, string, int, bool) error { return nil }
	runtime.teardownRules = func(string) error { return nil }
	runtime.waitForDaemon = func(context.Context, string) error { return nil }
	runtime.setNoSuidNoDev = func(dir string) error {
		backend.record("flags " + dir)
		return nil
	}
	runtime.checkVolumeTree = func(string) error { return nil }
	rec := metrics.NewRunnerRecorder(true)
	runtime.SetMetricsRecorder(rec)
	runtime.SetVolumeCache(cache)
	backend.found = func([]string) []string {
		if backend.isCreated() {
			return []string{backend.containerID}
		}
		return nil
	}
	*events = (*events)[:0]
	return volumeRuntime{Runtime: runtime, events: events, backend: backend, cache: cache, rec: rec}
}

func (v volumeRuntime) create(volumes ...runnerruntime.Volume) (*runnerruntime.SandboxInfo, error) {
	return v.CreateSandbox(context.Background(), volumeSandbox, &runnerruntime.CreateOptions{Volumes: volumes})
}

func TestSetVolumeCachePreparesTheCacheDir(t *testing.T) {
	events := &[]string{}
	runtime := newRuntime(&config.Config{VolumeCacheDir: "/cache"}, Config{}, &fakeDockerBackend{events: events})
	runtime.setNoSuidNoDev = func(dir string) error {
		*events = append(*events, "flags "+dir)
		return errors.New("not yet")
	}
	runtime.SetVolumeCache(&fakeVolumeCache{record: func(event string) { *events = append(*events, event) }})
	if want := []string{"flags /cache"}; !reflect.DeepEqual(*events, want) {
		t.Fatalf("events = %v, want %v", *events, want)
	}
}

// Every volume is acquired before the container exists, and bound into it
// read-only once the cache dir is nosuid,nodev.
func TestCreateWithVolumesAcquiresBeforeTheContainer(t *testing.T) {
	runtime := newVolumeRuntime(t)
	first, second := tarVolume("a", "/mnt/a", false), tarVolume("b", "/mnt/b", true)

	info, err := runtime.create(first, second)
	if err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	want := []string{"find", "acquire", "acquire", "flags /cache", "find", "create", "start", "containerIP"}
	if !reflect.DeepEqual(*runtime.events, want) {
		t.Fatalf("events = %v, want %v", *runtime.events, want)
	}
	wantMounts := []volumeMount{
		{source: "/cache/shared." + first.SHA256 + ".1", target: "/mnt/a"},
		{source: "/cache/shared." + second.SHA256 + ".1", target: "/mnt/b"},
	}
	if !reflect.DeepEqual(runtime.backend.mounts, wantMounts) {
		t.Errorf("mounts = %+v, want %+v", runtime.backend.mounts, wantMounts)
	}
	if len(info.VolumesLeftOut) != 0 {
		t.Errorf("left out = %v, want none", info.VolumesLeftOut)
	}
	if pins := runtime.cache.pins(volumeSandbox); len(pins) != 2 {
		t.Errorf("pins = %v, want both volumes", pins)
	}
}

func TestDockerVolumeArgs(t *testing.T) {
	if args := dockerVolumeArgs(nil); args != nil {
		t.Errorf("args without volumes = %v, want none", args)
	}
	mounts := []volumeMount{
		{source: "/cache/shared.aa.1", target: "/mnt/a"},
		// A comma or a quote in a mount path must not start a field of its own.
		{source: "/cache/shared.bb.2", target: `/mnt/b,source=/etc,"x`},
	}
	args := dockerVolumeArgs(mounts)
	want := []string{
		"--label", "sandbox-service.volumes=true",
		"--mount", "type=bind,source=/cache/shared.aa.1,target=/mnt/a,readonly",
		"--mount", `type=bind,source=/cache/shared.bb.2,"target=/mnt/b,source=/etc,""x",readonly`,
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("dockerVolumeArgs() = %#v, want %#v", args, want)
	}
	// Docker reads a --mount value as one CSV record.
	fields, err := csv.NewReader(strings.NewReader(args[5])).Read()
	wantFields := []string{"type=bind", "source=/cache/shared.bb.2", `target=/mnt/b,source=/etc,"x`, "readonly"}
	if err != nil || !reflect.DeepEqual(fields, wantFields) {
		t.Errorf("Docker would read %q, %v; want %q", fields, err, wantFields)
	}

	createArgs := dockerContainerCreateArgs("sandbox-id", "default", "sandbox-name", "sandbox-image", runnerBridgeNetwork, nil, false, nil, true, mounts[:1])
	if tail := createArgs[len(createArgs)-5:]; !reflect.DeepEqual(tail, append(want[:4:4], "sandbox-image")) {
		t.Errorf("create args end in %q, want the volume args before the image", tail)
	}
}

// A required volume that fails fails the create before the container exists,
// keeps a capacity refusal one, and releases what the create pinned.
func TestCreateWithVolumesRequiredFailureRollsBack(t *testing.T) {
	for name, cacheErr := range map[string]error{
		"capacity": fmt.Errorf("volume cache full: %w", runnerruntime.ErrCapacityExhausted),
		"download": errors.New("download volume object: status 403"),
		"unpack":   fmt.Errorf("unpack volume tar: %w", safetar.ErrInvalid),
	} {
		t.Run(name, func(t *testing.T) {
			runtime := newVolumeRuntime(t)
			failing := tarVolume("b", "/mnt/b", false)
			runtime.cache.errs[failing.SHA256] = cacheErr

			_, err := runtime.create(tarVolume("a", "/mnt/a", true), failing)
			if !errors.Is(err, cacheErr) || !strings.Contains(err.Error(), "volume 1") {
				t.Fatalf("CreateSandbox() error = %v, want volume 1's error", err)
			}
			if slices.Contains(*runtime.events, "create") {
				t.Errorf("events = %v, want no container", *runtime.events)
			}
			if pins := runtime.cache.pins(volumeSandbox); len(pins) != 0 {
				t.Errorf("pins = %v after a failed create, want none", pins)
			}
		})
	}
}

// An optional volume that fails is left out: the sandbox starts with the
// others, and the echo and the metric say why.
func TestCreateWithVolumesLeavesOutAnOptionalVolume(t *testing.T) {
	for reason, cacheErr := range map[string]error{
		runnerruntime.VolumeLeftOutCacheFull: fmt.Errorf("volume cache full: %w", runnerruntime.ErrCapacityExhausted),
		runnerruntime.VolumeLeftOutTimeout:   context.DeadlineExceeded,
		runnerruntime.VolumeLeftOutDownload:  errors.New("download volume object: sha256 mismatch"),
		runnerruntime.VolumeLeftOutUnpack:    fmt.Errorf("unpack volume tar: %w", safetar.ErrTooLarge),
	} {
		t.Run(reason, func(t *testing.T) {
			runtime := newVolumeRuntime(t)
			required, optional := tarVolume("a", "/mnt/a", false), tarVolume("b", "/mnt/b", true)
			runtime.cache.errs[optional.SHA256] = cacheErr
			opts := &runnerruntime.CreateOptions{Volumes: []runnerruntime.Volume{required, optional}}

			info, err := runtime.CreateSandbox(context.Background(), volumeSandbox, opts)
			if err != nil {
				t.Fatalf("CreateSandbox() failed: %v", err)
			}
			if want := map[int]string{1: reason}; !reflect.DeepEqual(info.VolumesLeftOut, want) {
				t.Errorf("left out = %v, want %v", info.VolumesLeftOut, want)
			}
			if len(runtime.backend.mounts) != 1 || runtime.backend.mounts[0].target != "/mnt/a" {
				t.Errorf("mounts = %+v, want only /mnt/a", runtime.backend.mounts)
			}
			applied := opts.Applied(info.VolumesLeftOut)
			if !applied.Volumes[0].Attached || applied.Volumes[1].Attached || applied.Volumes[1].Reason != reason {
				t.Errorf("echo = %+v, want /mnt/a attached and /mnt/b left out for %s", applied.Volumes, reason)
			}
			if got := runtime.rec.VolumeLeftOutCount(reason); got != 1 {
				t.Errorf("volumes_left_out_total{reason=%s} = %v, want 1", reason, got)
			}
		})
	}
}

// An optional volume still downloading when the create's wait ends is left
// out, and the create still has its reserve for the rest.
func TestCreateWithVolumesStopsWaitingBeforeTheDeadline(t *testing.T) {
	previous := volumeAcquireReserve
	volumeAcquireReserve = time.Second
	t.Cleanup(func() { volumeAcquireReserve = previous })
	runtime := newVolumeRuntime(t)
	slow := tarVolume("b", "/mnt/b", true)
	runtime.cache.block[slow.SHA256] = true

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	info, err := runtime.CreateSandbox(ctx, volumeSandbox, &runnerruntime.CreateOptions{Volumes: []runnerruntime.Volume{tarVolume("a", "/mnt/a", false), slow}})
	if err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	if info.VolumesLeftOut[1] != runnerruntime.VolumeLeftOutTimeout {
		t.Errorf("left out = %v, want volume 1 for timeout", info.VolumesLeftOut)
	}
	if ctx.Err() != nil {
		t.Error("the create used up its whole deadline waiting for the volume")
	}
}

// A sandbox whose optional volumes were all left out gets no mount and no
// label, and the cache dir is left alone.
func TestCreateWithEveryVolumeLeftOut(t *testing.T) {
	runtime := newVolumeRuntime(t)
	optional := tarVolume("b", "/mnt/b", true)
	runtime.cache.errs[optional.SHA256] = errors.New("download volume object: status 404")

	if _, err := runtime.create(optional); err != nil {
		t.Fatal(err)
	}
	if runtime.backend.mounts != nil || slices.Contains(*runtime.events, "flags /cache") {
		t.Errorf("mounts = %+v, events = %v; want no mount and no flags", runtime.backend.mounts, *runtime.events)
	}
}

// The host's faults fail the create whatever the volume, and release its pins.
func TestCreateWithVolumesHostFaultsFailAnyVolume(t *testing.T) {
	for name, tt := range map[string]struct {
		breakHost func(volumeRuntime)
		want      error
	}{
		"cache dir flags": {func(runtime volumeRuntime) {
			runtime.setNoSuidNoDev = func(string) error { return errors.New("mount: operation not permitted") }
		}, runnerruntime.ErrVolumesUnsupported},
		"tree check": {func(runtime volumeRuntime) {
			runtime.checkVolumeTree = func(string) error { return errors.New("cached volume tree is not a directory") }
		}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			runtime := newVolumeRuntime(t)
			tt.breakHost(runtime)
			_, err := runtime.create(tarVolume("b", "/mnt/b", true))
			if err == nil || (tt.want != nil && !errors.Is(err, tt.want)) {
				t.Fatalf("CreateSandbox() error = %v, want a failure (%v)", err, tt.want)
			}
			if slices.Contains(*runtime.events, "create") {
				t.Errorf("events = %v, want no container", *runtime.events)
			}
			if pins := runtime.cache.pins(volumeSandbox); len(pins) != 0 {
				t.Errorf("pins = %v, want none", pins)
			}
		})
	}
}

// The cache pins by sandbox ID, so a create with volumes is refused while a
// container of the ID exists, whose pins a failed create would release.
func TestCreateWithVolumesRefusesAnIDThatHasAContainer(t *testing.T) {
	runtime := newVolumeRuntime(t)
	if _, err := runtime.create(tarVolume("a", "/mnt/a", false)); err != nil {
		t.Fatal(err)
	}
	*runtime.events = (*runtime.events)[:0]
	if _, err := runtime.create(tarVolume("b", "/mnt/b", false)); err == nil || !strings.Contains(err.Error(), "has a container already") {
		t.Fatalf("CreateSandbox() = %v, want a refusal", err)
	}
	if want := []string{"find"}; !reflect.DeepEqual(*runtime.events, want) {
		t.Errorf("events = %v, want %v", *runtime.events, want)
	}
	if pins := runtime.cache.pins(volumeSandbox); len(pins) != 1 {
		t.Errorf("pins = %v, want the first create's", pins)
	}
}

// A refused duplicate create leaves the existing sandbox's pins to its delete.
func TestRefusedCreateLeavesThePinsToTheDelete(t *testing.T) {
	runtime := newVolumeRuntime(t)
	if _, err := runtime.create(tarVolume("a", "/mnt/a", false)); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.create(tarVolume("b", "/mnt/b", false)); err == nil {
		t.Fatal("duplicate CreateSandbox() succeeded")
	}
	if pins := runtime.cache.pins(volumeSandbox); len(pins) != 1 {
		t.Fatalf("pins = %v after the refused create, want the first create's", pins)
	}
	if err := runtime.DeleteSandbox(context.Background(), volumeSandbox); err != nil {
		t.Fatal(err)
	}
	if pins := runtime.cache.pins(volumeSandbox); len(pins) != 0 {
		t.Errorf("pins = %v after the delete, want none", pins)
	}
}

// While a create with volumes runs, a second create of the ID is refused and a
// delete of it waits: it then removes the new container and releases its
// pins, never pins of a create in progress.
func TestCreateWithVolumesHoldsTheSandboxLock(t *testing.T) {
	runtime := newVolumeRuntime(t)
	slow := tarVolume("b", "/mnt/b", true)
	runtime.cache.block[slow.SHA256] = true
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	created := make(chan error, 1)
	go func() {
		_, err := runtime.CreateSandbox(ctx, volumeSandbox, &runnerruntime.CreateOptions{Volumes: []runnerruntime.Volume{tarVolume("a", "/mnt/a", false), slow}})
		created <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(runtime.cache.pins(volumeSandbox)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first create never pinned its volume")
		}
		time.Sleep(time.Millisecond)
	}

	if _, err := runtime.create(tarVolume("c", "/mnt/c", false)); err == nil || !strings.Contains(err.Error(), "being created or deleted") {
		t.Errorf("second CreateSandbox() = %v, want a refusal", err)
	}
	deleted := make(chan error, 1)
	go func() { deleted <- runtime.DeleteSandbox(context.Background(), volumeSandbox) }()
	if err := <-created; err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	if err := <-deleted; err != nil {
		t.Fatalf("DeleteSandbox() after the create = %v", err)
	}
	if pins := runtime.cache.pins(volumeSandbox); len(pins) != 0 {
		t.Errorf("pins = %v after the delete, want none", pins)
	}
	if runtime.backend.isCreated() {
		t.Error("the delete left the container the create made")
	}
	runtime.mu.Lock()
	locks := len(runtime.sandboxLocks)
	runtime.mu.Unlock()
	if locks != 0 {
		t.Errorf("%d sandbox locks left, want none", locks)
	}
}

// A delete gives up waiting for the lock when its context ends.
func TestDeleteWaitsForTheLockOnlyUntilItsDeadline(t *testing.T) {
	runtime := newVolumeRuntime(t)
	unlock, err := runtime.lockSandbox(context.Background(), volumeSandbox, false)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runtime.lockSandbox(ctx, volumeSandbox, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("lockSandbox() = %v, want %v", err, context.Canceled)
	}
}

// Docker's errors quote the paths it could not bind; the error that can reach
// the client names the cache dir only as such, however it was configured.
func TestCreateWithVolumesErrorsLeaveOutTheCacheDir(t *testing.T) {
	for _, configured := range []string{"/cache", "/cache/", "//cache/./"} {
		runtime := newVolumeRuntime(t)
		runtime.runnerConfig.VolumeCacheDir = configured
		vol := tarVolume("a", "/mnt/a", false)
		runtime.backend.createErr = fmt.Errorf("docker container create --mount type=bind,source=/cache/shared.%s.1: %w", vol.SHA256, context.DeadlineExceeded)
		_, err := runtime.create(vol)
		if err == nil || strings.Contains(err.Error(), "/cache/") || !strings.Contains(err.Error(), "<volume cache>/shared.") {
			t.Fatalf("dir %q: CreateSandbox() error = %v, want the cache dir left out", configured, err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("error %v lost what it wraps", err)
		}
	}
	if got := (withoutCachePaths{err: errors.New("a.b"), dir: ""}).Error(); got != "a.b" {
		t.Errorf("without a dir, Error() = %q, want the error unchanged", got)
	}
}

// The tree check names the volume whose tree failed, after one left out.
func TestCreateWithVolumesTreeCheckNamesItsVolume(t *testing.T) {
	runtime := newVolumeRuntime(t)
	leftOut, checked := tarVolume("a", "/mnt/a", true), tarVolume("b", "/mnt/b", false)
	runtime.cache.errs[leftOut.SHA256] = errors.New("download volume object: status 404")
	runtime.checkVolumeTree = func(path string) error {
		if strings.Contains(path, checked.SHA256) {
			return errors.New("cached volume tree is not a directory")
		}
		return nil
	}
	if _, err := runtime.create(leftOut, checked); err == nil || !strings.Contains(err.Error(), "volume at /mnt/b") {
		t.Fatalf("CreateSandbox() error = %v, want it to name /mnt/b", err)
	}
}

// A create that fails once the container exists releases the pins if the
// container is gone, and keeps them if its cleanup left it behind.
func TestCreateWithVolumesContainerFailure(t *testing.T) {
	for name, tt := range map[string]struct {
		removeErr error
		wantPins  int
	}{
		"container removed":     {nil, 0},
		"container left behind": {errors.New("removal in progress"), 1},
	} {
		t.Run(name, func(t *testing.T) {
			runtime := newVolumeRuntime(t)
			runtime.backend.removeErr = tt.removeErr
			runtime.waitForDaemon = func(context.Context, string) error { return errors.New("daemon never ready") }

			if _, err := runtime.create(tarVolume("a", "/mnt/a", false)); err == nil {
				t.Fatal("CreateSandbox() succeeded")
			}
			if pins := runtime.cache.pins(volumeSandbox); len(pins) != tt.wantPins {
				t.Errorf("pins = %v, want %d", pins, tt.wantPins)
			}
		})
	}
}

func TestCreateWithVolumesRefusals(t *testing.T) {
	for name, tt := range map[string]struct {
		volume  runnerruntime.Volume
		noCache bool
	}{
		"ext4 volume": {volume: func() runnerruntime.Volume {
			vol := tarVolume("a", "/mnt/a", true)
			vol.Kind, vol.Tar = volume.KindExt4, volume.TarCounts{}
			return vol
		}()},
		"no volume cache": {volume: tarVolume("a", "/mnt/a", true), noCache: true},
	} {
		t.Run(name, func(t *testing.T) {
			runtime := newVolumeRuntime(t)
			if tt.noCache {
				runtime.volumeCache = nil
			}
			if _, err := runtime.create(tt.volume); !errors.Is(err, runnerruntime.ErrVolumesUnsupported) {
				t.Fatalf("CreateSandbox() error = %v, want %v", err, runnerruntime.ErrVolumesUnsupported)
			}
			if len(*runtime.events) != 0 {
				t.Errorf("events = %v, want nothing looked up, acquired or created", *runtime.events)
			}
		})
	}
}

// A sandbox without volumes is created as before: no acquire, no flags, no
// mounts.
func TestCreateWithoutVolumesIsUnchanged(t *testing.T) {
	runtime := newVolumeRuntime(t)
	info, err := runtime.CreateSandbox(context.Background(), volumeSandbox, &runnerruntime.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"find", "create", "start", "containerIP"}; !reflect.DeepEqual(*runtime.events, want) || runtime.backend.mounts != nil {
		t.Errorf("events = %v, mounts = %v; want %v and none", *runtime.events, runtime.backend.mounts, want)
	}
	if info.VolumesLeftOut != nil {
		t.Errorf("left out = %v, want nil", info.VolumesLeftOut)
	}
}

// Stop keeps the pins; delete releases them once the container is gone, and
// not while it may still be there.
func TestDeleteReleasesVolumesOnceTheContainerIsGone(t *testing.T) {
	runtime := newVolumeRuntime(t)
	if _, err := runtime.create(tarVolume("a", "/mnt/a", false)); err != nil {
		t.Fatal(err)
	}
	runtime.backend.removeErr = errors.New("removal in progress")
	if err := runtime.DeleteSandbox(context.Background(), volumeSandbox); err == nil {
		t.Fatal("DeleteSandbox() succeeded")
	}
	if pins := runtime.cache.pins(volumeSandbox); len(pins) != 1 {
		t.Fatalf("pins = %v after a failed delete, want the volume", pins)
	}
	runtime.backend.removeErr = nil
	if err := runtime.DeleteSandbox(context.Background(), volumeSandbox); err != nil {
		t.Fatal(err)
	}
	if pins := runtime.cache.pins(volumeSandbox); len(pins) != 0 {
		t.Fatalf("pins = %v after delete, want none", pins)
	}
}

// A container a failed create reported left behind but that is gone after
// all releases its pins on the next delete.
func TestDeleteWithoutAContainerReleasesVolumes(t *testing.T) {
	runtime := newVolumeRuntime(t)
	runtime.backend.removeErr = errors.New("timed out")
	runtime.waitForDaemon = func(context.Context, string) error { return errors.New("daemon never ready") }
	if _, err := runtime.create(tarVolume("a", "/mnt/a", false)); err == nil {
		t.Fatal("CreateSandbox() succeeded")
	}
	if pins := runtime.cache.pins(volumeSandbox); len(pins) != 1 {
		t.Fatalf("pins = %v, want the left-behind container's", pins)
	}
	runtime.backend.created = false
	if err := runtime.DeleteSandbox(context.Background(), volumeSandbox); !errors.Is(err, ErrSandboxNotFound) {
		t.Fatalf("DeleteSandbox() = %v, want %v", err, ErrSandboxNotFound)
	}
	if pins := runtime.cache.pins(volumeSandbox); len(pins) != 0 {
		t.Errorf("pins = %v, want none", pins)
	}
}

func TestStopKeepsVolumesPinned(t *testing.T) {
	runtime := newVolumeRuntime(t)
	if _, err := runtime.create(tarVolume("a", "/mnt/a", false)); err != nil {
		t.Fatal(err)
	}
	if err := runtime.StopSandbox(context.Background(), volumeSandbox); err != nil {
		t.Fatal(err)
	}
	if pins := runtime.cache.pins(volumeSandbox); len(pins) != 1 {
		t.Fatalf("pins = %v after stop, want the volume", pins)
	}
}

// Startup fails while a container with volumes survives its removal: the cache
// wipe deletes its trees, and the runner would serve it without them.
func TestRemoveVolumeContainersLeft(t *testing.T) {
	for name, tt := range map[string]struct {
		survivors []string
		wantErr   bool
	}{
		"none":     {nil, false},
		"survivor": {[]string{"container-1"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			events := &[]string{}
			var filters [][]string
			backend := &fakeDockerBackend{events: events, containerID: "container-1", removeErr: errors.New("device busy")}
			backend.found = func(given []string) []string {
				filters = append(filters, given)
				return tt.survivors
			}
			runtime := newRuntime(&config.Config{}, Config{}, backend)
			runtime.teardownRules = func(string) error { return nil }

			err := runtime.removeVolumeContainersLeft(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("removeVolumeContainersLeft() = %v, want error %t", err, tt.wantErr)
			}
			for _, given := range filters {
				if !slices.Contains(given, volumesLabelFilter) || !slices.Contains(given, managedLabelFilter) {
					t.Errorf("lookup filters = %v, want managed containers with volumes", given)
				}
			}
			if tt.survivors != nil && !slices.Contains(*events, "remove") {
				t.Errorf("events = %v, want another removal", *events)
			}
		})
	}
}

func TestCheckVolumeTreeRefusesTreesASandboxCouldChange(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	writable := filepath.Join(root, "writable")
	if err := os.Mkdir(writable, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(writable, 0o777); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(root, "good")
	if err := os.Mkdir(good, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"file": file, "writable": writable, "link": link, "missing": filepath.Join(root, "missing")} {
		err := checkVolumeTree(path)
		if err == nil || strings.Contains(err.Error(), root) {
			t.Errorf("%s: checkVolumeTree() = %v, want an error that names no path", name, err)
		}
	}
	// A good tree passes up to the mount flags, which a temp dir may lack.
	if err := checkVolumeTree(good); err != nil && !strings.Contains(err.Error(), "nosuid") && !strings.Contains(err.Error(), "Linux") {
		t.Errorf("checkVolumeTree() of a good tree = %v, want at most a mount flag error", err)
	}
}
