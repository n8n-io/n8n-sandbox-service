package runner

import (
	"context"
	"encoding/json"
	"fmt"
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

// crashingRuntime dies on the nth daemon lookup a request makes. It is the CI
// timeline that broke the first attempt at this fix: a delete that arrived while the
// sandbox was alive, looked it up, and had the guest die before it looked again. The
// lookup is a docker ps and an inspect, so the gap between two of them is wide enough
// for a whole crash.
type crashingRuntime struct {
	*fakeRuntime
	dieOnLook int32
	looks     atomic.Int32
}

func (c *crashingRuntime) DaemonURL(ctx context.Context, id string) (string, error) {
	if c.looks.Add(1) >= c.dieOnLook {
		return "", runnerruntime.ErrSandboxNotRunning
	}
	return c.fakeRuntime.DaemonURL(ctx, id)
}

// Deleting an execution must not wake the sandbox, and above all must not spend the
// one restart report a crash gets. The SDK sends this delete after every command and
// discards the answer, so a crash that lands in that window would be reported to a
// request nobody reads, and the client's next call would meet a sandbox that looks
// healthy and has lost everything it was running.
//
// The 204 is honest rather than a fiction that keeps the report: an execution lives
// only in the guest's memory, so a sandbox that is not running has already lost it.
func TestDeleteExecutionDoesNotWakeOrSpendTheRestartReport(t *testing.T) {
	// A sandbox already down when the delete arrives, and one that goes down while
	// the delete is being served. The two are answered by different halves of the
	// handler — the first by the runner, the second by the daemon it was already
	// proxied to — and what has to hold for both is that neither woke the sandbox,
	// neither reported a restart, and neither looked twice. The second lookup is
	// the whole bug: it is where a crash gets to change an answer already given.
	crashAt := map[string]int32{"before the delete": 1, "during the delete": 2}

	for name, dieOnLook := range crashAt {
		t.Run(name, func(t *testing.T) {
			var daemonHits atomic.Int32
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				daemonHits.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer daemon.Close()

			base := &fakeRuntime{daemonURL: daemon.URL, recovered: true}
			rt := &crashingRuntime{fakeRuntime: base, dieOnLook: dieOnLook}
			cfg, rec := proxyTestConfig(), metrics.NewRunnerRecorder(false)

			del := httptest.NewRecorder()
			DeleteExecutionHandler(rt, cfg, rec).ServeHTTP(del, deleteExecutionRequest())

			if del.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204: %s", del.Code, del.Body.String())
			}
			if got := base.ensureCalls.Load(); got != 0 {
				t.Errorf("wakes = %d, want 0: a cleanup delete is not a client asking to use the sandbox", got)
			}
			if got := rt.looks.Load(); got > 1 {
				t.Errorf("daemon lookups = %d, want 1: a second lookup is the window the crash fits into", got)
			}
			if got := del.Header().Get(sandboxproxy.SandboxRestartedHeader); got != "" {
				t.Errorf("%s = %q, want it unset: this delete has no client to report to", sandboxproxy.SandboxRestartedHeader, got)
			}

			// The report is still there for the request that has a client behind it.
			base.daemonErr = runnerruntime.ErrSandboxNotRunning
			exec := httptest.NewRecorder()
			ExecProxyHandler(base, cfg, rec).ServeHTTP(exec, proxyTestRequest())

			if exec.Code != http.StatusConflict {
				t.Fatalf("exec status = %d, want 409: the delete spent the restart report", exec.Code)
			}
		})
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

// A daemon's answers are the guest's to choose. Following its redirect would point
// the runner, and on a 307 or 308 the exec body, at anything the runner can reach;
// relaying it would hand the choice to the API and its client.
func TestProxyHandlersRefuseDaemonRedirects(t *testing.T) {
	for name, newHandler := range wakingHandlers {
		for _, code := range []int{301, 302, 303, 307, 308} {
			t.Run(fmt.Sprintf("%s %d", name, code), func(t *testing.T) {
				var targetHits atomic.Int32
				target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
					targetHits.Add(1)
				}))
				defer target.Close()

				var daemonHits atomic.Int32
				daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					daemonHits.Add(1)
					http.Redirect(w, r, target.URL+r.URL.Path, code)
				}))
				defer daemon.Close()

				rec := httptest.NewRecorder()
				newHandler(&fakeRuntime{daemonURL: daemon.URL}, proxyTestConfig(), metrics.NewRunnerRecorder(false)).ServeHTTP(rec, proxyTestRequest())

				if rec.Code != http.StatusBadGateway {
					t.Fatalf("status = %d, want 502: %s", rec.Code, rec.Body.String())
				}
				if loc := rec.Header().Get("Location"); loc != "" {
					t.Errorf("Location relayed: %q", loc)
				}
				var payload struct {
					Error string `json:"error"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil || payload.Error != errDaemonRedirect.Error() {
					t.Errorf("body = %s, want error %q", rec.Body.String(), errDaemonRedirect.Error())
				}
				if got := daemonHits.Load(); got != 1 {
					t.Errorf("daemon hits = %d, want 1", got)
				}
				if got := targetHits.Load(); got != 0 {
					t.Fatalf("redirect target hits = %d, want 0: the redirect was followed", got)
				}
			})
		}
	}
}

// Only the runner may send its signals: the API drops its store row on
// X-Sandbox-Gone, and a client treats X-Sandbox-Restarted as lost state. The
// daemon's status and body still reach the caller.
func TestProxyHandlersStripDaemonSignalHeaders(t *testing.T) {
	const body = `{"error":"sandbox not found"}`
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sandboxproxy.MarkSandboxGone(w.Header())
		sandboxproxy.MarkSandboxRestarted(w.Header())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(body))
	}))
	defer daemon.Close()

	for name, newHandler := range wakingHandlers {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			newHandler(&fakeRuntime{daemonURL: daemon.URL}, proxyTestConfig(), metrics.NewRunnerRecorder(false)).ServeHTTP(rec, proxyTestRequest())

			if rec.Code != http.StatusNotFound || rec.Body.String() != body {
				t.Fatalf("got %d %q, want the daemon's 404 %q", rec.Code, rec.Body.String(), body)
			}
			for _, h := range []string{sandboxproxy.SandboxGoneHeader, sandboxproxy.SandboxRestartedHeader} {
				if got := rec.Header().Get(h); got != "" {
					t.Errorf("%s = %q relayed from the daemon", h, got)
				}
			}
		})
	}
}

// The daemon runs in the guest, so the fleet key the API puts on every request,
// and anything else a caller set, must stop at the runner. What the daemon may
// see besides Content-Type is what Go's client adds on its own.
func TestProxyHandlersForwardOnlyContentTypeToDaemon(t *testing.T) {
	allowed := map[string]bool{"Content-Type": true, "Content-Length": true, "Accept-Encoding": true, "User-Agent": true}
	type route struct {
		handler func(runnerruntime.Runtime, *config.Config, *metrics.RunnerRecorder) http.HandlerFunc
		request func() *http.Request
	}
	cases := map[string]route{"delete execution": {DeleteExecutionHandler, deleteExecutionRequest}}
	for name, h := range wakingHandlers {
		cases[name] = route{h, proxyTestRequest}
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var got, gotTrailer http.Header
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, gotTrailer = r.Header.Clone(), r.Trailer.Clone()
				w.Header().Set("Content-Type", "application/x-ndjson")
				_, _ = w.Write([]byte(exitEvent(1) + "\n"))
			}))
			defer daemon.Close()

			req := tc.request()
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Api-Key", "fleet-runner-key")
			req.Header.Set("X-Forwarded-For", "203.0.113.7")
			req.Header.Set("X-Injected", "1")
			req.Header.Set("Traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
			// Trailers only travel on a chunked body.
			req.ContentLength = -1
			req.Trailer = http.Header{"X-Injected-Trailer": nil}
			tc.handler(&fakeRuntime{daemonURL: daemon.URL}, proxyTestConfig(), metrics.NewRunnerRecorder(false)).ServeHTTP(httptest.NewRecorder(), req)

			if got == nil {
				t.Fatal("request never reached the daemon")
			}
			if ct := got.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			for h, v := range got {
				if !allowed[h] {
					t.Errorf("%s = %q reached the daemon", h, v)
				}
			}
			if len(gotTrailer) != 0 {
				t.Errorf("trailers %v reached the daemon", gotTrailer)
			}
		})
	}
}
