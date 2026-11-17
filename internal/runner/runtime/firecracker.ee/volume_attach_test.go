package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/daemon/volumedrives"
	"github.com/n8n-io/sandbox-service/internal/metrics"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
	"github.com/n8n-io/sandbox-service/internal/volume"
)

const volumeTestSandbox = "sandbox-id-123456"

func testVolume(index int, optional bool) runnerruntime.Volume {
	return runnerruntime.Volume{
		Scope:     volume.SharedScope,
		SHA256:    fmt.Sprintf("%064x", index+1),
		Size:      int64(index+1) << 20,
		Kind:      volume.KindExt4,
		URL:       fmt.Sprintf("https://objects.example/%d?sig=secret-token", index),
		MountPath: fmt.Sprintf("/mnt/v%d", index),
		Optional:  optional,
	}
}

func volumeOptions(volumes ...runnerruntime.Volume) *runnerruntime.CreateOptions {
	return &runnerruntime.CreateOptions{Egress: runnerruntime.EgressPublic, Volumes: volumes}
}

func cachedImage(vol runnerruntime.Volume) string {
	return "/var/cache/volumes/shared." + vol.SHA256 + ".1"
}

// fakeVolumeCache pins by holder, like the real cache. An object listed in
// stall waits for ctx; with a gate, every Acquire waits for it to close.
type fakeVolumeCache struct {
	mu       sync.Mutex
	record   func(string)
	errs     map[string]error
	stall    map[string]bool
	gate     chan struct{}
	pins     map[string]map[string]bool
	releases int
}

func (c *fakeVolumeCache) Acquire(ctx context.Context, vol runnerruntime.Volume, holder string) (string, error) {
	c.record("acquire " + vol.MountPath)
	c.mu.Lock()
	err, stall := c.errs[vol.SHA256], c.stall[vol.SHA256]
	c.mu.Unlock()
	if stall {
		<-ctx.Done()
		return "", ctx.Err()
	}
	if c.gate != nil {
		<-c.gate
	}
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pins[holder] == nil {
		c.pins[holder] = map[string]bool{}
	}
	c.pins[holder][vol.SHA256] = true
	return cachedImage(vol), nil
}

func (c *fakeVolumeCache) ReleaseAll(holder string) {
	c.record("release " + holder)
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pins, holder)
	c.releases++
}

func (c *fakeVolumeCache) pinned(holder string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pins[holder])
}

// volumeRig is a runtime whose host and guest calls are recorded in order.
type volumeRig struct {
	t     *testing.T
	rt    *Runtime
	cache *fakeVolumeCache
	rec   *metrics.RunnerRecorder

	mu       sync.Mutex
	events   []string
	scripts  []string
	mounts   [][]volumedrives.Volume
	failed   map[int]string
	mountErr error
	runErr   func(script string) error
	patchErr error
	imageErr error
}

func newVolumeRig(t *testing.T) *volumeRig {
	t.Helper()
	rt := testRuntimeT(t, 2)
	stubCreateDeps(rt)
	rig := &volumeRig{t: t, rt: rt, rec: metrics.NewRunnerRecorder(true)}
	rig.cache = &fakeVolumeCache{record: rig.record, errs: map[string]error{}, stall: map[string]bool{}, pins: map[string]map[string]bool{}}
	rt.SetVolumeCache(rig.cache)
	rt.SetMetricsRecorder(rig.rec)

	rt.deps.checkVolumeImage = func(string) error { return rig.imageErr }
	cloneRootfs := rt.deps.cloneRootfs
	rt.deps.cloneRootfs = func(ctx context.Context, templatePath, destPath string) error {
		rig.record("clone rootfs")
		return cloneRootfs(ctx, templatePath, destPath)
	}
	rt.deps.run = func(_ context.Context, _ string, args ...string) error {
		script := args[len(args)-1]
		rig.mu.Lock()
		rig.scripts = append(rig.scripts, script)
		runErr := rig.runErr
		rig.mu.Unlock()
		switch {
		case strings.Contains(script, "touch "):
			rig.record("prepare jail")
		case strings.Contains(script, "rm -rf"):
			rig.record("cleanup host")
		default:
			rig.record("run")
		}
		if runErr != nil {
			return runErr(script)
		}
		return nil
	}
	rt.deps.start = func(context.Context, func(error), string, ...string) (process, error) {
		rig.record("start jailer")
		return &fakeProcess{}, nil
	}
	rt.deps.loadSnapshot = func(context.Context, string, Config) error {
		rig.record("load snapshot")
		return nil
	}
	rt.deps.patchVolumeDrive = func(_ context.Context, _ string, drive volumeDrive) error {
		rig.record("patch " + drive.DriveID + " " + drive.Path)
		return rig.patchErr
	}
	rt.deps.mountVolumeDrives = func(_ context.Context, _ string, drives []volumedrives.Volume) (map[int]string, error) {
		rig.record("mount request")
		rig.mu.Lock()
		defer rig.mu.Unlock()
		rig.mounts = append(rig.mounts, drives)
		return rig.failed, rig.mountErr
	}
	return rig
}

