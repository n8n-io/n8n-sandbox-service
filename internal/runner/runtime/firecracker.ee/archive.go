package firecracker

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

var _ runnerruntime.Archiver = (*Runtime)(nil)

// ExportSandbox streams a stopped sandbox's rootfs, under a claim on its
// transitions so nothing wakes it mid-copy. The memory snapshot stays: an
// archive holds files, and the restored guest cold boots.
func (r *Runtime) ExportSandbox(ctx context.Context, sandboxID string, w io.Writer) error {
	state, _, cancel, err := r.beginTransition(ctx, sandboxID, transitionStopping)
	if err != nil {
		return err
	}
	defer cancel()
	defer r.endTransition(state)
	r.mu.Lock()
	stopped, path := state.stopped && !state.running, state.rootfsPath
	r.mu.Unlock()
	if !stopped {
		return fmt.Errorf("%w: stop it before archiving", runnerruntime.ErrSandboxNotRunning)
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open rootfs: %w", err)
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

// ImportSandbox creates sandboxID with its rootfs read from archive and cold
// boots it with the template's boot parameters.
func (r *Runtime) ImportSandbox(ctx context.Context, sandboxID string, opts *runnerruntime.CreateOptions, archive io.Reader) (*runnerruntime.SandboxInfo, error) {
	return r.createSandbox(ctx, sandboxID, opts, archive)
}

// restoreRootfs writes the archive as the sandbox's rootfs and leaves empty
// snapshot files for the jail to bind; the sandbox is pinned to cold boot.
func (r *Runtime) restoreRootfs(state *sandboxState, archive io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(state.rootfsPath), 0o755); err != nil {
		return fmt.Errorf("create sandbox data dir: %w", err)
	}
	f, err := os.OpenFile(state.rootfsPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create rootfs: %w", err)
	}
	if err := writeSparse(f, archive); err != nil {
		_ = f.Close()
		return fmt.Errorf("write rootfs from archive: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	for _, p := range []string{state.snapshotMemPath, state.snapshotStatePath} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			return fmt.Errorf("create snapshot placeholder: %w", err)
		}
	}
	r.mu.Lock()
	state.mustColdBoot = true
	r.mu.Unlock()
	return nil
}

// writeSparse copies r to f, seeking over blocks of zeros so the rootfs stays
// as sparse as the template clone it was archived from.
func writeSparse(f *os.File, r io.Reader) error {
	buf := make([]byte, 64<<10)
	var size int64
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			block := buf[:n]
			if isZero(block) {
				if _, err := f.Seek(int64(n), io.SeekCurrent); err != nil {
					return err
				}
			} else if _, err := f.Write(block); err != nil {
				return err
			}
			size += int64(n)
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return f.Truncate(size)
		}
		if err != nil {
			return err
		}
	}
}

func isZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
