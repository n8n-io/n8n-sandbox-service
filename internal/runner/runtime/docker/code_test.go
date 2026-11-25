package docker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/runner/config"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

func codeRuntime(t *testing.T, enabled bool) (*Runtime, *fakeDockerBackend, *[]string) {
	t.Helper()
	events := []string{}
	backend := &fakeDockerBackend{events: &events, containerID: "container-1", ip: "172.19.0.2"}
	runnerConfig := &config.Config{CodeSandboxesEnabled: enabled, CodeCPUPercent: 50, CodeMaxMemoryMB: 2048}
	m := newRuntime(runnerConfig, Config{SandboxImage: "sandbox-image", CodeImage: "code-image", DefaultMemoryMB: 512, DefaultCPUPercent: 100, DefaultPidsMax: 256}, backend)
	m.imageReady.Store(true)
	m.codeImageReady.Store(true)
	m.applyPolicy = func(string, string, string, string, int, bool) error { return nil }
	m.teardownRules = func(string) error { return nil }
	m.waitForDaemon = func(context.Context, string) error {
		events = append(events, "waitForDaemon")
		return nil
	}
	m.waitForCodeDaemon = func(context.Context, string) error {
		events = append(events, "waitForCodeDaemon")
		return nil
	}
	return m, backend, &events
}

func codeOptions() *runnerruntime.CreateOptions {
	return &runnerruntime.CreateOptions{Egress: runnerruntime.EgressNone, CodeOpts: &runnerruntime.CodeOptions{MemoryMB: 768}}
}

func TestCreateCodeSandboxUsesCodeImageAndLimits(t *testing.T) {
	m, backend, events := codeRuntime(t, true)

	if _, err := m.CreateSandbox(context.Background(), "sandbox-id-1234", codeOptions()); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	if backend.image != "code-image" || backend.kind != sandboxKindCode {
		t.Fatalf("created image = %q, kind = %q, want code-image, code", backend.image, backend.kind)
	}
	want := ResourceLimits{MemoryMB: 768, CPUPercent: 50, PidsMax: 256}
	if *backend.limits != want {
		t.Fatalf("limits = %+v, want %+v", *backend.limits, want)
	}
	if backend.network != runnerNoEgressNetwork {
		t.Fatalf("network = %q, want %q", backend.network, runnerNoEgressNetwork)
	}
	if _, err := m.ensureSandboxRunningOnce(context.Background(), "sandbox-id-1234"); err != nil {
		t.Fatalf("ensureSandboxRunningOnce() failed: %v", err)
	}
	var waits []string
	for _, event := range *events {
		if event == "waitForDaemon" || event == "waitForCodeDaemon" {
			waits = append(waits, event)
		}
	}
	if want := []string{"waitForCodeDaemon", "waitForCodeDaemon"}; !reflect.DeepEqual(waits, want) {
		t.Fatalf("readiness checks = %v, want %v", waits, want)
	}
}

func TestCreateGeneralSandboxIgnoresCodeSettings(t *testing.T) {
	m, backend, _ := codeRuntime(t, true)

	if _, err := m.CreateSandbox(context.Background(), "sandbox-id-1234", nil); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	want := ResourceLimits{MemoryMB: 512, CPUPercent: 100, PidsMax: 256}
	if backend.image != "sandbox-image" || backend.kind != sandboxKindGeneric || *backend.limits != want {
		t.Fatalf("created image = %q, kind = %q, limits = %+v; want sandbox-image, generic, %+v", backend.image, backend.kind, *backend.limits, want)
	}
}

