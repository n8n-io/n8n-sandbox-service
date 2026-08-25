package firecracker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPinBaseAssetsRequiresVmlinux(t *testing.T) {
	rt := testRuntime(1)
	rt.deps.pathExists = func(path string) bool {
		return path != filepath.Join(rt.config.TemplateDir, "vmlinux")
	}
	if err := rt.pinBaseAssets(); err == nil || !strings.Contains(err.Error(), "vmlinux") {
		t.Fatalf("pinBaseAssets() error = %v, want missing vmlinux", err)
	}
}

func TestVerifyManifestPinGitSHA(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "MANIFEST.json")
	if err := os.WriteFile(manifestPath, []byte(`{"git_sha":"abc123"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	rt := testRuntime(1)
	rt.config.ManifestPath = manifestPath
	rt.config.ExpectedGitSHA = "abc123"
	rt.deps.pathExists = func(path string) bool { return path == manifestPath }

	if err := rt.verifyManifestPin(); err != nil {
		t.Fatalf("verifyManifestPin() failed: %v", err)
	}

	rt.config.ExpectedGitSHA = "other"
	if err := rt.verifyManifestPin(); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("verifyManifestPin() error = %v, want git_sha mismatch", err)
	}
}

func TestVerifyManifestPinDaemonChecksum(t *testing.T) {
	dir := t.TempDir()
	daemonPath := filepath.Join(dir, "sandbox-daemon")
	payload := []byte("daemon-bytes")
	if err := os.WriteFile(daemonPath, payload, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	want := hex.EncodeToString(sum[:])
	manifestPath := filepath.Join(dir, "MANIFEST.json")
	manifest := `{"git_sha":"abc","binaries":{"sandbox-daemon":{"sha256":"` + want + `"}}}`
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	rt := testRuntime(1)
	rt.config.ManifestPath = manifestPath
	rt.config.DaemonBin = daemonPath
	rt.deps.pathExists = func(path string) bool {
		return path == manifestPath || path == daemonPath
	}
	if err := rt.verifyManifestPin(); err != nil {
		t.Fatalf("verifyManifestPin() failed: %v", err)
	}
}

func TestEnsureGoldenSnapshotSkipsWhenPresent(t *testing.T) {
	rt := testRuntime(1)
	called := false
	rt.deps.pathExists = func(string) bool { return true }
	rt.deps.run = func(context.Context, string, ...string) error {
		called = true
		return nil
	}
	if err := rt.ensureGoldenSnapshot(context.Background()); err != nil {
		t.Fatalf("ensureGoldenSnapshot() failed: %v", err)
	}
	if called {
		t.Fatal("expected create script not to run when snapshots exist")
	}
}

// A complete set is checked on every admission, not only in the moment after the
// runner regenerated it: a configured path aiming away from the generated file
// outlives regeneration, and every restart in between would certify it.
func TestEnsureGoldenSnapshotVerifiesACompleteSet(t *testing.T) {
	writeFile := func(t *testing.T, path string) {
		t.Helper()
		if err := os.WriteFile(path, []byte{1}, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeGenerated := func(t *testing.T, outDir string) {
		t.Helper()
		writeFile(t, filepath.Join(outDir, "snapshot_mem"))
		writeFile(t, filepath.Join(outDir, "snapshot_state"))
	}
	symlink := func(t *testing.T, target, path string) {
		t.Helper()
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name    string
		setup   func(t *testing.T, outDir, memPath, statePath string)
		wantErr string
	}{
		{
			name: "configured paths symlinked to the generated files",
			setup: func(t *testing.T, outDir, memPath, statePath string) {
				writeGenerated(t, outDir)
				symlink(t, "snapshot_mem", memPath)
				symlink(t, "snapshot_state", statePath)
			},
		},
		{
			name: "configured memory path is an independent copy beside the generated file",
			setup: func(t *testing.T, outDir, memPath, statePath string) {
				writeGenerated(t, outDir)
				writeFile(t, memPath)
				symlink(t, "snapshot_state", statePath)
			},
			wantErr: "resolves to",
		},
		{
			name: "snapshot assembled by hand, with nothing generated to compare against",
			setup: func(t *testing.T, _, memPath, statePath string) {
				writeFile(t, memPath)
				writeFile(t, statePath)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			outDir := t.TempDir()
			script := filepath.Join(t.TempDir(), "create.sh")
			writeFile(t, script)

			rt := testRuntime(1)
			rt.config.CreateSnapshotScript = script
			rt.config.SnapshotMemPath = filepath.Join(outDir, "mem")
			rt.config.SnapshotStatePath = filepath.Join(outDir, "state")
			rt.deps.pathExists = pathExists
			rt.deps.run = func(context.Context, string, ...string) error {
				t.Fatal("create script ran for a snapshot set that is already complete")
				return nil
			}
			tc.setup(t, outDir, rt.config.SnapshotMemPath, rt.config.SnapshotStatePath)
			writeBootParams(t, bootParamsPath(rt.config), testBootParams(rt.config))

			err := rt.ensureGoldenSnapshot(context.Background())
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("ensureGoldenSnapshot() failed: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("ensureGoldenSnapshot() error = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestEnsureGoldenSnapshotRequiresScriptWhenMissing(t *testing.T) {
	rt := testRuntime(1)
	rt.config.CreateSnapshotScript = ""
	rt.deps.pathExists = func(string) bool { return false }
	if err := rt.ensureGoldenSnapshot(context.Background()); err == nil || !strings.Contains(err.Error(), "create script is not configured") {
		t.Fatalf("ensureGoldenSnapshot() error = %v, want missing script", err)
	}
}

func TestEnsureGoldenSnapshotRejectsSplitSnapshotDirs(t *testing.T) {
	rt := testRuntime(1)
	rt.config.CreateSnapshotScript = "/srv/firecracker/scripts/create-golden-snapshot.sh"
	rt.config.DaemonBin = "/srv/firecracker/bin/sandbox-daemon"
	rt.config.SnapshotMemPath = "/srv/firecracker/snapshots/mem"
	rt.config.SnapshotStatePath = "/var/firecracker/state"
	rt.deps.pathExists = func(path string) bool {
		return path == rt.config.CreateSnapshotScript || path == rt.config.DaemonBin
	}
	if err := rt.ensureGoldenSnapshot(context.Background()); err == nil || !strings.Contains(err.Error(), "same directory") {
		t.Fatalf("ensureGoldenSnapshot() error = %v, want same-directory rejection", err)
	}
}

func TestEnsureGoldenSnapshotRunsScriptAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "create.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	daemon := filepath.Join(dir, "sandbox-daemon")
	if err := os.WriteFile(daemon, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "snapshots")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}

	rt := testRuntime(1)
	rt.config.CreateSnapshotScript = script
	rt.config.DaemonBin = daemon
	rt.config.SnapshotMemPath = filepath.Join(outDir, "mem")
	rt.config.SnapshotStatePath = filepath.Join(outDir, "state")
	rt.config.TemplateDir = filepath.Join(dir, "template")

	rt.deps.pathExists = func(path string) bool {
		switch path {
		case rt.config.SnapshotMemPath, rt.config.SnapshotStatePath:
			_, err := os.Stat(path)
			return err == nil
		case script, daemon:
			return true
		case filepath.Join(outDir, "snapshot_mem"), filepath.Join(outDir, "snapshot_state"), bootParamsPath(rt.config):
			_, err := os.Stat(path)
			return err == nil
		default:
			return false
		}
	}
	rt.deps.run = func(_ context.Context, name string, args ...string) error {
		if name != "sudo" || !strings.Contains(strings.Join(args, " "), script) {
			t.Fatalf("unexpected command %s %v", name, args)
		}
		if err := os.WriteFile(filepath.Join(outDir, "snapshot_mem"), []byte{1}, 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(outDir, "snapshot_state"), []byte{1}, 0o644); err != nil {
			return err
		}
		writeBootParams(t, bootParamsPath(rt.config), testBootParams(rt.config))
		return nil
	}

	if err := rt.ensureGoldenSnapshot(context.Background()); err != nil {
		t.Fatalf("ensureGoldenSnapshot() failed: %v", err)
	}
	if _, err := os.Lstat(rt.config.SnapshotMemPath); err != nil {
		t.Fatalf("expected mem symlink: %v", err)
	}
	if _, err := os.Lstat(rt.config.SnapshotStatePath); err != nil {
		t.Fatalf("expected state symlink: %v", err)
	}
}

func TestRunAdmissionCanary(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/healthz":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/executions":
			_, _ = w.Write([]byte(`{"type":"exit","exit_code":0}` + "\n"))
		case r.Method == http.MethodPut && r.URL.Path == "/files":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/files/content":
			_, _ = w.Write([]byte(admissionCanaryPayload))
		default:
			http.NotFound(w, r)
		}
	}))
	srv.Listener = ln
	srv.Start()
	defer srv.Close()

	rt := testRuntimeT(t, 1)
	rt.config.ProxyPortStart = port
	stubCreateDeps(rt)

	if err := rt.runAdmissionCanary(context.Background()); err != nil {
		t.Fatalf("runAdmissionCanary() failed: %v", err)
	}
	if len(rt.sandboxes) != 0 {
		t.Fatalf("expected canary sandbox deleted, still have %d", len(rt.sandboxes))
	}
}

func TestRunAdmissionCanaryCleanupTimesOut(t *testing.T) {
	prev := admissionCanaryCleanupTimeout
	admissionCanaryCleanupTimeout = 50 * time.Millisecond
	t.Cleanup(func() { admissionCanaryCleanupTimeout = prev })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	srv := startAdmissionCanaryServer(t, ln)
	defer srv.Close()

	rt := testRuntimeT(t, 1)
	rt.config.ProxyPortStart = port
	stubCreateDeps(rt)
	rt.deps.run = func(ctx context.Context, _ string, args ...string) error {
		for _, a := range args {
			if strings.Contains(a, "umount") {
				<-ctx.Done()
				return ctx.Err()
			}
		}
		return nil
	}

	start := time.Now()
	err = rt.runAdmissionCanary(context.Background())
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "admission canary cleanup") {
		t.Fatalf("runAdmissionCanary() error = %v, want cleanup failure", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("canary cleanup took %v; expected bounded timeout", elapsed)
	}
}

func TestRunAdmissionCanaryFailsWhenCleanupFails(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	srv := startAdmissionCanaryServer(t, ln)
	defer srv.Close()

	rt := testRuntimeT(t, 1)
	rt.config.ProxyPortStart = port
	stubCreateDeps(rt)
	rt.deps.run = func(_ context.Context, _ string, args ...string) error {
		for _, a := range args {
			if strings.Contains(a, "umount") {
				return fmt.Errorf("forced host cleanup failure")
			}
		}
		return nil
	}

	err = rt.runAdmissionCanary(context.Background())
	if err == nil || !strings.Contains(err.Error(), "admission canary cleanup") {
		t.Fatalf("runAdmissionCanary() error = %v, want cleanup failure", err)
	}
	if len(rt.sandboxes) != 1 {
		t.Fatalf("expected stuck canary still occupying a slot, have %d sandboxes", len(rt.sandboxes))
	}
	cap, capErr := rt.Capacity(context.Background())
	if capErr != nil {
		t.Fatalf("Capacity() failed: %v", capErr)
	}
	if cap.Used != 1 {
		t.Fatalf("Capacity.Used = %d, want 1 (canary still holding slot)", cap.Used)
	}
	if rt.admissionOK.Load() {
		t.Fatal("admissionOK must stay false when canary cleanup fails")
	}

	// Recovery: once host cleanup works again, the next admit attempt should
	// purge the leftover canary and complete successfully on a capacity-1 runner.
	stubCreateDeps(rt)
	rt.config.ProxyPortStart = port
	if err := rt.runAdmissionCanary(context.Background()); err != nil {
		t.Fatalf("runAdmissionCanary() recovery failed: %v", err)
	}
	if len(rt.sandboxes) != 0 {
		t.Fatalf("expected canary deleted after recovery, still have %d", len(rt.sandboxes))
	}
	cap, capErr = rt.Capacity(context.Background())
	if capErr != nil {
		t.Fatalf("Capacity() after recovery failed: %v", capErr)
	}
	if cap.Used != 0 {
		t.Fatalf("Capacity.Used = %d after recovery, want 0", cap.Used)
	}
}

func startAdmissionCanaryServer(t *testing.T, ln net.Listener) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/healthz":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/executions":
			_, _ = w.Write([]byte(`{"type":"exit","exit_code":0}` + "\n"))
		case r.Method == http.MethodPut && r.URL.Path == "/files":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/files/content":
			_, _ = w.Write([]byte(admissionCanaryPayload))
		default:
			http.NotFound(w, r)
		}
	}))
	srv.Listener = ln
	srv.Start()
	return srv
}

func TestProbeAdmissionDaemon(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/healthz":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/executions":
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = w.Write([]byte(`{"type":"exit","exit_code":0}` + "\n"))
		case r.Method == http.MethodPut && r.URL.Path == "/files":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/files/content":
			_, _ = w.Write([]byte(admissionCanaryPayload))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	if err := probeAdmissionDaemon(context.Background(), srv.URL); err != nil {
		t.Fatalf("probeAdmissionDaemon() failed: %v", err)
	}
}

func TestProbeAdmissionDaemonFailsOnTruncatedBody(t *testing.T) {
	tests := []struct {
		name       string
		wantSubstr string
		hijackPath string
		partial    string
	}{
		{
			name:       "truncated executions body with success markers",
			wantSubstr: "executions read body",
			hijackPath: "/executions",
			partial:    `{"type":"exit","exit_code":0}`,
		},
		{
			name:       "truncated files content matching payload",
			wantSubstr: "files get read body",
			hijackPath: "/files/content",
			partial:    admissionCanaryPayload,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/healthz":
					w.WriteHeader(http.StatusOK)
				case r.Method == http.MethodPost && r.URL.Path == "/executions":
					if tc.hijackPath == "/executions" {
						writeTruncatedHTTPResponse(t, w, tc.partial)
						return
					}
					_, _ = w.Write([]byte(`{"type":"exit","exit_code":0}` + "\n"))
				case r.Method == http.MethodPut && r.URL.Path == "/files":
					w.WriteHeader(http.StatusOK)
				case r.Method == http.MethodGet && r.URL.Path == "/files/content":
					if tc.hijackPath == "/files/content" {
						writeTruncatedHTTPResponse(t, w, tc.partial)
						return
					}
					_, _ = w.Write([]byte(admissionCanaryPayload))
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()

			err := probeAdmissionDaemon(context.Background(), srv.URL)
			if err == nil || !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("probeAdmissionDaemon() error = %v, want %q", err, tc.wantSubstr)
			}
		})
	}
}

// writeTruncatedHTTPResponse sends a 200 with Content-Length longer than the
// body, then closes the connection so io.ReadAll returns a partial body + error.
func writeTruncatedHTTPResponse(t *testing.T, w http.ResponseWriter, partial string) {
	t.Helper()
	hj, ok := w.(http.Hijacker)
	if !ok {
		t.Fatal("ResponseWriter does not support hijacking")
	}
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		t.Fatalf("Hijack: %v", err)
	}
	_, _ = fmt.Fprintf(bufrw, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s", len(partial)+64, partial)
	_ = bufrw.Flush()
	_ = conn.Close()
}

func TestEnsureHostNATReadyRetriesAfterFailure(t *testing.T) {
	rt := testRuntime(1)
	var calls int
	rt.deps.run = func(context.Context, string, ...string) error {
		calls++
		if calls == 1 {
			return fmt.Errorf("transient nat failure")
		}
		return nil
	}

	if err := rt.ensureHostNATReady(context.Background()); err == nil || !strings.Contains(err.Error(), "transient nat failure") {
		t.Fatalf("ensureHostNATReady() first call error = %v, want transient failure", err)
	}
	if rt.hostNATOK {
		t.Fatal("hostNATOK must stay false after failed setup")
	}

	if err := rt.ensureHostNATReady(context.Background()); err != nil {
		t.Fatalf("ensureHostNATReady() retry failed: %v", err)
	}
	if !rt.hostNATOK {
		t.Fatal("hostNATOK should be true after successful setup")
	}
	if err := rt.ensureHostNATReady(context.Background()); err != nil {
		t.Fatalf("ensureHostNATReady() after success: %v", err)
	}
	if calls != 2 {
		t.Fatalf("EnsureHostNAT calls = %d, want 2 (fail once, succeed once, then cache)", calls)
	}
}

func TestPrepareSucceedsViaAdmitOnce(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	srv := startAdmissionCanaryServer(t, ln)
	defer srv.Close()

	rt := testRuntimeT(t, 1)
	rt.config.ProxyPortStart = port
	stubCreateDeps(rt)
	useTestGoldenSnapshotDir(t, rt)

	select {
	case <-rt.ReadyCh():
		t.Fatal("ReadyCh should not be closed before admission")
	default:
	}

	// Prepare retries admission until it succeeds or the context ends. Bounding
	// it turns a failure into a test failure with the admission error in the log
	// rather than a hang that only surfaces as a package-wide timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rt.Prepare(ctx)
	if ctx.Err() != nil {
		t.Fatal("Prepare gave up on admission; see the logged admission error")
	}

	select {
	case <-rt.ReadyCh():
	default:
		t.Fatal("ReadyCh should be closed after Prepare succeeds")
	}
	if err := rt.Ready(context.Background()); err != nil {
		t.Fatalf("Ready() after Prepare: %v", err)
	}
	if !rt.admissionOK.Load() {
		t.Fatal("admissionOK should be true after Prepare")
	}
	if len(rt.sandboxes) != 0 {
		t.Fatalf("expected canary cleaned up after admitOnce, have %d sandboxes", len(rt.sandboxes))
	}
}

func TestIsSuccessfulExit(t *testing.T) {
	if !isSuccessfulExit([]byte(`{"type":"stdout"}\n{"type":"exit","exit_code":0}`)) {
		t.Fatal("expected successful exit")
	}
	if isSuccessfulExit([]byte(`{"type":"exit","exit_code":1}`)) {
		t.Fatal("expected non-zero exit to fail")
	}
}
