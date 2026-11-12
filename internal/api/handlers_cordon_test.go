package api

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/metrics"
)

const cordonSandboxID = "66666666-6666-4666-8666-666666666666"

type cordonTestEnv struct {
	router     http.Handler
	cfg        *config.APIConfig
	registry   *registry.MemoryRegistry
	store      *lockCountingStore
	control    *fakeSandboxControl
	runnerHits *atomic.Int32
	rec        *metrics.APIRecorder
}

// lockCountingStore counts LockSandbox calls, which is how a test sees
// requests queue on a sandbox's lock.
type lockCountingStore struct {
	store.SandboxStore
	locks atomic.Int32
}

func (s *lockCountingStore) LockSandbox(ctx context.Context, id string) (func(), error) {
	s.locks.Add(1)
	return s.SandboxStore.LockSandbox(ctx, id)
}

// newCordonTestEnv serves one sandbox with the given status on runner-1, which
// the registry reports cordoned or not.
func newCordonTestEnv(t *testing.T, sandboxStatus string, cordoned bool) *cordonTestEnv {
	t.Helper()
	env := &cordonTestEnv{control: &fakeSandboxControl{}, runnerHits: &atomic.Int32{}, rec: metrics.NewAPIRecorder(true)}
	runner := newTestRunnerServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		env.runnerHits.Add(1)
		writer.WriteHeader(http.StatusOK)
	}))
	controlAddr := startFakeRunnerControl(t, env.control)

	env.store = &lockCountingStore{SandboxStore: newSweepStore(t)}
	now := time.Now().Unix()
	if err := env.store.Create(&store.SandboxRecord{
		ID:                    cordonSandboxID,
		Status:                sandboxStatus,
		CreatedAt:             now,
		LastActiveAt:          now,
		RunnerID:              "runner-1",
		RunnerHTTPBase:        runner.URL,
		RunnerControlGRPCAddr: controlAddr,
		TenantID:              store.AdminTenantID,
	}); err != nil {
		t.Fatalf("Create() failed: %v", err)
	}

	env.registry = registry.New(45 * time.Second)
	env.registry.Upsert("runner-1", runner.URL, controlAddr, true, cordoned, 10, 0, 0)
	env.cfg = withRunnerTLS(&config.APIConfig{
		APIKeys:        map[string]struct{}{"admin-key": {}},
		RunnerAPIKey:   "runner-key",
		MaxFileBytes:   1024,
		HeartbeatGrace: 45 * time.Second,
	})
	router, err := NewGatewayRouter(env.store, env.cfg, env.registry, env.rec, NewActivityRecorder(env.store, nil), nil)
	if err != nil {
		t.Fatalf("create gateway router: %v", err)
	}
	env.router = router
	return env
}

func (env *cordonTestEnv) serve(method, path string) *httptest.ResponseRecorder {
	body := ""
	if method == http.MethodPost {
		body = `{"command":"true"}`
	}
	req := httptest.NewRequest(method, "/sandboxes/"+cordonSandboxID+path, strings.NewReader(body))
	req.Header.Set("X-Api-Key", "admin-key")
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	return rr
}

// A request for a stopped sandbox on a cordoned runner deletes it instead of
// letting the runner wake it. The 404 is what makes n8n create the sandbox
// again, and placement then skips the cordoned runner.
func TestSandboxProxyDeletesAStoppedSandboxOnACordonedRunner(t *testing.T) {
	env := newCordonTestEnv(t, "stopped", true)

	if rr := env.serve(http.MethodPost, "/executions"); rr.Code != http.StatusNotFound {
		t.Fatalf("exec status = %d, want 404: %s", rr.Code, rr.Body.String())
	}
	if got := env.runnerHits.Load(); got != 0 {
		t.Errorf("runner hits = %d, want 0: the runner would have woken the sandbox", got)
	}
	if _, deleted := env.control.calls(); len(deleted) != 1 || deleted[0] != cordonSandboxID {
		t.Errorf("runner deletes = %v, want [%s]", deleted, cordonSandboxID)
	}
	if rec := mustGet(t, env.store, cordonSandboxID); rec != nil {
		t.Errorf("row still present: %+v", rec)
	}
	if rr := env.serve(http.MethodGet, ""); rr.Code != http.StatusNotFound {
		t.Errorf("GET status = %d, want 404", rr.Code)
	}
	if got := env.rec.SandboxOpCount(metrics.OpCordonDelete, true); got != 1 {
		t.Errorf("cordon_delete success metric = %v, want 1", got)
	}
}