func (rig *volumeRig) record(event string) {
	rig.mu.Lock()
	defer rig.mu.Unlock()
	rig.events = append(rig.events, event)
}

func (rig *volumeRig) recorded() []string {
	rig.mu.Lock()
	defer rig.mu.Unlock()
	return slices.Clone(rig.events)
}

func (rig *volumeRig) lastScript(t *testing.T, contains string) string {
	t.Helper()
	rig.mu.Lock()
	defer rig.mu.Unlock()
	for index := len(rig.scripts) - 1; index >= 0; index-- {
		if strings.Contains(rig.scripts[index], contains) {
			return rig.scripts[index]
		}
	}
	t.Fatalf("no script contains %q among %q", contains, rig.scripts)
	return ""
}

func (rig *volumeRig) lastMount() []volumedrives.Volume {
	rig.mu.Lock()
	defer rig.mu.Unlock()
	if len(rig.mounts) == 0 {
		return nil
	}
	return rig.mounts[len(rig.mounts)-1]
}

func (rig *volumeRig) jailRoot() string {
	return filepath.Join(rig.rt.config.JailerBaseDir, "firecracker", "sandbox-"+shortID(volumeTestSandbox), "root")
}

func (rig *volumeRig) bindLine(vol runnerruntime.Volume, drive int) string {
	return fmt.Sprintf("mount --bind -o ro '%s' '%s/vol%d.img'\n", cachedImage(vol), rig.jailRoot(), drive)
}

func (rig *volumeRig) unmountLine(drive int) string {
	target := fmt.Sprintf("'%s/vol%d.img'", rig.jailRoot(), drive)
	return "[ -L " + target + " ] || umount -l " + target + " 2>/dev/null || true\n"
}

// requireGone checks a failed create left nothing: not tracked, no slot, no
// pins.
func (rig *volumeRig) requireGone(t *testing.T) {
	t.Helper()
	if _, err := rig.rt.GetSandboxInfo(context.Background(), volumeTestSandbox); !errors.Is(err, runnerruntime.ErrSandboxNotFound) {
		t.Fatalf("GetSandboxInfo() error = %v, want the failed create untracked", err)
	}
	if got := capacityOf(t, rig.rt); got.Used != 0 {
		t.Fatalf("capacity = %+v, want the slot released", got)
	}
	if got := rig.cache.pinned(volumeTestSandbox); got != 0 {
		t.Fatalf("pins = %d, want none", got)
	}
}

func indexOf(t *testing.T, events []string, event string) int {
	t.Helper()
	index := slices.Index(events, event)
	if index < 0 {
		t.Fatalf("events %q lack %q", events, event)
	}
	return index
}

func lastIndex(events []string, event string) int {
	for index := len(events) - 1; index >= 0; index-- {
		if events[index] == event {
			return index
		}
	}
	return -1
}

