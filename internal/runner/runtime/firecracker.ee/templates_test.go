package firecracker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

func pythonTemplate() templateAssets {
	return templateAssets{
		TemplateDir:       "/srv/firecracker/templates/python",
		SnapshotMemPath:   "/srv/firecracker/templates/python/snapshot/mem",
		SnapshotStatePath: "/srv/firecracker/templates/python/snapshot/state",
	}
}

// A sandbox of a named template clones that template's rootfs and snapshot,
// is reserved against its boot parameters and kernel, and binds its kernel.
func TestCreateSandboxUsesItsTemplate(t *testing.T) {
	rt := testRuntime(2)
	rt.config.Templates = map[string]templateAssets{"python": pythonTemplate()}
	var cloned, clonedMem, bootPath, kernelStat string
	var scripts []string
	rt.deps.run = func(_ context.Context, _ string, args ...string) error {
		scripts = append(scripts, args[len(args)-1])
		return nil
	}
	rt.deps.start = func(context.Context, func(error), string, ...string) (process, error) { return &fakeProcess{}, nil }
	rt.deps.pathExists = func(string) bool { return true }
	rt.deps.cloneRootfs = func(_ context.Context, template, _ string) error { cloned = template; return nil }
	rt.deps.cloneGoldenSnapshot = func(_ context.Context, mem, _, _ string) error { clonedMem = mem; return nil }
	inner := rt.deps.loadBootParams
	rt.deps.loadBootParams = func(path string) (*bootParams, error) { bootPath = path; return inner(path) }
	rt.deps.statTemplateKernel = func(path string) (kernelPin, error) { kernelStat = path; return testKernelPin, nil }
	rt.deps.loadSnapshot = func(context.Context, string, Config) error { return nil }
	rt.deps.newProxy = func(context.Context, string, string, string, string) (daemonProxy, error) { return &fakeProxy{}, nil }
	rt.deps.probeDaemon = func(context.Context, string) error { return nil }

	if _, err := rt.CreateSandbox(context.Background(), "sandbox-id-123456", &runnerruntime.CreateOptions{Template: "python"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	py := pythonTemplate()
	if cloned != py.rootfsPath() || clonedMem != py.SnapshotMemPath || bootPath != py.bootParamsPath() || kernelStat != py.kernelPath() {
		t.Fatalf("rootfs %q mem %q boot %q kernel %q", cloned, clonedMem, bootPath, kernelStat)
	}
	if !strings.Contains(scripts[0], py.kernelPath()) {
		t.Fatalf("prepare jail script %q does not bind the template's kernel", scripts[0])
	}
	rt.mu.Lock()
	got := rt.sandboxes["sandbox-id-123456"].template.Name
	rt.mu.Unlock()
	if got != "python" {
		t.Fatalf("sandbox template = %q", got)
	}

	if _, err := rt.CreateSandbox(context.Background(), "sandbox-id-654321", &runnerruntime.CreateOptions{Template: "go"}); err == nil {
		t.Fatal("unknown template created")
	}
}

// Admission covers every template, and records what its sandboxes get.
func TestPinSnapshotAssetsRecordsEveryTemplate(t *testing.T) {
	rt := testRuntime(1)
	dir := useTestGoldenSnapshotDir(t, rt)
	pyDir := filepath.Join(t.TempDir(), "python")
	if err := os.MkdirAll(filepath.Join(pyDir, "snapshot"), 0o755); err != nil {
		t.Fatal(err)
	}
	py := templateAssets{TemplateDir: pyDir, SnapshotMemPath: filepath.Join(pyDir, "snapshot", "mem"), SnapshotStatePath: filepath.Join(pyDir, "snapshot", "state")}
	params := testBootParams(rt.config)
	params.VCPUCount, params.MemSizeMiB = 4, 8192
	writeBootParams(t, py.bootParamsPath(), params)
	rt.config.Templates = map[string]templateAssets{"python": py}
	rt.deps.pathExists = func(string) bool { return true }
	rt.deps.fileSize = func(path string) (int64, error) {
		if path == py.rootfsPath() {
			return 20 << 30, nil
		}
		return 2 << 30, nil
	}
	_ = dir

	if got := rt.Templates(); len(got) != 2 || got[1].Name != "python" || got[1].MemoryMB != 0 {
		t.Fatalf("before admission: %+v", got)
	}
	if err := rt.pinSnapshotAssets(); err != nil {
		t.Fatalf("pinSnapshotAssets: %v", err)
	}
	got := rt.Templates()
	for i := range got {
		if got[i].Version == "" {
			t.Fatalf("template %q has no version", got[i].Name)
		}
		got[i].Version = ""
	}
	want := []runnerruntime.TemplateInfo{
		{Name: "default", VCPUs: 1, MemoryMB: 512, DiskMB: 2048},
		{Name: "python", VCPUs: 4, MemoryMB: 8192, DiskMB: 20480},
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("templates = %+v, want %+v", got, want)
	}

	// A template whose sidecar disagrees with the runner fails admission.
	params.GuestIP = "172.16.0.99"
	writeBootParams(t, py.bootParamsPath(), params)
	if err := rt.pinSnapshotAssets(); err == nil || !strings.Contains(err.Error(), `"python"`) {
		t.Fatalf("mismatched template: %v", err)
	}
}

// Each template's canary boots that template; admission runs them default
// first, then by name.
func TestAdmissionCanaryBootsItsTemplate(t *testing.T) {
	rt := testRuntime(1)
	rt.config.Templates = map[string]templateAssets{"python": pythonTemplate(), "go": {TemplateDir: "/t/go", SnapshotMemPath: "/t/go/s/mem", SnapshotStatePath: "/t/go/s/state"}}
	var cloned string
	rt.deps.cloneRootfs = func(_ context.Context, template, _ string) error {
		cloned = template
		return context.Canceled // stop once the template is chosen
	}
	rt.deps.run = func(context.Context, string, ...string) error { return nil }
	if err := rt.runTemplateCanary(context.Background(), "python"); err == nil || cloned != pythonTemplate().rootfsPath() {
		t.Fatalf("python canary: err=%v cloned=%q", err, cloned)
	}
	if err := rt.runAdmissionCanary(context.Background()); err == nil || cloned != "/srv/firecracker/template/rootfs.ext4" {
		t.Fatalf("admission starts with the default: err=%v cloned=%q", err, cloned)
	}
	var names []string
	for _, a := range rt.config.templateAssetsList() {
		names = append(names, a.Name)
	}
	if strings.Join(names, ",") != "default,go,python" {
		t.Fatalf("admission order = %v", names)
	}
}

func TestLoadConfigParsesTemplates(t *testing.T) {

	t.Setenv("SANDBOX_RUNNER_FIRECRACKER_TEMPLATES", `{"python":{"template_dir":"/t/py","snapshot_mem_path":"/t/py/s/mem","snapshot_state_path":"/t/py/s/state"}}`)
	cfg, err := LoadConfig(2)
	if err != nil {
		t.Fatal(err)
	}
	if a, ok := cfg.templateAssetsFor("python"); !ok || a.TemplateDir != "/t/py" || a.Name != "python" {
		t.Fatalf("python = %+v", a)
	}
	for _, bad := range []string{
		`{"default":{"template_dir":"/a","snapshot_mem_path":"/a/m","snapshot_state_path":"/a/s"}}`,
		`{"Py":{"template_dir":"/a","snapshot_mem_path":"/a/m","snapshot_state_path":"/a/s"}}`,
		`{"py":{"template_dir":"a","snapshot_mem_path":"/a/m","snapshot_state_path":"/a/s"}}`,
		`{"py":{"template_dir":"/a","snapshot_mem_path":"/a/m","snapshot_state_path":"/a/s","vcpus":2}}`,
		// Shares the default's snapshot directory.
		`{"py":{"template_dir":"/a","snapshot_mem_path":"/srv/firecracker/snapshots/m","snapshot_state_path":"/srv/firecracker/snapshots/s"}}`,
	} {
		t.Setenv("SANDBOX_RUNNER_FIRECRACKER_TEMPLATES", bad)
		if _, err := LoadConfig(2); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
