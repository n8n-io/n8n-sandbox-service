package firecracker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	admissionCanaryIDPrefix = "admission-canary-"
	admissionRetryInitial   = time.Second
	admissionRetryMax       = 30 * time.Second
	admissionCanaryFilePath = "/tmp/admission-canary.txt"
	admissionCanaryPayload  = "admission-canary-ok"
)

type releaseManifest struct {
	GitSHA   string `json:"git_sha"`
	Binaries struct {
		SandboxDaemon struct {
			SHA256 string `json:"sha256"`
		} `json:"sandbox-daemon"`
	} `json:"binaries"`
}

// Prepare ensures host NAT, then pins guest assets, ensures a golden snapshot,
// and runs an admission canary before marking the runtime ready.
func (r *Runtime) Prepare(ctx context.Context) {
	_ = r.ensureHostNATReady(ctx)
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

func (r *Runtime) runAdmissionCanary(ctx context.Context) error {
	sandboxID := admissionCanaryIDPrefix + shortID(fmt.Sprintf("%d", time.Now().UnixNano()))
	// shortID is 12 hex chars; prefix makes the full id well over CreateSandbox's 12-char minimum.
	if len(sandboxID) < 12 {
		return fmt.Errorf("internal error: admission canary sandbox id too short: %q", sandboxID)
	}
	slog.Info("firecracker admission canary starting", "sandbox_id", sandboxID)
	if _, err := r.CreateSandbox(ctx, sandboxID, nil); err != nil {
		return fmt.Errorf("admission canary create: %w", err)
	}
	defer func() {
		if delErr := r.DeleteSandbox(context.WithoutCancel(ctx), sandboxID); delErr != nil {
			slog.Warn("firecracker admission canary cleanup failed", "sandbox_id", sandboxID, "error", delErr)
		}
	}()

	daemonURL, err := r.DaemonURL(ctx, sandboxID)
	if err != nil {
		return fmt.Errorf("admission canary daemon url: %w", err)
	}
	if err := probeAdmissionDaemon(ctx, daemonURL); err != nil {
		return fmt.Errorf("admission canary daemon probe: %w", err)
	}
	slog.Info("firecracker admission canary passed", "sandbox_id", sandboxID)
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
	execBody, _ := io.ReadAll(execResp.Body)
	_ = execResp.Body.Close()
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
	got, _ := io.ReadAll(getResp.Body)
	_ = getResp.Body.Close()
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