func TestCreateAttachesVolumes(t *testing.T) {
	rig := newVolumeRig(t)
	first, second := testVolume(0, false), testVolume(1, true)

	info, err := rig.rt.CreateSandbox(context.Background(), volumeTestSandbox, volumeOptions(first, second))
	if err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	if len(info.VolumesLeftOut) != 0 {
		t.Fatalf("left out = %v, want none", info.VolumesLeftOut)
	}

	// Acquired before anything exists; bound before jailer starts; patched
	// after the restore; mounted last.
	events := rig.recorded()
	acquired := max(indexOf(t, events, "acquire /mnt/v0"), indexOf(t, events, "acquire /mnt/v1"))
	for _, later := range []string{"clone rootfs", "prepare jail", "start jailer"} {
		if indexOf(t, events, later) < acquired {
			t.Fatalf("events %q: %s before the volumes were acquired", events, later)
		}
	}
	if indexOf(t, events, "prepare jail") > indexOf(t, events, "start jailer") {
		t.Fatalf("events %q: the jail was prepared after jailer started", events)
	}
	load := indexOf(t, events, "load snapshot")
	patch0, patch1 := indexOf(t, events, "patch vol0 /vol0.img"), indexOf(t, events, "patch vol1 /vol1.img")
	if patch0 < load || patch1 < load || indexOf(t, events, "mount request") < max(patch0, patch1) {
		t.Fatalf("events %q: want the restore, then the patches, then the mount request", events)
	}

	script := rig.lastScript(t, "touch ")
	for drive, vol := range []runnerruntime.Volume{first, second} {
		bind := rig.bindLine(vol, drive)
		placed := fmt.Sprintf("mv -fT '%s/.vol%d.img' '%s/vol%d.img'\n", filepath.Dir(rig.jailRoot()), drive, rig.jailRoot(), drive)
		if bindAt, placedAt := strings.Index(script, bind), strings.Index(script, placed); bindAt < 0 || placedAt < 0 || bindAt < placedAt {
			t.Fatalf("jail script does not bind volume %d read-only over its placeholder:\nwant %s after %s\ngot\n%s", drive, bind, placed, script)
		}
	}
	if strings.Contains(script, "chown 1000:1000 '"+rig.jailRoot()+"/vol") || strings.Contains(script, "chown 1000:1000 "+cachedImage(first)) {
		t.Fatalf("jail script chowns a volume image:\n%s", script)
	}

	want := []volumedrives.Volume{{Drive: 0, Sectors: 2048, Path: "/mnt/v0"}, {Drive: 1, Sectors: 4096, Path: "/mnt/v1"}}
	if got := rig.lastMount(); !slices.Equal(got, want) {
		t.Fatalf("mount request = %+v, want %+v", got, want)
	}
	if got := rig.cache.pinned(volumeTestSandbox); got != 2 {
		t.Fatalf("pins = %d, want 2", got)
	}
	if got := rig.rec.VolumeMountCount("success"); got != 1 {
		t.Fatalf("mount requests counted as success = %v, want 1", got)
	}
}

func TestCreateWithoutVolumesIsUnchanged(t *testing.T) {
	rig := newVolumeRig(t)
	info, err := rig.rt.CreateSandbox(context.Background(), volumeTestSandbox, &runnerruntime.CreateOptions{})
	if err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	if info.VolumesLeftOut != nil {
		t.Fatalf("left out = %v, want nil", info.VolumesLeftOut)
	}
	if err := rig.rt.DeleteSandbox(context.Background(), volumeTestSandbox); err != nil {
		t.Fatalf("DeleteSandbox() failed: %v", err)
	}
	for _, event := range rig.recorded() {
		if strings.HasPrefix(event, "acquire") || strings.HasPrefix(event, "patch") || strings.HasPrefix(event, "release") {
			t.Fatalf("events %q include a volume call", rig.recorded())
		}
	}
	for _, script := range rig.scripts {
		if strings.Contains(script, "mount --bind -o ro '/var/cache") || strings.Contains(script, "umount -l '"+rig.jailRoot()+"/vol") {
			t.Fatalf("script binds or unmounts a volume:\n%s", script)
		}
	}
	if got := rig.lastMount(); got != nil {
		t.Fatalf("mount request = %+v, want no drives", got)
	}
}

// A required volume that fails at any stage fails the create, which leaves
// nothing behind: no pins, no slot, and the binds unmounted.
func TestCreateFailsWhenARequiredVolumeFails(t *testing.T) {
	required, optional := testVolume(0, false), testVolume(1, true)
	tests := []struct {
		name     string
		fault    func(*volumeRig)
		wantErr  string
		capacity bool
		jail     bool
	}{
		{"cache full", func(rig *volumeRig) {
			rig.cache.errs[required.SHA256] = fmt.Errorf("volume cache full: %w", runnerruntime.ErrCapacityExhausted)
		}, "volume cache full", true, false},
		{"download", func(rig *volumeRig) {
			rig.cache.errs[required.SHA256] = errors.New("download volume object: status 404")
		}, "status 404", false, false},
		{"image not root-owned", func(rig *volumeRig) { rig.imageErr = errors.New("cached volume image is owned by uid 1000") }, "owned by uid 1000", false, false},
		{"bind", func(rig *volumeRig) {
			rig.runErr = func(script string) error {
				if strings.Contains(script, "mount --bind -o ro '/var/cache") {
					return errors.New("mount: permission denied")
				}
				return nil
			}
		}, "prepare firecracker jail", false, true},
		{"patch", func(rig *volumeRig) { rig.patchErr = errors.New("firecracker API PATCH returned status 400") }, "patch volume drive vol0", false, true},
		{"guest mount", func(rig *volumeRig) { rig.failed = map[int]string{0: volumedrives.ReasonMount} }, "guest did not mount required volume 0", false, true},
		{"mount request", func(rig *volumeRig) { rig.mountErr = &volumeMountError{status: http.StatusConflict} }, "mount guest volume drives", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rig := newVolumeRig(t)
			tt.fault(rig)

			_, err := rig.rt.CreateSandbox(context.Background(), volumeTestSandbox, volumeOptions(required, optional))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("CreateSandbox() error = %v, want one containing %q", err, tt.wantErr)
			}
			if got := errors.Is(err, runnerruntime.ErrCapacityExhausted); got != tt.capacity {
				t.Fatalf("CreateSandbox() error %v is a capacity refusal: %v, want %v", err, got, tt.capacity)
			}
			if strings.Contains(err.Error(), "secret-token") {
				t.Fatalf("CreateSandbox() error %q names the URL", err)
			}
			rig.requireGone(t)
			events := rig.recorded()
			if !tt.jail {
				for _, event := range events {
					if !strings.HasPrefix(event, "acquire") && !strings.HasPrefix(event, "release") {
						t.Fatalf("events %q: the create ran %q before failing on its volumes", events, event)
					}
				}
				return
			}
			cleanup := rig.lastScript(t, "rm -rf")
			for drive := range 2 {
				if !strings.Contains(cleanup, rig.unmountLine(drive)) {
					t.Fatalf("cleanup does not unmount volume %d:\n%s", drive, cleanup)
				}
			}
			if strings.Index(cleanup, rig.unmountLine(1)) > strings.Index(cleanup, "rm -rf") {
				t.Fatalf("cleanup removes the jail before unmounting:\n%s", cleanup)
			}
		})
	}
}

