package firecracker

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/daemon/volumedrives"
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
	rt.deps.mountVolumeDrives = func(_ context.Context, baseURL string, drives []volumedrives.Volume) (map[int]string, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if len(drives) != 0 {
			c.calls = append(c.calls, "mount with drives")
		} else {
			c.calls = append(c.calls, "mount "+baseURL)
		}
		return nil, c.err
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
	calls.fail(&volumeMountError{status: http.StatusInternalServerError, reason: "mount"})

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
	drives := []volumedrives.Volume{{Drive: 0, Sectors: 8, Path: "/mnt/a"}, {Drive: 2, Sectors: 16, Path: "/mnt/b"}}
	tests := []struct {
		name       string
		drives     []volumedrives.Volume
		status     int
		answer     string
		wantFailed map[int]string
		wantErr    string
		result     string
	}{
		{"mounted nothing", nil, http.StatusNoContent, "", nil, "", "success"},
		{"daemon without the endpoint", nil, http.StatusNotFound, "404 page not found\n", nil, "", "success"},
		{"daemon not root", nil, http.StatusNotImplemented, `{"error":"not root","code":501}`, nil, "", "success"},
		{"request already made", nil, http.StatusConflict, `{"error":"first request only","code":409}`, nil, "daemon answered 409", "already_mounted"},
		{"not JSON", nil, http.StatusInternalServerError, "guest says no\n", nil, "daemon answered 500", "error"},
		{"redirect", nil, http.StatusTemporaryRedirect, "", nil, "daemon answered 307", "error"},
		{"answer without drives", nil, http.StatusOK, `{"volumes":[]}`, nil, "daemon answered 200", "error"},

		{"each mounted", drives, http.StatusOK, `{"volumes":[{"drive":0,"mounted":true},{"drive":2,"mounted":true}]}`, map[int]string{}, "", "success"},
		{"second not mounted", drives, http.StatusOK, `{"volumes":[{"drive":0,"mounted":true},{"drive":2,"mounted":false,"reason":"mount"}]}`,
			map[int]string{2: "mount"}, "", "mount"},
		{"first not mounted", drives, http.StatusOK, `{"volumes":[{"drive":0,"mounted":false,"reason":"mount_point"},{"drive":2,"mounted":false,"reason":"drive"}]}`,
			map[int]string{0: "mount_point", 2: "drive"}, "", "mount_point"},
		{"older daemon mounted all", drives, http.StatusNoContent, "", nil, "", "success"},
		{"older daemon mounted none", drives, http.StatusInternalServerError, `{"error":"bad superblock","code":500,"reason":"mount"}`,
			map[int]string{0: "mount", 2: "mount"}, "", "mount"},
		{"older daemon with an unknown reason", drives, http.StatusInternalServerError, `{"error":"x","code":500,"reason":"made up"}`, nil, "daemon answered 500", "error"},
		{"drives for a daemon without the endpoint", drives, http.StatusNotFound, "404 page not found\n", nil, "daemon answered 404", "unsupported"},
		{"drives for a daemon not root", drives, http.StatusNotImplemented, `{"error":"not root","code":501}`, nil, "daemon answered 501", "unsupported"},
		{"invalid request", drives, http.StatusBadRequest, `{"error":"invalid request body: x","code":400}`, nil, "daemon answered 400", "invalid_request"},

		{"unknown drive", drives, http.StatusOK, `{"volumes":[{"drive":0,"mounted":true},{"drive":5,"mounted":true}]}`, nil, "invalid answer", "invalid_answer"},
		{"drive twice", drives, http.StatusOK, `{"volumes":[{"drive":0,"mounted":true},{"drive":0,"mounted":true}]}`, nil, "invalid answer", "invalid_answer"},
		{"drive missing", drives, http.StatusOK, `{"volumes":[{"drive":0,"mounted":true}]}`, nil, "invalid answer", "invalid_answer"},
		{"unknown reason", drives, http.StatusOK, `{"volumes":[{"drive":0,"mounted":true},{"drive":2,"mounted":false,"reason":"/etc/shadow"}]}`, nil, "invalid answer", "invalid_answer"},
		{"extra field", drives, http.StatusOK, `{"volumes":[{"drive":0,"mounted":true,"detail":"/etc/shadow"},{"drive":2,"mounted":true}]}`, nil, "invalid answer", "invalid_answer"},
		{"oversized", drives, http.StatusOK, `{"volumes":[{"drive":0,"mounted":true},{"drive":2,"mounted":true}]}` + strings.Repeat(" ", volumedrives.MaxAnswerBytes), nil, "invalid answer", "invalid_answer"},
		{"not JSON with drives", drives, http.StatusOK, "/etc/shadow", nil, "invalid answer", "invalid_answer"},
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

			failed, err := mountVolumeDrives(context.Background(), daemon.URL, tt.drives)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("mountVolumeDrives() error = %v, want none", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("mountVolumeDrives() error = %v, want one containing %q", err, tt.wantErr)
			}
			if err != nil {
				for _, guestText := range []string{"guest says", "superblock", "/etc/shadow", "not root", "first request", "made up"} {
					if strings.Contains(err.Error(), guestText) {
						t.Fatalf("mountVolumeDrives() error %q repeats the daemon's answer", err)
					}
				}
			}
			if len(failed) != len(tt.wantFailed) {
				t.Fatalf("failed drives = %v, want %v", failed, tt.wantFailed)
			}
			for drive, reason := range tt.wantFailed {
				if failed[drive] != reason {
					t.Fatalf("failed drives = %v, want %v", failed, tt.wantFailed)
				}
			}
			if got := volumeMountResult(tt.drives, failed, err); got != tt.result {
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
				wantBody = `{"volumes":[{"drive":0,"sectors":8,"path":"/mnt/a"},{"drive":2,"sectors":16,"path":"/mnt/b"}]}`
			}
			if body != wantBody {
				t.Fatalf("request body = %s, want %s", body, wantBody)
			}
		})
	}
}

// net/http quotes a malformed status line in its error; none of it may reach
// the runner's.
func TestMountVolumeDrivesLeavesOutAMalformedAnswer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// The whole request first: closing with some of it unread can reset the
		// connection before the client reads the answer.
		req, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, req.Body)
		_, _ = io.WriteString(conn, "guest-text-/etc/shadow\r\n\r\n")
	}()

	_, err = mountVolumeDrives(context.Background(), "http://"+listener.Addr().String(), []volumedrives.Volume{{Drive: 0, Sectors: 1, Path: "/mnt/a"}})
	if err == nil || strings.Contains(err.Error(), "shadow") || strings.Contains(err.Error(), "guest-text") {
		t.Fatalf("mountVolumeDrives() error = %v, want one without the answer", err)
	}
	if got := volumeMountResult(nil, nil, err); got != "no_response" {
		t.Fatalf("volumeMountResult() = %q, want no_response", got)
	}
}

func TestMountVolumeDrivesWithoutAnAnswer(t *testing.T) {
	daemon := httptest.NewServer(http.NotFoundHandler())
	url := daemon.URL
	daemon.Close()

	_, err := mountVolumeDrives(context.Background(), url, nil)
	if got := volumeMountResult(nil, nil, err); err == nil || got != "no_response" {
		t.Fatalf("mountVolumeDrives() error = %v, result %q; want an error counted as no_response", err, got)
	}
}
