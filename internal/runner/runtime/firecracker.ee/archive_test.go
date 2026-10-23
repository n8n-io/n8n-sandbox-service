package firecracker

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// A restore writes the archive as the rootfs, never clones the template or
// restores a memory snapshot, and cold boots; the export of a stopped sandbox
// gives the rootfs back.
func TestImportColdBootsTheArchive(t *testing.T) {
	rt := testRuntimeT(t, 1)
	rt.deps.run = func(context.Context, string, ...string) error { return nil }
	rt.deps.start = func(context.Context, func(error), string, ...string) (process, error) { return &fakeProcess{}, nil }
	rt.deps.pathExists = func(string) bool { return true }
	rt.deps.cloneRootfs = func(context.Context, string, string) error { t.Fatal("cloned the template"); return nil }
	rt.deps.cloneGoldenSnapshot = func(context.Context, string, string, string) error { t.Fatal("cloned the snapshot"); return nil }
	rt.deps.loadSnapshot = func(context.Context, string, Config) error { t.Fatal("restored a snapshot"); return nil }
	coldBooted := false
	rt.deps.coldBoot = func(context.Context, string, *bootParams) error { coldBooted = true; return nil }
	rt.deps.newProxy = func(context.Context, string, string, string, string) (daemonProxy, error) { return &fakeProxy{}, nil }
	rt.deps.probeDaemon = func(context.Context, string) error { return nil }

	const id = "sandbox-id-123456"
	disk := append(bytes.Repeat([]byte{0}, 256<<10), []byte("files")...)
	if _, err := rt.ImportSandbox(context.Background(), id, &runnerruntime.CreateOptions{}, bytes.NewReader(disk)); err != nil {
		t.Fatalf("import: %v", err)
	}
	if !coldBooted {
		t.Fatal("restored sandbox did not cold boot")
	}
	rootfs := filepath.Join(rt.runnerConfig.DataDir, id, "rootfs.ext4")
	got, err := os.ReadFile(rootfs)
	if err != nil || !bytes.Equal(got, disk) {
		t.Fatalf("rootfs differs: %v", err)
	}

	var out bytes.Buffer
	if err := rt.ExportSandbox(context.Background(), id, &out); err == nil || !strings.Contains(err.Error(), "stop") {
		t.Fatalf("export of a running sandbox: %v", err)
	}
	rt.mu.Lock()
	rt.sandboxes[id].running, rt.sandboxes[id].stopped = false, true
	rt.mu.Unlock()
	if err := rt.ExportSandbox(context.Background(), id, &out); err != nil || !bytes.Equal(out.Bytes(), disk) {
		t.Fatalf("export: %v", err)
	}
}

func TestWriteSparseKeepsHoles(t *testing.T) {
	data := append(append(bytes.Repeat([]byte{0}, 1<<20), 'x'), bytes.Repeat([]byte{0}, 1<<20)...)
	f, err := os.Create(filepath.Join(t.TempDir(), "disk"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := writeSparse(f, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(f.Name())
	if !bytes.Equal(got, data) {
		t.Fatal("content differs")
	}
}