// An optional volume is left out only for a fault of its own, and the sandbox
// runs without it.
func TestCreateLeavesOutAnOptionalVolume(t *testing.T) {
	required, optional := testVolume(0, false), testVolume(1, true)
	tests := []struct {
		name       string
		fault      func(*volumeRig)
		reason     string
		wantMount  []volumedrives.Volume
		boundAfter bool
	}{
		{"cache full", func(rig *volumeRig) {
			rig.cache.errs[optional.SHA256] = fmt.Errorf("volume cache full: %w", runnerruntime.ErrCapacityExhausted)
		}, runnerruntime.VolumeLeftOutCacheFull, []volumedrives.Volume{{Drive: 0, Sectors: 2048, Path: "/mnt/v0"}}, false},
		{"download", func(rig *volumeRig) {
			rig.cache.errs[optional.SHA256] = errors.New("download volume object: sha256 mismatch")
		}, runnerruntime.VolumeLeftOutDownload, []volumedrives.Volume{{Drive: 0, Sectors: 2048, Path: "/mnt/v0"}}, false},
		{"timeout", func(rig *volumeRig) {
			rig.cache.stall[optional.SHA256] = true
			shrinkCreateBudget(rig.t, 400*time.Millisecond, 300*time.Millisecond)
		}, runnerruntime.VolumeLeftOutTimeout, []volumedrives.Volume{{Drive: 0, Sectors: 2048, Path: "/mnt/v0"}}, false},
		{"guest drive", func(rig *volumeRig) { rig.failed = map[int]string{1: volumedrives.ReasonDrive} },
			runnerruntime.VolumeLeftOutDrive, nil, true},
		{"guest mount point", func(rig *volumeRig) { rig.failed = map[int]string{1: volumedrives.ReasonMountPoint} },
			runnerruntime.VolumeLeftOutMountPoint, nil, true},
		{"guest mount", func(rig *volumeRig) { rig.failed = map[int]string{1: volumedrives.ReasonMount} },
			runnerruntime.VolumeLeftOutMount, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rig := newVolumeRig(t)
			tt.fault(rig)

			info, err := rig.rt.CreateSandbox(context.Background(), volumeTestSandbox, volumeOptions(required, optional))
			if err != nil {
				t.Fatalf("CreateSandbox() failed: %v", err)
			}
			if want := map[int]string{1: tt.reason}; !maps.Equal(info.VolumesLeftOut, want) {
				t.Fatalf("left out = %v, want %v", info.VolumesLeftOut, want)
			}
			echo, err := json.Marshal(volumeOptions(required, optional).Applied(info.VolumesLeftOut))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(echo), `"mount_path":"/mnt/v1","optional":true,"attached":false,"reason":"`+tt.reason+`"`) ||
				!strings.Contains(string(echo), `"mount_path":"/mnt/v0","attached":true`) || strings.Contains(string(echo), "secret-token") {
				t.Fatalf("echo = %s, want v0 attached and v1 left out for %s, without URLs", echo, tt.reason)
			}
			if url, err := rig.rt.DaemonURL(context.Background(), volumeTestSandbox); err != nil || url == "" {
				t.Fatalf("DaemonURL() = %q, %v; want a running sandbox", url, err)
			}
			if tt.wantMount != nil && !slices.Equal(rig.lastMount(), tt.wantMount) {
				t.Fatalf("mount request = %+v, want %+v", rig.lastMount(), tt.wantMount)
			}
			// A volume the guest did not mount keeps its bind and pin: its drive
			// size is in the guest, and a wake restores it from the same path.
			script := rig.lastScript(t, "touch ")
			if got := strings.Contains(script, rig.bindLine(optional, 1)); got != tt.boundAfter {
				t.Fatalf("left out volume bound: %v, want %v:\n%s", got, tt.boundAfter, script)
			}
			wantPins := 1
			if tt.boundAfter {
				wantPins = 2
			}
			if got := rig.cache.pinned(volumeTestSandbox); got != wantPins {
				t.Fatalf("pins = %d, want %d", got, wantPins)
			}
			if got := rig.rec.VolumeLeftOutCount(tt.reason); got != 1 {
				t.Fatalf("volumes left out for %s = %v, want 1", tt.reason, got)
			}
		})
	}
}

