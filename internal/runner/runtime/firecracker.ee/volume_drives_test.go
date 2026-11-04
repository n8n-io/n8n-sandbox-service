package firecracker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/metrics"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// volumeMountCalls records the daemon probe and volume mount calls, in order.
type volumeMountCalls struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (c *volumeMountCalls) install(rt *Runtime) {
	rt.deps.probeDaemon = func(context.Context, string) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.calls = append(c.calls, "probe")
		return nil
	}
	rt.deps.mountVolumeDrives = func(_ context.Context, baseURL string, drives []volumeDrive) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		if len(drives) != 0 {
			c.calls = append(c.calls, "mount with drives")
		} else {
			c.calls = append(c.calls, "mount "+baseURL)
		}
		return c.err
	}
}

func (c *volumeMountCalls) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
}

func (c *volumeMountCalls) require(t *testing.T, want ...string) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !slices.Equal(c.calls, want) {
		t.Fatalf("daemon calls = %q, want %q", c.calls, want)
	}
}

const testMountCall = "mount http://127.0.0.1:18081"

func TestCreateSandboxSendsTheVolumeMountRequest(t *testing.T) {
	rt := testRuntimeT(t, 1)
	stubCreateDeps(rt)
	rec := metrics.NewRunnerRecorder(true)
	rt.SetMetricsRecorder(rec)
	calls := &volumeMountCalls{}
	calls.install(rt)

	if _, err := rt.CreateSandbox(context.Background(), "sandbox-id-123456", nil); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	calls.require(t, "probe", testMountCall)
	if got := rec.VolumeMountCount("success"); got != 1 {
		t.Fatalf("volume mounts counted as success = %v, want 1", got)
	}
}

func TestColdBootSendsTheVolumeMountRequest(t *testing.T) {
	rt := testRuntimeT(t, 1)
	stubCreateDeps(rt)
	calls := &volumeMountCalls{}
	calls.install(rt)
	exits := &guestExitStub{}
	exits.install(rt, &fakeProcess{})

	const sandboxID = "sandbox-id-123456"
	if _, err := rt.CreateSandbox(context.Background(), sandboxID, nil); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	exits.fire(t, 0)
	recoversByColdBoot(t, rt, sandboxID)
	calls.require(t, "probe", testMountCall, "probe", testMountCall)
}

func TestSnapshotWakeSkipsTheVolumeMountRequest(t *testing.T) {
	rt := testRuntimeT(t, 1)
	stubCreateDeps(rt)
	calls := &volumeMountCalls{}
	calls.install(rt)

	const sandboxID = "sandbox-id-123456"
	if _, err := rt.CreateSandbox(context.Background(), sandboxID, nil); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	if err := rt.StopSandbox(context.Background(), sandboxID); err != nil {
		t.Fatalf("StopSandbox() failed: %v", err)
	}
	wake, err := rt.EnsureSandboxRunning(context.Background(), sandboxID)
	if err != nil {
		t.Fatalf("EnsureSandboxRunning() failed: %v", err)
	}
	if wake.Recovered {
		t.Fatal("wake was a recovery, want a restore of the stop snapshot")
	}
	calls.require(t, "probe", testMountCall, "probe")
}

func TestCreateSandboxFailsWhenTheVolumeMountRequestFails(t *testing.T) {
	rt := testRuntimeT(t, 1)
	stubCreateDeps(rt)
	proc := &fakeProcess{}
	rt.deps.start = func(context.Context, func(error), string, ...string) (process, error) { return proc, nil }
	rec := metrics.NewRunnerRecorder(true)
	rt.SetMetricsRecorder(rec)
	calls := &volumeMountCalls{}
	calls.install(rt)
	calls.fail(&volumeMountError{status: http.StatusInternalServerError, reason: "mount", msg: "mount volumes: bad superblock"})

	const sandboxID = "sandbox-id-123456"
	_, err := rt.CreateSandbox(context.Background(), sandboxID, nil)
	if err == nil || !strings.Contains(err.Error(), "mount guest volume drives") {
		t.Fatalf("CreateSandbox() error = %v, want the volume mount failure", err)
	}
	if !proc.killed {
		t.Fatal("the guest whose volume mount failed was not killed")
	}
	if _, err := rt.GetSandboxInfo(context.Background(), sandboxID); !errors.Is(err, runnerruntime.ErrSandboxNotFound) {
		t.Fatalf("GetSandboxInfo() error = %v, want the failed create untracked", err)
	}
	if got := capacityOf(t, rt); got.Used != 0 {
		t.Fatalf("capacity = %+v, want the slot released", got)
	}
	if got := rec.VolumeMountCount("mount"); got != 1 {
		t.Fatalf("volume mounts counted as failed mounts = %v, want 1", got)
	}
}

