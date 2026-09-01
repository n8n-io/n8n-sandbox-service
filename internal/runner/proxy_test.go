package runner

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/runner/config"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
	"github.com/n8n-io/sandbox-service/internal/sandboxproxy"
)

const proxyTestSandboxID = "550e8400-e29b-41d4-a716-446655440000"

// Every handler that can wake a sandbox goes through resolveDaemonURL, so they all
// have to refuse a recovery rather than only the plain proxy.
var wakingHandlers = map[string]func(runnerruntime.Runtime, *config.Config, *metrics.RunnerRecorder) http.HandlerFunc{
	"proxy":        ProxyHandler,
	"upload proxy": UploadProxyHandler,
	"exec proxy":   ExecProxyHandler,
}

// MaxFileBytes has to be set for the upload proxy, which enforces it on every body
// it forwards and would otherwise reject this one as too large before proxying.
func proxyTestConfig() *config.Config {
	return &config.Config{MaxFileBytes: 1 << 20}
}

func proxyTestRequest() *http.Request {
	body := `{"command":"echo hello","exec_id":"exec-after-recovery"}`
	req := httptest.NewRequest(http.MethodPost, "/sandboxes/"+proxyTestSandboxID+"/executions", strings.NewReader(body))
	req.SetPathValue("id", proxyTestSandboxID)
	return req
}

// The request that drove a recovery is refused, not proxied. The sandbox is up by
// then, so a 200 here would look entirely healthy while the processes an earlier
// request started, and the daemon's execution history, are gone — a loss the client
// would otherwise meet as a bug in its own code.
//
// The retry is the other half, and the reason refusing is safe at all: the recovery
// runs before the refusal, so the sandbox is reachable by the time the client comes
// back and the retry the 409 asks for is not a gamble. Both are asserted here
// because either alone passes for a broken handler — one that refuses without ever
// recovering leaves the client retrying into a sandbox that is still down, and one
// that recovers without refusing hands back the silent 200 the status exists to
// prevent.
func TestProxyHandlersRefuseARecoveryThenServeTheRetry(t *testing.T) {
	for name, newHandler := range wakingHandlers {
		t.Run(name, func(t *testing.T) {
			var daemonHits atomic.Int32
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				daemonHits.Add(1)
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(exitEvent(1) + "\n"))
			}))
			defer daemon.Close()

			rt := &fakeRuntime{
				daemonURL: daemon.URL,
				daemonErr: runnerruntime.ErrSandboxNotRunning,
				recovered: true,
			}
			handler := newHandler(rt, proxyTestConfig(), metrics.NewRunnerRecorder(false))

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, proxyTestRequest())

			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
			}
			if got := daemonHits.Load(); got != 0 {
				t.Errorf("daemon hits = %d, want 0: a request whose sandbox was recovered under it must not be proxied", got)
			}
			if got := rec.Header().Get(sandboxproxy.SandboxRestartedHeader); got != "1" {
				t.Errorf("%s = %q, want 1; it is the channel that survives both proxy hops", sandboxproxy.SandboxRestartedHeader, got)
			}
			var payload struct {
				Error  string `json:"error"`
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatalf("decode body %q: %v", rec.Body.String(), err)
			}
			if payload.Reason != "sandbox_restarted" {
				t.Errorf("reason = %q, want sandbox_restarted", payload.Reason)
			}
			if payload.Error == "" {
				t.Error("body has no error message to show a client")
			}

			// The retry the 409 asked for, on the same runtime the refusal left behind.
			// It reaches the daemon without driving a second wake, which is what makes it
			// deterministic rather than a client guessing when to come back.
			retry := httptest.NewRecorder()
			handler.ServeHTTP(retry, proxyTestRequest())

			if retry.Code != http.StatusOK {
				t.Fatalf("retry status = %d, want 200: %s", retry.Code, retry.Body.String())
			}
			if got := retry.Header().Get(sandboxproxy.SandboxRestartedHeader); got != "" {
				t.Errorf("retry %s = %q, want it unset: one crash is reported once", sandboxproxy.SandboxRestartedHeader, got)
			}
			if got := daemonHits.Load(); got != 1 {
				t.Errorf("daemon hits = %d, want 1: the retry has to land on the recovered sandbox", got)
			}
		})
	}
}