// shrinkCreateBudget lets a stalled download run out of time quickly, with
// time left for the rest of the create.
func shrinkCreateBudget(t *testing.T, budget, reserve time.Duration) {
	previousBudget, previousReserve := createBudget, volumeAcquireReserve
	createBudget, volumeAcquireReserve = budget, reserve
	t.Cleanup(func() { createBudget, volumeAcquireReserve = previousBudget, previousReserve })
}

// Host faults are not the volume's: they fail the create even for an optional
// one.
func TestCreateFailsOnHostFaultsForOptionalVolumes(t *testing.T) {
	optional := testVolume(0, true)
	for name, breakIt := range map[string]func(*volumeRig){
		"image check": func(rig *volumeRig) { rig.imageErr = errors.New("cached volume image is not a regular file") },
		"bind": func(rig *volumeRig) {
			rig.runErr = func(script string) error {
				if strings.Contains(script, "mount --bind -o ro '/var/cache") {
					return errors.New("mount failed")
				}
				return nil
			}
		},
		"patch": func(rig *volumeRig) { rig.patchErr = errors.New("status 400") },
	} {
		t.Run(name, func(t *testing.T) {
			rig := newVolumeRig(t)
			breakIt(rig)
			if _, err := rig.rt.CreateSandbox(context.Background(), volumeTestSandbox, volumeOptions(optional)); err == nil {
				t.Fatal("CreateSandbox() succeeded, want the host fault to fail it")
			}
			rig.requireGone(t)
		})
	}
}

func TestCreateRefusesVolumesItCannotAttach(t *testing.T) {
	tooMany := make([]runnerruntime.Volume, 9)
	for index := range tooMany {
		tooMany[index] = testVolume(index, true)
	}
	tarVolume := testVolume(0, true)
	tarVolume.Kind = volume.KindTar
	tests := []struct {
		name    string
		opts    *runnerruntime.CreateOptions
		noCache bool
		wantErr string
	}{
		{"more volumes than drives", volumeOptions(tooMany...), false, "the golden snapshot has 8 volume drives, the create asks for 9"},
		{"tar", volumeOptions(tarVolume), false, "Firecracker attaches ext4 images, not tar"},
		{"no cache", volumeOptions(testVolume(0, false)), true, "no volume cache"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rig := newVolumeRig(t)
			if tt.noCache {
				rig.rt.volumeCache = nil
			}
			_, err := rig.rt.CreateSandbox(context.Background(), volumeTestSandbox, tt.opts)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !errors.Is(err, runnerruntime.ErrVolumesUnsupported) {
				t.Fatalf("CreateSandbox() error = %v, want ErrVolumesUnsupported containing %q", err, tt.wantErr)
			}
			rig.requireGone(t)
			if events := rig.recorded(); slices.ContainsFunc(events, func(event string) bool { return !strings.HasPrefix(event, "release") }) {
				t.Fatalf("events %q, want nothing done", events)
			}
		})
	}
}

// A create that fails is not counted or logged as leaving volumes out, even
// for an optional volume decided before the failure.
func TestFailedCreateReportsNoVolumeLeftOut(t *testing.T) {
	for name, fault := range map[string]func(*volumeRig){
		"download, then a required volume the guest did not mount": func(rig *volumeRig) {
			rig.cache.errs[testVolume(0, true).SHA256] = errors.New("download volume object: status 404")
			rig.failed = map[int]string{1: volumedrives.ReasonMount}
		},
		"optional, then required, the guest mounted neither": func(rig *volumeRig) {
			rig.failed = map[int]string{0: volumedrives.ReasonMount, 1: volumedrives.ReasonMount}
		},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newVolumeRig(t)
			fault(rig)
			if _, err := rig.rt.CreateSandbox(context.Background(), volumeTestSandbox, volumeOptions(testVolume(0, true), testVolume(1, false))); err == nil {
				t.Fatal("CreateSandbox() succeeded, want the required volume to fail it")
			}
			for _, reason := range runnerruntime.VolumeLeftOutReasons {
				if got := rig.rec.VolumeLeftOutCount(reason); got != 0 {
					t.Fatalf("volumes left out for %s = %v, want none for a failed create", reason, got)
				}
			}
		})
	}
}