func TestCreateCodeSandboxRefusedBeforeCreate(t *testing.T) {
	for _, tc := range []struct {
		name           string
		enabled        bool
		codeImageReady bool
		want           error
	}{
		{"disabled", false, true, runnerruntime.ErrCodeSandboxesUnsupported},
		{"code image not ready", true, false, runnerruntime.ErrCodeImageNotReady},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, backend, _ := codeRuntime(t, tc.enabled)
			m.codeImageReady.Store(tc.codeImageReady)

			if _, err := m.CreateSandbox(context.Background(), "sandbox-id-1234", codeOptions()); !errors.Is(err, tc.want) {
				t.Fatalf("CreateSandbox() error = %v, want %v", err, tc.want)
			}
			if backend.isCreated() {
				t.Fatal("a container was created")
			}
		})
	}
}

func TestNewRequiresCodeImageWhenEnabled(t *testing.T) {
	_, err := New(&config.Config{CodeSandboxesEnabled: true}, Config{SandboxImage: "sandbox-image"})
	if err == nil {
		t.Fatal("New() succeeded without a code image")
	}
}

func TestDockerContainerCreateArgsLabelTheKind(t *testing.T) {
	for _, kind := range []sandboxKind{sandboxKindGeneric, sandboxKindCode} {
		args := dockerContainerCreateArgs("sandbox-id", "sandbox-name", "image", runnerBridgeNetwork, nil, false, kind, nil, false, nil)
		if label := containerLabelKind + "=" + string(kind); !slices.Contains(args, label) {
			t.Fatalf("%s sandbox args %v do not have the label %s", kind, args, label)
		}
	}
}

// A container from before the kind label is generic.
func TestKindOf(t *testing.T) {
	for _, tc := range []struct {
		labels map[string]string
		want   sandboxKind
	}{
		{nil, sandboxKindGeneric},
		{map[string]string{containerLabelKind: "generic"}, sandboxKindGeneric},
		{map[string]string{containerLabelKind: "code"}, sandboxKindCode},
		{map[string]string{containerLabelKind: "unknown"}, sandboxKindGeneric},
	} {
		inspect := &containerInspect{}
		inspect.Config.Labels = tc.labels
		if got := kindOf(inspect); got != tc.want {
			t.Errorf("kindOf(%v) = %q, want %q", tc.labels, got, tc.want)
		}
	}
}

// pullRecorder records the images pulled and never sees a container death.
type pullRecorder struct {
	*fakeDockerBackend
	mu     sync.Mutex
	pulled []string
}

func (p *pullRecorder) pullImage(_ context.Context, image string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pulled = append(p.pulled, image)
	return nil
}

func (p *pullRecorder) watchContainerDeaths(ctx context.Context, _ func(string, string)) error {
	<-ctx.Done()
	return ctx.Err()
}

func (p *pullRecorder) images() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.pulled)
}

func TestPreparePullsCodeImageOnlyWhenEnabled(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		backend := &pullRecorder{fakeDockerBackend: &fakeDockerBackend{events: &[]string{}}}
		m := newRuntime(&config.Config{CodeSandboxesEnabled: enabled}, Config{SandboxImage: "sandbox-image", CodeImage: "code-image"}, backend)
		ctx, cancel := context.WithCancel(context.Background())
		m.Prepare(ctx)
		if enabled {
			deadline := time.Now().Add(5 * time.Second)
			for !m.codeImageReady.Load() && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
		}
		cancel()

		want := []string{"sandbox-image"}
		if enabled {
			want = append(want, "code-image")
		}
		got := backend.images()
		slices.Sort(got)
		slices.Sort(want)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("enabled = %v: pulled %v, want %v", enabled, got, want)
		}
		if m.codeImageReady.Load() != enabled {
			t.Fatalf("enabled = %v: code image ready = %v", enabled, m.codeImageReady.Load())
		}
	}
}

func TestWaitForCodeDaemonChecksHealthOnly(t *testing.T) {
	var paths []string
	var mu sync.Mutex
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer daemon.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := waitForCodeDaemon(ctx, daemon.URL); err != nil {
		t.Fatalf("waitForCodeDaemon() failed: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(paths, []string{"/healthz"}) {
		t.Fatalf("requests = %v, want one /healthz", paths)
	}
}