// A cordon only stops wakes: a running sandbox is served, an uncordoned runner
// wakes as before, and deleting an execution never wakes a sandbox at all.
// Silence changes none of that: a runner whose heartbeats stall is still served.
func TestSandboxProxyServesWhatACordonDoesNotRefuse(t *testing.T) {
	cases := map[string]struct {
		status       string
		cordoned     bool
		silent       bool
		method, path string
	}{
		"running sandbox on a cordoned runner":          {"running", true, false, http.MethodPost, "/executions"},
		"stopped sandbox on an uncordoned runner":       {"stopped", false, false, http.MethodPost, "/executions"},
		"execution delete on a cordoned runner":         {"stopped", true, false, http.MethodDelete, "/executions/exec-1"},
		"running sandbox on a silent cordoned runner":   {"running", true, true, http.MethodPost, "/executions"},
		"stopped sandbox on a silent uncordoned runner": {"stopped", false, true, http.MethodPost, "/executions"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			env := newCordonTestEnv(t, tc.status, tc.cordoned)
			if tc.silent {
				env.cfg.HeartbeatGrace = time.Nanosecond
			}

			if rr := env.serve(tc.method, tc.path); rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want the runner's 200: %s", rr.Code, rr.Body.String())
			}
			if got := env.runnerHits.Load(); got != 1 {
				t.Errorf("runner hits = %d, want 1", got)
			}
			if _, deleted := env.control.calls(); len(deleted) != 0 {
				t.Errorf("runner deletes = %v, want none", deleted)
			}
			if rec := mustGet(t, env.store, cordonSandboxID); rec == nil {
				t.Error("row deleted")
			}
		})
	}
}

// The 404 tells the client the sandbox is gone, so a runner that answers has to
// have deleted it first. When its delete fails, the row stays and the client's
// retry, which a 503 asks for, tries again.
func TestSandboxProxyKeepsTheRowWhenTheCordonDeleteFails(t *testing.T) {
	env := newCordonTestEnv(t, "stopped", true)
	env.control.failDeletes(status.Error(codes.Internal, "delete failed"))

	if rr := env.serve(http.MethodPost, "/executions"); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rr.Code, rr.Body.String())
	}
	if rec := mustGet(t, env.store, cordonSandboxID); rec == nil {
		t.Fatal("row deleted although the runner still has the sandbox")
	}
	if got := env.rec.SandboxOpCount(metrics.OpCordonDelete, false); got != 1 {
		t.Errorf("cordon_delete error metric = %v, want 1", got)
	}

	env.control.failDeletes(nil)
	if rr := env.serve(http.MethodPost, "/executions"); rr.Code != http.StatusNotFound {
		t.Fatalf("retry status = %d, want 404: %s", rr.Code, rr.Body.String())
	}
}

// A cordoned runner that can't be reached is in its reimage or already removed,
// and the sandbox with it. Waiting for its delete would answer 503 for as long
// as the row stays, so the row goes and the client gets its 404.
func TestSandboxProxyDeletesTheRowWhenTheCordonedRunnerIsUnreachable(t *testing.T) {
	env := newCordonTestEnv(t, "stopped", true)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadAddr := lis.Addr().String()
	_ = lis.Close()
	env.registry.Upsert("runner-1", "https://"+deadAddr, deadAddr, true, true, 10, 0, 0)

	if rr := env.serve(http.MethodPost, "/executions"); rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rr.Code, rr.Body.String())
	}
	if rec := mustGet(t, env.store, cordonSandboxID); rec != nil {
		t.Errorf("row still present: %+v", rec)
	}
	if got := env.rec.SandboxOpCount(metrics.OpCordonDelete, true); got != 1 {
		t.Errorf("cordon_delete success metric = %v, want 1", got)
	}
}