// Shutdown can take a sandbox while its acquires still wait. What they pin
// after that is released too.
func TestShutdownDuringAcquireLeavesNoPins(t *testing.T) {
	rig := newVolumeRig(t)
	rig.cache.gate = make(chan struct{})

	created := make(chan error, 1)
	go func() {
		_, err := rig.rt.CreateSandbox(context.Background(), volumeTestSandbox, volumeOptions(testVolume(0, false)))
		created <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !slices.Contains(rig.recorded(), "acquire /mnt/v0") {
		if time.Now().After(deadline) {
			t.Fatal("the create never started acquiring")
		}
		time.Sleep(time.Millisecond)
	}
	rig.rt.Shutdown(context.Background())
	close(rig.cache.gate)

	if err := <-created; err == nil || !strings.Contains(err.Error(), "torn down while its volumes were acquired") {
		t.Fatalf("CreateSandbox() error = %v, want the create abandoned", err)
	}
	if got := rig.cache.pinned(volumeTestSandbox); got != 0 {
		t.Fatalf("pins = %d, want none after shutdown", got)
	}
	if events := rig.recorded(); slices.Contains(events, "clone rootfs") {
		t.Fatalf("events %q: the abandoned create went on", events)
	}
}

// A second create for a sandbox ID the runner holds fails before it acquires,
// so its failure can't release the live sandbox's pins.
func TestDuplicateCreateKeepsTheSandboxPins(t *testing.T) {
	rig := newVolumeRig(t)
	opts := volumeOptions(testVolume(0, false))
	if _, err := rig.rt.CreateSandbox(context.Background(), volumeTestSandbox, opts); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	if _, err := rig.rt.CreateSandbox(context.Background(), volumeTestSandbox, opts); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second CreateSandbox() error = %v, want already exists", err)
	}
	if got := rig.cache.pinned(volumeTestSandbox); got != 1 {
		t.Fatalf("pins = %d, want the live sandbox's", got)
	}
}

func TestStopAndWakeBindTheSameImagesWithoutAMountRequest(t *testing.T) {
	rig := newVolumeRig(t)
	vol := testVolume(0, false)
	if _, err := rig.rt.CreateSandbox(context.Background(), volumeTestSandbox, volumeOptions(vol)); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	if err := rig.rt.StopSandbox(context.Background(), volumeTestSandbox); err != nil {
		t.Fatalf("StopSandbox() failed: %v", err)
	}
	if cleanup := rig.lastScript(t, "rm -rf"); !strings.Contains(cleanup, rig.unmountLine(0)) {
		t.Fatalf("stop does not unmount the volume:\n%s", cleanup)
	}
	if got := rig.cache.pinned(volumeTestSandbox); got != 1 {
		t.Fatalf("pins after stop = %d, want the sandbox's", got)
	}
	before := len(rig.recorded())

	wake, err := rig.rt.EnsureSandboxRunning(context.Background(), volumeTestSandbox)
	if err != nil || wake.Recovered {
		t.Fatalf("EnsureSandboxRunning() = %+v, %v; want a restore", wake, err)
	}
	woke := rig.recorded()[before:]
	if slices.ContainsFunc(woke, func(event string) bool {
		return event == "mount request" || strings.HasPrefix(event, "patch") || strings.HasPrefix(event, "acquire")
	}) {
		t.Fatalf("wake events %q, want no acquire, patch or mount request", woke)
	}
	if script := rig.lastScript(t, "touch "); !strings.Contains(script, rig.bindLine(vol, 0)) {
		t.Fatalf("wake does not bind the image again:\n%s", script)
	}
}