func TestColdBootFailsWhenTheVolumeMountRequestFails(t *testing.T) {
	rt := testRuntimeT(t, 1)
	stubCreateDeps(rt)
	calls := &volumeMountCalls{}
	calls.install(rt)
	exits := &guestExitStub{}
	proc := &fakeProcess{}
	exits.install(rt, proc)

	const sandboxID = "sandbox-id-123456"
	if _, err := rt.CreateSandbox(context.Background(), sandboxID, nil); err != nil {
		t.Fatalf("CreateSandbox() failed: %v", err)
	}
	exits.fire(t, 0)

	calls.fail(&volumeMountError{status: http.StatusConflict})
	proc.killed = false
	if _, err := rt.EnsureSandboxRunning(context.Background(), sandboxID); err == nil || !strings.Contains(err.Error(), "mount guest volume drives") {
		t.Fatalf("EnsureSandboxRunning() error = %v, want the volume mount failure", err)
	}
	if !proc.killed {
		t.Fatal("the cold booted guest whose volume mount failed was not killed")
	}
	if _, err := rt.DaemonURL(context.Background(), sandboxID); !errors.Is(err, runnerruntime.ErrSandboxNotRunning) {
		t.Fatalf("DaemonURL() error = %v, want ErrSandboxNotRunning", err)
	}
	if got := capacityOf(t, rt); got.Used != 0 || got.Stopped != 1 {
		t.Fatalf("capacity = %+v, want used 0 and stopped 1", got)
	}

	calls.fail(nil)
	recoversByColdBoot(t, rt, sandboxID)
	calls.require(t, "probe", testMountCall, "probe", testMountCall, "probe", testMountCall)
}

func TestMountVolumeDrives(t *testing.T) {
	drives := []volumeDrive{{Drive: 0, Sectors: 8, Path: "/mnt/a"}}
	tests := []struct {
		name    string
		drives  []volumeDrive
		status  int
		answer  string
		wantErr string
		result  string
	}{
		{"mounted nothing", nil, http.StatusNoContent, "", "", "success"},
		{"mounted", drives, http.StatusNoContent, "", "", "success"},
		{"daemon without the endpoint", nil, http.StatusNotFound, "404 page not found\n", "", "success"},
		{"daemon not root", nil, http.StatusNotImplemented, `{"error":"not root","code":501}`, "", "success"},
		{"drives for a daemon without the endpoint", drives, http.StatusNotFound, "404 page not found\n", "daemon answered 404: 404 page not found", "unsupported"},
		{"drives for a daemon not root", drives, http.StatusNotImplemented, `{"error":"not root","code":501}`, "daemon answered 501: not root", "unsupported"},
		{"request already made", nil, http.StatusConflict, `{"error":"first request only","code":409}`, "daemon answered 409: first request only", "already_mounted"},
		{"invalid request", drives, http.StatusBadRequest, `{"error":"invalid request body: x","code":400}`, "daemon answered 400: invalid request body: x", "invalid_request"},
		{"drive never ready", drives, http.StatusInternalServerError, `{"error":"/dev/vdb has 0 bytes","code":500,"reason":"drive"}`, "daemon answered 500: /dev/vdb has 0 bytes", "drive"},
		{"mount point refused", drives, http.StatusInternalServerError, `{"error":"symlink","code":500,"reason":"mount_point"}`, "daemon answered 500: symlink", "mount_point"},
		{"mount refused", drives, http.StatusInternalServerError, `{"error":"bad superblock","code":500,"reason":"mount"}`, "daemon answered 500: bad superblock", "mount"},
		{"unknown reason", drives, http.StatusInternalServerError, `{"error":"x","code":500,"reason":"made up"}`, "daemon answered 500: x", "error"},
		{"not JSON", nil, http.StatusInternalServerError, "guest says no\n", "daemon answered 500: guest says no", "error"},
		{"redirect", nil, http.StatusTemporaryRedirect, "", "daemon answered 307", "error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			var method, path, contentType, body string
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) > 1 {
					t.Error("request was retried")
				}
				method, path, contentType = r.Method, r.URL.Path, r.Header.Get("Content-Type")
				b, _ := io.ReadAll(r.Body)
				body = string(b)
				if tt.status == http.StatusTemporaryRedirect {
					http.Redirect(w, r, "/elsewhere", tt.status)
					return
				}
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.answer)
			}))
			defer daemon.Close()

			err := mountVolumeDrives(context.Background(), daemon.URL, tt.drives)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("mountVolumeDrives() error = %v, want none", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("mountVolumeDrives() error = %v, want one containing %q", err, tt.wantErr)
			}
			if got := volumeMountResult(err); got != tt.result {
				t.Fatalf("volumeMountResult() = %q, want %q", got, tt.result)
			}
			if got := requests.Load(); got != 1 {
				t.Fatalf("daemon got %d requests, want 1", got)
			}
			if method != http.MethodPost || path != "/volumes/mount" || contentType != "application/json" {
				t.Fatalf("request = %s %s (%s), want POST /volumes/mount (application/json)", method, path, contentType)
			}
			wantBody := `{"volumes":[]}`
			if len(tt.drives) > 0 {
				wantBody = `{"volumes":[{"drive":0,"sectors":8,"path":"/mnt/a"}]}`
			}
			if body != wantBody {
				t.Fatalf("request body = %s, want %s", body, wantBody)
			}
		})
	}
}

func TestMountVolumeDrivesWithoutAnAnswer(t *testing.T) {
	daemon := httptest.NewServer(http.NotFoundHandler())
	url := daemon.URL
	daemon.Close()

	err := mountVolumeDrives(context.Background(), url, nil)
	if got := volumeMountResult(err); err == nil || got != "no_response" {
		t.Fatalf("mountVolumeDrives() error = %v, result %q; want an error counted as no_response", err, got)
	}
}