// Silence in the registry doesn't prove a runner is gone: heartbeats stall for
// other reasons too. So a silent cordoned runner is still asked, and one that
// answers deletes the sandbox itself.
func TestSandboxProxyStillAsksASilentCordonedRunnerToDelete(t *testing.T) {
	env := newCordonTestEnv(t, "stopped", true)
	env.cfg.HeartbeatGrace = time.Nanosecond

	if rr := env.serve(http.MethodPost, "/executions"); rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rr.Code, rr.Body.String())
	}
	if _, deleted := env.control.calls(); len(deleted) != 1 {
		t.Errorf("runner deletes = %v, want exactly one", deleted)
	}
}

// A silent cordoned runner is usually gone, so it gets only a short time to
// answer before the row goes without it.
func TestSandboxProxyGivesUpOnASilentCordonedRunnerThatDoesNotAnswer(t *testing.T) {
	env := newCordonTestEnv(t, "stopped", true)
	env.cfg.HeartbeatGrace = time.Nanosecond
	shrinkSilentRunnerDeleteBudget(t, 50*time.Millisecond)
	env.control.deleteHook = func(ctx context.Context) { <-ctx.Done() }

	if rr := env.serve(http.MethodPost, "/executions"); rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rr.Code, rr.Body.String())
	}
	if rec := mustGet(t, env.store, cordonSandboxID); rec != nil {
		t.Errorf("row still present: %+v", rec)
	}
}

// A cordoned runner that still heartbeats has the sandbox, so a delete that
// only times out keeps the row.
func TestSandboxProxyKeepsTheRowWhenAHeartbeatingCordonedRunnerTimesOut(t *testing.T) {
	env := newCordonTestEnv(t, "stopped", true)
	shrinkRunnerLifecycleBudget(t, 50*time.Millisecond)
	env.control.deleteHook = func(ctx context.Context) { <-ctx.Done() }

	if rr := env.serve(http.MethodPost, "/executions"); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rr.Code, rr.Body.String())
	}
	if rec := mustGet(t, env.store, cordonSandboxID); rec == nil {
		t.Fatal("row deleted although the runner still has the sandbox")
	}
}

func shrinkSilentRunnerDeleteBudget(t *testing.T, budget time.Duration) {
	t.Helper()
	prev := silentRunnerDeleteBudget
	silentRunnerDeleteBudget = budget
	t.Cleanup(func() { silentRunnerDeleteBudget = prev })
}

// The delete goes over control gRPC, so a row without the runner's HTTP base is
// still deleted rather than answered with 502.
func TestSandboxProxyDeletesWithoutTheRunnerHTTPBase(t *testing.T) {
	env := newCordonTestEnv(t, "stopped", true)
	rec := mustGet(t, env.store, cordonSandboxID)
	if err := env.store.Delete(cordonSandboxID); err != nil {
		t.Fatalf("Delete() failed: %v", err)
	}
	rec.RunnerHTTPBase = ""
	if err := env.store.Create(rec); err != nil {
		t.Fatalf("Create() failed: %v", err)
	}

	if rr := env.serve(http.MethodPost, "/executions"); rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rr.Code, rr.Body.String())
	}
	if _, deleted := env.control.calls(); len(deleted) != 1 {
		t.Errorf("runner deletes = %v, want exactly one", deleted)
	}
}

// n8n sends several requests at once. They queue on the sandbox lock behind the
// first one's delete and get the same 404, without a second delete.
func TestSandboxProxyDeletesOnceForConcurrentRequests(t *testing.T) {
	env := newCordonTestEnv(t, "stopped", true)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseDelete := func() { releaseOnce.Do(func() { close(release) }) }
	// Also on failure, so no request stays blocked in the fake runner.
	t.Cleanup(releaseDelete)
	env.control.deleteHook = func(context.Context) { <-release }

	const requests = 4
	statuses := make([]int, requests)
	var wg sync.WaitGroup
	for request := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[request] = env.serve(http.MethodPost, "/executions").Code
		}()
	}
	// Each request has read the stopped row and holds the lock in the delete or
	// waits for it, so a delete without the lock would show up as a second one.
	waitFor(t, "every request to reach the sandbox lock", func() bool { return env.store.locks.Load() == requests })
	releaseDelete()
	wg.Wait()

	for request, code := range statuses {
		if code != http.StatusNotFound {
			t.Errorf("request %d status = %d, want 404", request, code)
		}
	}
	if _, deleted := env.control.calls(); len(deleted) != 1 {
		t.Errorf("runner deletes = %v, want exactly one", deleted)
	}
}