func TestColdBootBindsAndMountsWhatTheCreateMounted(t *testing.T) {
	rig := newVolumeRig(t)
	required, leftOutInGuest := testVolume(0, false), testVolume(1, true)
	rig.failed = map[int]string{1: volumedrives.ReasonMount}
	exits := &guestExitStub{}
	exits.install(rig.rt, &fakeProcess{})

	if _, err := rig.rt.CreateSandbox(context.Background(), volumeTestSandbox, volumeOptions(required, leftOutInGuest)); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	rig.failed = nil
	exits.fire(t, 0)
	waitForGuestDeathHandled(t, rig)
	if cleanup := rig.lastScript(t, "rm -rf"); !strings.Contains(cleanup, rig.unmountLine(0)) {
		t.Fatalf("guest death does not unmount the volume:\n%s", cleanup)
	}
	if got := rig.cache.pinned(volumeTestSandbox); got != 2 {
		t.Fatalf("pins after guest death = %d, want the sandbox's 2: the cold boot needs them", got)
	}

	recoversByColdBoot(t, rig.rt, volumeTestSandbox)
	script := rig.lastScript(t, "touch ")
	for drive, vol := range []runnerruntime.Volume{required, leftOutInGuest} {
		if !strings.Contains(script, rig.bindLine(vol, drive)) {
			t.Fatalf("cold boot does not bind volume %d:\n%s", drive, script)
		}
	}
	if want := []volumedrives.Volume{{Drive: 0, Sectors: 2048, Path: "/mnt/v0"}}; !slices.Equal(rig.lastMount(), want) {
		t.Fatalf("cold boot mount request = %+v, want %+v", rig.lastMount(), want)
	}
}

func waitForGuestDeathHandled(t *testing.T, rig *volumeRig) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for capacityOf(t, rig.rt).Stopped != 1 {
		if time.Now().After(deadline) {
			t.Fatal("guest death was not handled")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestColdBootFailsWhenTheGuestLosesARequiredVolume(t *testing.T) {
	rig := newVolumeRig(t)
	exits := &guestExitStub{}
	exits.install(rig.rt, &fakeProcess{})
	if _, err := rig.rt.CreateSandbox(context.Background(), volumeTestSandbox, volumeOptions(testVolume(0, false))); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	exits.fire(t, 0)
	waitForGuestDeathHandled(t, rig)

	rig.failed = map[int]string{0: volumedrives.ReasonDrive}
	rig.rt.deps.coldBoot = func(context.Context, string, *bootParams) error { return nil }
	if _, err := rig.rt.EnsureSandboxRunning(context.Background(), volumeTestSandbox); err == nil || !strings.Contains(err.Error(), "required volume 0") {
		t.Fatalf("EnsureSandboxRunning() error = %v, want the required volume's failure", err)
	}
	if got := capacityOf(t, rig.rt); got.Used != 0 || got.Stopped != 1 {
		t.Fatalf("capacity = %+v, want the sandbox stopped", got)
	}
	if got := rig.cache.pinned(volumeTestSandbox); got != 1 {
		t.Fatalf("pins = %d, want the stopped sandbox's", got)
	}
}

func TestDeleteAndShutdownUnmountAndRelease(t *testing.T) {
	for name, end := range map[string]func(*Runtime) error{
		"delete": func(rt *Runtime) error { return rt.DeleteSandbox(context.Background(), volumeTestSandbox) },
		"delete stopped": func(rt *Runtime) error {
			if err := rt.StopSandbox(context.Background(), volumeTestSandbox); err != nil {
				return err
			}
			return rt.DeleteSandbox(context.Background(), volumeTestSandbox)
		},
		"shutdown": func(rt *Runtime) error { rt.Shutdown(context.Background()); return nil },
	} {
		t.Run(name, func(t *testing.T) {
			rig := newVolumeRig(t)
			if _, err := rig.rt.CreateSandbox(context.Background(), volumeTestSandbox, volumeOptions(testVolume(0, false))); err != nil {
				t.Fatalf("CreateSandbox() failed: %v", err)
			}
			if err := end(rig.rt); err != nil {
				t.Fatalf("ending the sandbox failed: %v", err)
			}
			if cleanup := rig.lastScript(t, "rm -rf"); !strings.Contains(cleanup, rig.unmountLine(0)) {
				t.Fatalf("cleanup does not unmount the volume:\n%s", cleanup)
			}
			events := rig.recorded()
			if released := indexOf(t, events, "release "+volumeTestSandbox); released < lastIndex(events, "cleanup host") {
				t.Fatalf("events %q: pins released before the binds were unmounted", events)
			}
			rig.requireGone(t)
		})
	}
}

// A delete whose cleanup fails keeps the sandbox, and with it the pins of
// images that may still be bound.
func TestFailedDeleteKeepsThePins(t *testing.T) {
	rig := newVolumeRig(t)
	if _, err := rig.rt.CreateSandbox(context.Background(), volumeTestSandbox, volumeOptions(testVolume(0, false))); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	rig.runErr = func(script string) error {
		if strings.Contains(script, "rm -rf") {
			return errors.New("rm: cannot remove: Device or resource busy")
		}
		return nil
	}
	if err := rig.rt.DeleteSandbox(context.Background(), volumeTestSandbox); err == nil {
		t.Fatal("DeleteSandbox() succeeded, want the cleanup failure")
	}
	if got := rig.cache.pinned(volumeTestSandbox); got != 1 {
		t.Fatalf("pins = %d, want them kept for the delete's retry", got)
	}
}

// Older daemons answer a request with drives 204 when all mounted and 500 when
// none did; anything else, or an answer that doesn't fit the request, fails the
// create.
func TestCreateWithDaemonAnswers(t *testing.T) {
	required, optional := testVolume(0, false), testVolume(1, true)
	tests := []struct {
		name        string
		volumes     []runnerruntime.Volume
		status      int
		answer      string
		wantLeftOut map[int]string
		wantErr     string
	}{
		{"older daemon mounted all", []runnerruntime.Volume{required, optional}, http.StatusNoContent, "", map[int]string{}, ""},
		{"older daemon mounted none of the optional", []runnerruntime.Volume{optional}, http.StatusInternalServerError,
			`{"error":"bad superblock","code":500,"reason":"mount"}`, map[int]string{0: "mount"}, ""},
		{"older daemon mounted none", []runnerruntime.Volume{required, optional}, http.StatusInternalServerError,
			`{"error":"bad superblock","code":500,"reason":"mount"}`, nil, "guest did not mount required volume 0"},
		{"unknown drive", []runnerruntime.Volume{required, optional}, http.StatusOK,
			`{"volumes":[{"drive":0,"mounted":true},{"drive":7,"mounted":true}]}`, nil, "invalid answer"},
		{"unknown reason", []runnerruntime.Volume{optional}, http.StatusOK,
			`{"volumes":[{"drive":0,"mounted":false,"reason":"$(reboot)"}]}`, nil, "invalid answer"},
		{"drive twice", []runnerruntime.Volume{required, optional}, http.StatusOK,
			`{"volumes":[{"drive":0,"mounted":true},{"drive":0,"mounted":true}]}`, nil, "invalid answer"},
		{"oversized", []runnerruntime.Volume{optional}, http.StatusOK,
			`{"volumes":[{"drive":0,"mounted":true}]}` + strings.Repeat(" ", volumedrives.MaxAnswerBytes), nil, "invalid answer"},
		{"not found", []runnerruntime.Volume{optional}, http.StatusNotFound, "404 page not found", nil, "daemon answered 404"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.answer))
			}))
			defer daemon.Close()
			rig := newVolumeRig(t)
			rig.rt.deps.mountVolumeDrives = func(ctx context.Context, _ string, drives []volumedrives.Volume) (map[int]string, error) {
				return mountVolumeDrives(ctx, daemon.URL, drives)
			}

			info, err := rig.rt.CreateSandbox(context.Background(), volumeTestSandbox, volumeOptions(tt.volumes...))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("CreateSandbox() error = %v, want one containing %q", err, tt.wantErr)
				}
				rig.requireGone(t)
				return
			}
			if err != nil {
				t.Fatalf("CreateSandbox() failed: %v", err)
			}
			if !maps.Equal(info.VolumesLeftOut, tt.wantLeftOut) {
				t.Fatalf("left out = %v, want %v", info.VolumesLeftOut, tt.wantLeftOut)
			}
		})
	}
}

