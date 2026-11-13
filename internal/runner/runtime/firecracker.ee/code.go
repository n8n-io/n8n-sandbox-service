package firecracker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// Code sandboxes (Task API) restore one golden code snapshot. The code daemon in
// it already runs with fixed options and, with spare workers on, a ready worker
// process, so the create options must match the options the snapshot was built
// with (code.json, written by create-golden-snapshot.sh --code-options).

const (
	codeOptionsFileName = "code.json"

	// codeDaemonProbeInterval is short, because the create time of a code
	// sandbox is on the critical path of the first task in a scope.
	codeDaemonProbeInterval = 25 * time.Millisecond
	daemonProbeInterval     = 200 * time.Millisecond
)

var errCodeSandboxNoStop = errors.New("code sandboxes cannot be stopped")

// codeOptions are the code daemon options baked into the golden code snapshot.
type codeOptions struct {
	Runtime      string `json:"runtime"`
	Slots        int    `json:"slots"`
	TaskMemoryMB int    `json:"taskMemoryMb"`
	SpareWorkers bool   `json:"spareWorkers"`
	ReuseWorkers bool   `json:"reuseWorkers"`
}

// codeSnapshot is the golden code snapshot a code sandbox is created from.
type codeSnapshot struct {
	rootfsPath string
	memPath    string
	statePath  string
	params     *bootParams
}

// loadCodeSnapshot resolves the golden code snapshot for a create, and rejects
// options the snapshot was not built with.
func (r *Runtime) loadCodeSnapshot(opts *runnerruntime.CreateOptions) (*codeSnapshot, error) {
	if r.config.CodeSnapshotDir == "" {
		return nil, fmt.Errorf("code sandboxes are not enabled on this runner: SANDBOX_RUNNER_FIRECRACKER_CODE_SNAPSHOT_DIR is not set")
	}
	dir := r.config.CodeSnapshotDir
	baked, err := loadCodeOptions(filepath.Join(dir, codeOptionsFileName))
	if err != nil {
		return nil, err
	}
	if err := baked.matches(opts); err != nil {
		return nil, err
	}
	params, err := r.deps.loadBootParams(filepath.Join(dir, bootParamsFileName))
	if err != nil {
		return nil, err
	}
	if err := params.matchesConfig(r.config); err != nil {
		return nil, err
	}
	snap := &codeSnapshot{
		rootfsPath: filepath.Join(r.config.CodeTemplateDir, "rootfs.ext4"),
		memPath:    filepath.Join(dir, "snapshot_mem"),
		statePath:  filepath.Join(dir, "snapshot_state"),
		params:     params,
	}
	for label, path := range map[string]string{
		"code template rootfs": snap.rootfsPath,
		"code snapshot memory": snap.memPath,
		"code snapshot state":  snap.statePath,
	} {
		if !r.deps.pathExists(path) {
			return nil, fmt.Errorf("firecracker %s path does not exist: %s", label, path)
		}
	}
	return snap, nil
}

func loadCodeOptions(path string) (codeOptions, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return codeOptions{}, fmt.Errorf("read golden code snapshot options: %w", err)
	}
	var o codeOptions
	if err := json.Unmarshal(raw, &o); err != nil {
		return codeOptions{}, fmt.Errorf("parse golden code snapshot options %s: %w", path, err)
	}
	return o, nil
}

// matches reports an error when the create options differ from the options
// the code daemon in the snapshot runs with.
func (o codeOptions) matches(opts *runnerruntime.CreateOptions) error {
	got := codeOptions{
		Runtime:      opts.Runtime,
		Slots:        opts.Slots,
		TaskMemoryMB: opts.TaskMemoryMB,
		SpareWorkers: opts.SpareWorkers,
		ReuseWorkers: opts.ReuseWorkers,
	}
	if got != o {
		return fmt.Errorf("the golden code snapshot of this runner only serves %+v, got %+v", o, got)
	}
	return nil
}
