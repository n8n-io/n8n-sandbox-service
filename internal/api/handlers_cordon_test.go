package api

import (
	"context"
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
	store      store.SandboxStore
	control    *fakeSandboxControl
	runnerHits *atomic.Int32
	rec        *metrics.APIRecorder
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

	env.store = newSweepStore(t)
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

	reg := registry.New(45 * time.Second)
	reg.Upsert("runner-1", runner.URL, controlAddr, true, cordoned, 10, 0, 0)
	router, err := NewGatewayRouter(env.store, withRunnerTLS(&config.APIConfig{
		APIKeys:      map[string]struct{}{"admin-key": {}},
		RunnerAPIKey: "runner-key",
		MaxFileBytes: 1024,
	}), reg, env.rec, NewActivityRecorder(env.store, nil))
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
func TestSandboxProxyServesWhatACordonDoesNotRefuse(t *testing.T) {
	cases := map[string]struct {
		status       string
		cordoned     bool
		method, path string
	}{
		"running sandbox on a cordoned runner":    {"running", true, http.MethodPost, "/executions"},
		"stopped sandbox on an uncordoned runner": {"stopped", false, http.MethodPost, "/executions"},
		"execution delete on a cordoned runner":   {"stopped", true, http.MethodDelete, "/executions/exec-1"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			env := newCordonTestEnv(t, tc.status, tc.cordoned)

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

// The 404 tells the client the sandbox is gone, so it is only sent once the
// runner has deleted it. Until then the row stays and the client's retry, which
// a 503 asks for, tries again.
func TestSandboxProxyKeepsTheRowWhenTheCordonDeleteFails(t *testing.T) {
	env := newCordonTestEnv(t, "stopped", true)
	env.control.failDeletes(status.Error(codes.Unavailable, "runner going away"))

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

// n8n sends several requests at once. They queue on the sandbox lock behind the
// first one's delete and get the same 404, without a second delete.
func TestSandboxProxyDeletesOnceForConcurrentRequests(t *testing.T) {
	env := newCordonTestEnv(t, "stopped", true)
	release := make(chan struct{})
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
	time.Sleep(100 * time.Millisecond)
	close(release)
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