// The guard against the crash contract leaking into ordinary wakes, which are
// transparent by design: nothing was lost, so there is nothing to report.
func TestProxyHandlersDoNotRefuseAnOrdinaryWake(t *testing.T) {
	for name, newHandler := range wakingHandlers {
		t.Run(name, func(t *testing.T) {
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(exitEvent(1) + "\n"))
			}))
			defer daemon.Close()

			rt := &fakeRuntime{
				daemonURL: daemon.URL,
				daemonErr: runnerruntime.ErrSandboxNotRunning,
			}
			rec := httptest.NewRecorder()
			newHandler(rt, proxyTestConfig(), metrics.NewRunnerRecorder(false)).ServeHTTP(rec, proxyTestRequest())

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get(sandboxproxy.SandboxRestartedHeader); got != "" {
				t.Errorf("%s = %q on an ordinary wake, want it unset", sandboxproxy.SandboxRestartedHeader, got)
			}
		})
	}
}

func deleteExecutionRequest() *http.Request {
	path := "/sandboxes/" + proxyTestSandboxID + "/executions/exec-already-finished"
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	req.SetPathValue("id", proxyTestSandboxID)
	req.SetPathValue("exec_id", "exec-already-finished")
	return req
}

// Deleting an execution must not wake the sandbox, and above all must not spend the
// one restart report a crash gets. The SDK sends this delete after every execution
// and discards the answer, so a crash that lands in that window would be reported to
// a request nobody reads, and the client's next call would meet a sandbox that looks
// healthy and has lost everything it was running.
//
// The 204 is honest rather than a fiction that keeps the report: an execution lives
// only in the daemon's memory, so a sandbox that is not running has already lost it.
func TestDeleteExecutionDoesNotWakeOrSpendTheRestartReport(t *testing.T) {
	var daemonHits atomic.Int32
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		daemonHits.Add(1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(exitEvent(1) + "\n"))
	}))
	defer daemon.Close()

	rt := &fakeRuntime{
		daemonURL: daemon.URL,
		daemonErr: runnerruntime.ErrSandboxNotRunning,
		recovered: true,
	}
	cfg, rec := proxyTestConfig(), metrics.NewRunnerRecorder(false)

	del := httptest.NewRecorder()
	DeleteExecutionHandler(rt, cfg, rec).ServeHTTP(del, deleteExecutionRequest())

	if del.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", del.Code, del.Body.String())
	}
	if got := rt.ensureCalls.Load(); got != 0 {
		t.Errorf("wakes = %d, want 0: a cleanup delete is not a client asking to use the sandbox", got)
	}
	if got := daemonHits.Load(); got != 0 {
		t.Errorf("daemon hits = %d, want 0", got)
	}

	// The report is still there for the request that has a client behind it.
	exec := httptest.NewRecorder()
	ExecProxyHandler(rt, cfg, rec).ServeHTTP(exec, proxyTestRequest())

	if exec.Code != http.StatusConflict {
		t.Fatalf("exec status = %d, want 409: the delete consumed the restart report", exec.Code)
	}
}

// A running sandbox has the execution the delete names, so it is proxied like any
// other request.
func TestDeleteExecutionIsProxiedWhenTheSandboxIsRunning(t *testing.T) {
	var daemonHits atomic.Int32
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		daemonHits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer daemon.Close()

	rt := &fakeRuntime{daemonURL: daemon.URL}
	rec := httptest.NewRecorder()
	DeleteExecutionHandler(rt, proxyTestConfig(), metrics.NewRunnerRecorder(false)).ServeHTTP(rec, deleteExecutionRequest())

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", rec.Code, rec.Body.String())
	}
	if got := daemonHits.Load(); got != 1 {
		t.Errorf("daemon hits = %d, want 1", got)
	}
}
