package firecracker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

const (
	admissionCanaryIDPrefix = "admission-canary-"
	admissionRetryInitial   = time.Second
	admissionRetryMax       = 30 * time.Second
	admissionCanaryFilePath = "/tmp/admission-canary.txt"
	admissionCanaryPayload  = "admission-canary-ok"
)

// Bounds canary DeleteSandbox so cleanup survives probe cancellation without
// hanging forever on a stuck umount/netns host command.
var admissionCanaryCleanupTimeout = 30 * time.Second

type releaseManifest struct {
	GitSHA   string `json:"git_sha"`
	Binaries struct {
		SandboxDaemon struct {
			SHA256 string `json:"sha256"`
		} `json:"sandbox-daemon"`
	} `json:"binaries"`
}

// Prepare pins guest assets, ensures a golden snapshot, configures host NAT,
// and runs an admission canary before marking the runtime ready. Failures
// (including transient host NAT) retry with backoff via runAdmissionLoop.
func (r *Runtime) Prepare(ctx context.Context) {
	r.runAdmissionLoop(ctx)
}

func (r *Runtime) runAdmissionLoop(ctx context.Context) {
	backoff := admissionRetryInitial
	for {
		err := r.admitOnce(ctx)
		if err == nil {
			r.markAdmissionOK()
			slog.Info("firecracker admission succeeded; runner is healthy")
			return
		}
		slog.Error("firecracker admission failed; runner stays unhealthy", "error", err)

		if !sleepContext(ctx, backoff) {
			return
		}
		backoff *= 2
		if backoff > admissionRetryMax {
			backoff = admissionRetryMax
		}
	}
}

// sleepContext waits for d or until ctx is cancelled. Returns false if cancelled.
func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (r *Runtime) admitOnce(ctx context.Context) error {
	if err := r.ensureHostNATReady(ctx); err != nil {
		return fmt.Errorf("host NAT not configured: %w", err)
	}
	if err := r.pinBaseAssets(); err != nil {
		return err
	}
	if err := r.ensureGoldenSnapshot(ctx); err != nil {
		return err
	}
	if err := r.pinSnapshotAssets(); err != nil {
		return err
	}
	return r.runAdmissionCanary(ctx)
}

func (r *Runtime) markAdmissionOK() {
	r.admissionOK.Store(true)
	r.readyOnce.Do(func() {
		close(r.readyCh)
	})
}

// pinBaseAssets verifies Firecracker binaries and the template rootfs/kernel.
func (r *Runtime) pinBaseAssets() error {
	required := map[string]string{
		"jailer":           r.config.JailerBin,
		"firecracker":      r.config.FirecrackerBin,
		"template rootfs":  filepath.Join(r.config.TemplateDir, "rootfs.ext4"),
		"template vmlinux": filepath.Join(r.config.TemplateDir, "vmlinux"),
	}
	for label, path := range required {
		if !r.deps.pathExists(path) {
			return fmt.Errorf("firecracker %s path does not exist: %s", label, path)
		}
	}
	return r.verifyManifestPin()
}

func (r *Runtime) pinSnapshotAssets() error {
	required := map[string]string{
		"snapshot memory": r.config.SnapshotMemPath,
		"snapshot state":  r.config.SnapshotStatePath,
	}
	for label, path := range required {
		if !r.deps.pathExists(path) {
			return fmt.Errorf("firecracker %s path does not exist: %s", label, path)
		}
	}
	return nil
}

