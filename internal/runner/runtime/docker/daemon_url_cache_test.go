package docker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/runner/config"
)

func liveCache() *daemonURLCache {
	c := newDaemonURLCache()
	c.setLive(true)
	return c
}

func TestDaemonURLCacheStoresAndReturnsAURL(t *testing.T) {
	c := liveCache()
	c.store("sb", "c1", "http://10.0.0.2:8081", c.token("sb"))

	if url, ok := c.get("sb"); !ok || url != "http://10.0.0.2:8081" {
		t.Fatalf("get() = %q, %v; want the stored URL", url, ok)
	}
}

// Without the death watcher, the runner does not learn of a container that died,
// so nothing may be served from the cache or stored in it.
func TestDaemonURLCacheIsUnusedWhileTheWatcherIsDown(t *testing.T) {
	c := newDaemonURLCache()
	c.store("sb", "c1", "http://10.0.0.2:8081", c.token("sb"))
	if _, ok := c.get("sb"); ok {
		t.Fatal("stored a URL with no death watcher")
	}

	c = liveCache()
	c.store("sb", "c1", "http://10.0.0.2:8081", c.token("sb"))
	c.setLive(false)
	if _, ok := c.get("sb"); ok {
		t.Fatal("served a URL after the death watcher disconnected")
	}
	c.setLive(true)
	if _, ok := c.get("sb"); ok {
		t.Fatal("served a URL from before the reconnect; a death in the gap was not seen")
	}
}

// A lookup reads Docker, and the container can stop before the lookup stores
// what it read. The forget that came with the stop must win.
func TestDaemonURLCacheIgnoresALookupThatAForgetOvertook(t *testing.T) {
	c := liveCache()
	tok := c.token("sb")
	c.forget("sb")
	c.store("sb", "c1", "http://10.0.0.2:8081", tok)

	if _, ok := c.get("sb"); ok {
		t.Fatal("stored the result of a lookup that started before the forget")
	}

	// A lookup that starts after the forget stores again.
	c.store("sb", "c1", "http://10.0.0.2:8081", c.token("sb"))
	if _, ok := c.get("sb"); !ok {
		t.Fatal("a lookup after the forget did not store")
	}
}

func TestDaemonURLCacheIgnoresALookupThatSpansAReconnect(t *testing.T) {
	c := liveCache()
	tok := c.token("sb")
	c.setLive(false)
	c.setLive(true)
	c.store("sb", "c1", "http://10.0.0.2:8081", tok)

	if _, ok := c.get("sb"); ok {
		t.Fatal("stored the result of a lookup that started before the reconnect")
	}
}

func TestDaemonURLCacheForgetsByContainer(t *testing.T) {
	c := liveCache()
	c.store("sb1", "c1", "http://10.0.0.2:8081", c.token("sb1"))
	c.store("sb2", "c2", "http://10.0.0.3:8081", c.token("sb2"))

	c.forgetContainer("c1")

	if _, ok := c.get("sb1"); ok {
		t.Error("sb1 still cached after its container was forgotten")
	}
	if _, ok := c.get("sb2"); !ok {
		t.Error("sb2 was forgotten with another sandbox's container")
	}
}

func TestDaemonURLCacheSweepsOldTombstones(t *testing.T) {
	c := liveCache()
	c.forget("old")
	e := c.entries["old"]
	e.forgotAt = time.Now().Add(-daemonURLTombstoneTTL - time.Second)
	c.entries["old"] = e

	c.forget("new")

	if _, ok := c.entries["old"]; ok {
		t.Error("an expired tombstone was not swept")
	}
	if _, ok := c.entries["new"]; !ok {
		t.Error("the new tombstone is missing")
	}
}

// newCachingRuntime is a crash runtime with the death watcher marked as live,
// which is the state a runner serves in.
func newCachingRuntime(t *testing.T, backend *crashBackend) *Runtime {
	t.Helper()
	m, _ := newCrashRuntime(t, backend)
	m.daemonURLs.setLive(true)
	return m
}

func (f *crashBackend) inspectCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inspects
}