func TestVolumeImageAccess(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "image")
	if err := os.WriteFile(file, nil, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(file, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{file, filepath.Join(dir, "link"), dir, filepath.Join(dir, "missing")} {
		err := checkVolumeImage(path)
		if os.Getuid() == 0 && path == file {
			if err != nil {
				t.Fatalf("checkVolumeImage(%s) error = %v, want none for a root-owned 0444 file", path, err)
			}
			continue
		}
		if err == nil {
			t.Fatalf("checkVolumeImage(%s) succeeded, want it refused", path)
		}
		if strings.Contains(err.Error(), dir) {
			t.Fatalf("checkVolumeImage() error %q names the host path", err)
		}
	}
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkVolumeImage(file); err == nil || !strings.Contains(err.Error(), "writable by no one") {
		t.Fatalf("checkVolumeImage() of a writable file error = %v, want it refused", err)
	}
}

func TestAcquireWait(t *testing.T) {
	for _, tt := range []struct {
		remaining, want time.Duration
	}{
		{3 * time.Minute, 2 * time.Minute},
		{time.Minute, 30 * time.Second},
		{20 * time.Second, 10 * time.Second},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), tt.remaining)
		got := acquireWait(ctx)
		cancel()
		if got > tt.want || got < tt.want-time.Second {
			t.Errorf("acquireWait(%s left) = %s, want about %s", tt.remaining, got, tt.want)
		}
	}
}