func (r *Runtime) verifyManifestPin() error {
	if r.config.ManifestPath == "" && r.config.ExpectedGitSHA == "" {
		return nil
	}
	if r.config.ManifestPath == "" {
		return fmt.Errorf("SANDBOX_RUNNER_FIRECRACKER_EXPECTED_GIT_SHA set but MANIFEST_PATH is empty")
	}
	if !r.deps.pathExists(r.config.ManifestPath) {
		return fmt.Errorf("manifest path does not exist: %s", r.config.ManifestPath)
	}
	raw, err := os.ReadFile(r.config.ManifestPath)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	var manifest releaseManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	if r.config.ExpectedGitSHA != "" && manifest.GitSHA != r.config.ExpectedGitSHA {
		return fmt.Errorf("manifest git_sha %q does not match expected %q", manifest.GitSHA, r.config.ExpectedGitSHA)
	}
	if want := strings.TrimSpace(manifest.Binaries.SandboxDaemon.SHA256); want != "" {
		if !r.deps.pathExists(r.config.DaemonBin) {
			return fmt.Errorf("daemon binary path does not exist for checksum: %s", r.config.DaemonBin)
		}
		got, err := fileSHA256(r.config.DaemonBin)
		if err != nil {
			return fmt.Errorf("hash daemon binary: %w", err)
		}
		if !strings.EqualFold(got, want) {
			return fmt.Errorf("daemon sha256 %s does not match manifest %s", got, want)
		}
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ensureGoldenSnapshot creates host-local mem/state when missing, using the
// bundled create-golden-snapshot.sh when configured.
func (r *Runtime) ensureGoldenSnapshot(ctx context.Context) error {
	if r.deps.pathExists(r.config.SnapshotMemPath) && r.deps.pathExists(r.config.SnapshotStatePath) {
		return nil
	}
	if strings.TrimSpace(r.config.CreateSnapshotScript) == "" {
		return fmt.Errorf("golden snapshot missing at %s / %s and create script is not configured",
			r.config.SnapshotMemPath, r.config.SnapshotStatePath)
	}
	if !r.deps.pathExists(r.config.CreateSnapshotScript) {
		return fmt.Errorf("create snapshot script does not exist: %s", r.config.CreateSnapshotScript)
	}
	if !r.deps.pathExists(r.config.DaemonBin) {
		return fmt.Errorf("daemon binary for snapshot create does not exist: %s", r.config.DaemonBin)
	}
	if !snapshotDirsMatch(r.config.SnapshotMemPath, r.config.SnapshotStatePath) {
		return fmt.Errorf("golden snapshot create requires SnapshotMemPath and SnapshotStatePath in the same directory (got %q and %q); create-golden-snapshot.sh writes snapshot_mem and snapshot_state into a single --out directory",
			r.config.SnapshotMemPath, r.config.SnapshotStatePath)
	}

	outDir := filepath.Dir(r.config.SnapshotMemPath)
	kernel := filepath.Join(r.config.TemplateDir, "vmlinux")
	ext4 := filepath.Join(r.config.TemplateDir, "rootfs.ext4")
	slog.Info("firecracker creating host-local golden snapshot",
		"script", r.config.CreateSnapshotScript,
		"out", outDir,
		"kernel", kernel,
		"ext4", ext4,
	)
	if err := r.deps.run(ctx, "sudo", r.config.CreateSnapshotScript,
		"--kernel", kernel,
		"--ext4", ext4,
		"--daemon-bin", r.config.DaemonBin,
		"--out", outDir,
	); err != nil {
		return fmt.Errorf("create golden snapshot: %w", err)
	}
	if err := ensureSnapshotSymlinks(outDir, r.config.SnapshotMemPath, r.config.SnapshotStatePath, r.deps.pathExists); err != nil {
		return err
	}
	if !r.deps.pathExists(r.config.SnapshotMemPath) || !r.deps.pathExists(r.config.SnapshotStatePath) {
		return fmt.Errorf("golden snapshot still missing after create at %s / %s",
			r.config.SnapshotMemPath, r.config.SnapshotStatePath)
	}
	return nil
}

func ensureSnapshotSymlinks(outDir, memPath, statePath string, exists func(string) bool) error {
	snapshotMem := filepath.Join(outDir, "snapshot_mem")
	snapshotState := filepath.Join(outDir, "snapshot_state")
	if !exists(memPath) && exists(snapshotMem) {
		if err := os.Symlink("snapshot_mem", memPath); err != nil && !os.IsExist(err) {
			return fmt.Errorf("symlink snapshot mem: %w", err)
		}
	}
	if !exists(statePath) && exists(snapshotState) {
		if err := os.Symlink("snapshot_state", statePath); err != nil && !os.IsExist(err) {
			return fmt.Errorf("symlink snapshot state: %w", err)
		}
	}
	return nil
}

func (r *Runtime) runAdmissionCanary(ctx context.Context) (err error) {
	// A prior canary whose delete failed still holds a slot; free it before creating
	// another, or capacity-1 runners can never admit (and never create user sandboxes).
	if cleanErr := r.cleanupLeftoverAdmissionCanaries(ctx); cleanErr != nil {
		return cleanErr
	}

	sandboxID := admissionCanaryIDPrefix + shortID(fmt.Sprintf("%d", time.Now().UnixNano()))
	// shortID is 12 hex chars; prefix makes the full id well over CreateSandbox's 12-char minimum.
	if len(sandboxID) < 12 {
		return fmt.Errorf("internal error: admission canary sandbox id too short: %q", sandboxID)
	}
	slog.Info("firecracker admission canary starting", "sandbox_id", sandboxID)
	if _, createErr := r.CreateSandbox(ctx, sandboxID, nil); createErr != nil {
		return fmt.Errorf("admission canary create: %w", createErr)
	}
	defer func() {
		// Detach from probe cancellation so we still tear down the canary, but keep a
		// deadline so a hung umount/netns cleanup cannot block Prepare/shutdown forever.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), admissionCanaryCleanupTimeout)
		defer cancel()
		delErr := r.DeleteSandbox(cleanupCtx, sandboxID)
		if delErr == nil {
			return
		}
		// Cleanup is part of admission success: a stuck canary permanently consumes a
		// slot, so Ready must not pass until the slot is released.
		if err == nil {
			err = fmt.Errorf("admission canary cleanup: %w", delErr)
			return
		}
		err = fmt.Errorf("%w; admission canary cleanup: %v", err, delErr)
	}()

	daemonURL, urlErr := r.DaemonURL(ctx, sandboxID)
	if urlErr != nil {
		return fmt.Errorf("admission canary daemon url: %w", urlErr)
	}
	if probeErr := probeAdmissionDaemon(ctx, daemonURL); probeErr != nil {
		return fmt.Errorf("admission canary daemon probe: %w", probeErr)
	}
	slog.Info("firecracker admission canary passed", "sandbox_id", sandboxID)
	return nil
}

// cleanupLeftoverAdmissionCanaries deletes canaries left behind when a previous
// admission attempt's DeleteSandbox failed. Used so admission retries can recover
// capacity instead of failing CreateSandbox with "capacity exhausted".
func (r *Runtime) cleanupLeftoverAdmissionCanaries(ctx context.Context) error {
	r.mu.Lock()
	ids := make([]string, 0)
	for id, state := range r.sandboxes {
		if state.deleting {
			continue
		}
		if strings.HasPrefix(id, admissionCanaryIDPrefix) {
			ids = append(ids, id)
		}
	}
	r.mu.Unlock()

	for _, id := range ids {
		if delErr := r.DeleteSandbox(ctx, id); delErr != nil && !errors.Is(delErr, runnerruntime.ErrSandboxNotFound) {
			return fmt.Errorf("cleanup leftover admission canary %s: %w", id, delErr)
		}
	}
	return nil
}

func probeAdmissionDaemon(ctx context.Context, baseURL string) error {
	client := &http.Client{Timeout: 5 * time.Second}

	healthReq, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/healthz", nil)
	if err != nil {
		return err
	}
	healthResp, err := client.Do(healthReq)
	if err != nil {
		return fmt.Errorf("healthz: %w", err)
	}
	_, _ = io.Copy(io.Discard, healthResp.Body)
	_ = healthResp.Body.Close()
	if healthResp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz status %d", healthResp.StatusCode)
	}

	execReq, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		baseURL+"/executions",
		bytes.NewBufferString(`{"command":"true","timeout_ms":2000}`),
	)
	if err != nil {
		return err
	}
	execReq.Header.Set("Content-Type", "application/json")
	execResp, err := client.Do(execReq)
	if err != nil {
		return fmt.Errorf("executions: %w", err)
	}
	execBody, err := io.ReadAll(execResp.Body)
	_ = execResp.Body.Close()
	if err != nil {
		return fmt.Errorf("executions read body: %w", err)
	}
	if execResp.StatusCode != http.StatusOK {
		return fmt.Errorf("executions status %d", execResp.StatusCode)
	}
	if !isSuccessfulExit(execBody) {
		return fmt.Errorf("executions did not report successful exit: %s", truncateForLog(execBody, 256))
	}

	putReq, err := http.NewRequestWithContext(
		ctx,
		http.MethodPut,
		baseURL+"/files?path="+admissionCanaryFilePath+"&overwrite=true",
		bytes.NewBufferString(admissionCanaryPayload),
	)
	if err != nil {
		return err
	}
	putResp, err := client.Do(putReq)
	if err != nil {
		return fmt.Errorf("files put: %w", err)
	}
	_, _ = io.Copy(io.Discard, putResp.Body)
	_ = putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		return fmt.Errorf("files put status %d", putResp.StatusCode)
	}

	getReq, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		baseURL+"/files/content?path="+admissionCanaryFilePath,
		nil,
	)
	if err != nil {
		return err
	}
	getResp, err := client.Do(getReq)
	if err != nil {
		return fmt.Errorf("files get: %w", err)
	}
	got, err := io.ReadAll(getResp.Body)
	_ = getResp.Body.Close()
	if err != nil {
		return fmt.Errorf("files get read body: %w", err)
	}
	if getResp.StatusCode != http.StatusOK {
		return fmt.Errorf("files get status %d", getResp.StatusCode)
	}
	if string(got) != admissionCanaryPayload {
		return fmt.Errorf("files content mismatch: got %q", truncateForLog(got, 64))
	}
	return nil
}

func isSuccessfulExit(body []byte) bool {
	return bytes.Contains(body, []byte(`"type":"exit"`)) && bytes.Contains(body, []byte(`"exit_code":0`))
}

func truncateForLog(b []byte, n int) string {
	s := string(b)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