func TestDaemonURLAsksDockerOnceWhileTheWatcherIsLive(t *testing.T) {
	backend := &crashBackend{states: []containerState{runningState()}, ip: "172.18.0.2"}
	m := newCachingRuntime(t, backend)

	for range 3 {
		url, err := m.DaemonURL(context.Background(), crashSandboxID)
		if err != nil {
			t.Fatalf("DaemonURL() failed: %v", err)
		}
		if want := "http://172.18.0.2:8081"; url != want {
			t.Fatalf("DaemonURL() = %q, want %q", url, want)
		}
	}
	if got := backend.inspectCount(); got != 1 {
		t.Errorf("inspects = %d, want 1", got)
	}
}

func TestDaemonURLAsksDockerEveryTimeWithoutTheWatcher(t *testing.T) {
	backend := &crashBackend{states: []containerState{runningState()}, ip: "172.18.0.2"}
	m, _ := newCrashRuntime(t, backend)

	for range 2 {
		if _, err := m.DaemonURL(context.Background(), crashSandboxID); err != nil {
			t.Fatalf("DaemonURL() failed: %v", err)
		}
	}
	if got := backend.inspectCount(); got != 2 {
		t.Errorf("inspects = %d, want 2", got)
	}
}

// Every death ends the cached URL, also one the runner asked for: the container
// comes back, if at all, on an address the cache does not know.
func TestADeathEndsTheCachedDaemonURL(t *testing.T) {
	backend := &crashBackend{states: []containerState{runningState()}, ip: "172.18.0.2"}
	m := newCachingRuntime(t, backend)
	if _, err := m.DaemonURL(context.Background(), crashSandboxID); err != nil {
		t.Fatalf("DaemonURL() failed: %v", err)
	}

	m.expectStop(crashContainerID)
	m.handleContainerDeath(crashContainerID, crashSandboxID)
	backend.ip = "172.18.0.9"

	url, err := m.DaemonURL(context.Background(), crashSandboxID)
	if err != nil {
		t.Fatalf("DaemonURL() after the death failed: %v", err)
	}
	if want := "http://172.18.0.9:8081"; url != want {
		t.Errorf("DaemonURL() = %q, want %q: the cache served the address from before the death", url, want)
	}
}

// A death event without the sandbox label still names the container.
func TestADeathWithoutASandboxLabelEndsTheCachedDaemonURL(t *testing.T) {
	backend := &crashBackend{states: []containerState{runningState()}, ip: "172.18.0.2"}
	m := newCachingRuntime(t, backend)
	if _, err := m.DaemonURL(context.Background(), crashSandboxID); err != nil {
		t.Fatalf("DaemonURL() failed: %v", err)
	}

	m.handleContainerDeath(crashContainerID, "")

	if _, ok := m.daemonURLs.get(crashSandboxID); ok {
		t.Error("the URL is still cached after its container died")
	}
}

func TestAStopEndsTheCachedDaemonURL(t *testing.T) {
	backend := &crashBackend{
		states: []containerState{
			runningState(), // DaemonURL
			runningState(), // StopSandboxContainer
			{Status: containerStatusExited},
		},
		ip: "172.18.0.2",
	}
	m := newCachingRuntime(t, backend)
	if _, err := m.DaemonURL(context.Background(), crashSandboxID); err != nil {
		t.Fatalf("DaemonURL() failed: %v", err)
	}

	if err := m.StopSandboxContainer(context.Background(), crashSandboxID); err != nil {
		t.Fatalf("StopSandboxContainer() failed: %v", err)
	}

	if _, err := m.DaemonURL(context.Background(), crashSandboxID); !errors.Is(err, ErrSandboxNotRunning) {
		t.Errorf("DaemonURL() error = %v, want %v: a stopped sandbox was served from the cache", err, ErrSandboxNotRunning)
	}
}

func TestADeleteEndsTheCachedDaemonURL(t *testing.T) {
	backend := &crashBackend{states: []containerState{runningState()}, ip: "172.18.0.2"}
	m := newCachingRuntime(t, backend)
	if _, err := m.DaemonURL(context.Background(), crashSandboxID); err != nil {
		t.Fatalf("DaemonURL() failed: %v", err)
	}

	if err := m.DeleteSandbox(context.Background(), crashSandboxID); err != nil {
		t.Fatalf("DeleteSandbox() failed: %v", err)
	}

	if _, ok := m.daemonURLs.get(crashSandboxID); ok {
		t.Error("the URL is still cached after the delete")
	}
}

func TestForgetDaemonURLEndsTheCachedDaemonURL(t *testing.T) {
	backend := &crashBackend{states: []containerState{runningState()}, ip: "172.18.0.2"}
	m := newCachingRuntime(t, backend)
	if _, err := m.DaemonURL(context.Background(), crashSandboxID); err != nil {
		t.Fatalf("DaemonURL() failed: %v", err)
	}

	m.ForgetDaemonURL(crashSandboxID)

	if _, ok := m.daemonURLs.get(crashSandboxID); ok {
		t.Error("the URL is still cached after ForgetDaemonURL")
	}
}

// The wake path has just read the container's address and applied the network
// rules for it, so the first request after a recovery needs no lookup.
func TestARecoveryCachesTheNewDaemonURL(t *testing.T) {
	backend := &crashBackend{states: []containerState{runningState()}, ip: "172.18.0.2"}
	m := newCachingRuntime(t, backend)

	m.handleContainerDeath(crashContainerID, crashSandboxID)
	backend.ip = "172.18.0.7"
	if _, err := m.EnsureSandboxRunning(context.Background(), crashSandboxID); err != nil {
		t.Fatalf("EnsureSandboxRunning() failed: %v", err)
	}
	before := backend.inspectCount()

	url, err := m.DaemonURL(context.Background(), crashSandboxID)
	if err != nil {
		t.Fatalf("DaemonURL() failed: %v", err)
	}
	if want := "http://172.18.0.7:8081"; url != want {
		t.Errorf("DaemonURL() = %q, want %q", url, want)
	}
	if got := backend.inspectCount(); got != before {
		t.Errorf("inspects after the wake = %d, want %d", got, before)
	}
}

func TestACreateCachesTheDaemonURL(t *testing.T) {
	events := []string{}
	backend := &fakeDockerBackend{events: &events, containerID: "container-1", ip: "172.18.0.2"}
	m := newRuntime(&config.Config{}, Config{}, backend)
	m.daemonURLs.setLive(true)
	m.imageReady.Store(true)
	m.applyPolicy = func(string, string, string, string, int, bool) error { return nil }
	m.teardownRules = func(string) error { return nil }
	m.waitForDaemon = func(context.Context, string) error { return nil }

	if _, err := m.CreateContainer(context.Background(), "sandbox-id-1234", nil); err != nil {
		t.Fatalf("CreateContainer() failed: %v", err)
	}
	events = events[:0]

	url, err := m.DaemonURL(context.Background(), "sandbox-id-1234")
	if err != nil {
		t.Fatalf("DaemonURL() failed: %v", err)
	}
	if want := "http://172.18.0.2:8081"; url != want {
		t.Errorf("DaemonURL() = %q, want %q", url, want)
	}
	if len(events) != 0 {
		t.Errorf("DaemonURL() after create called docker: %v", events)
	}
}

// The cache is used only while the death watcher runs, and a reconnect starts
// it empty.
func TestTheDeathWatcherControlsTheDaemonURLCache(t *testing.T) {
	var m *Runtime
	liveDuringWatch := make(chan bool, 1)
	ctx, cancel := context.WithCancel(context.Background())
	backend := &crashBackend{
		states: []containerState{runningState()},
		watch: func(context.Context, func(string, string)) error {
			m.daemonURLs.mu.Lock()
			liveDuringWatch <- m.daemonURLs.live
			m.daemonURLs.mu.Unlock()
			cancel()
			return errors.New("stream broke")
		},
	}
	m, _ = newCrashRuntime(t, backend)

	m.watchGuestDeaths(ctx)

	if !<-liveDuringWatch {
		t.Error("the cache was not live while the watcher ran")
	}
	if m.daemonURLs.live {
		t.Error("the cache is still live after the watcher stopped")
	}
}
