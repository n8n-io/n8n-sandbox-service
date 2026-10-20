package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func mintTenantKey(t *testing.T, router http.Handler, body string) createTenantResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/tenants", strings.NewReader(body))
	req.Header.Set("X-Api-Key", "admin-key")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("mint tenant: %d %s", rr.Code, rr.Body.String())
	}
	var created createTenantResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Key == nil || created.Key.APIKey == "" {
		t.Fatalf("expected plaintext key, got %+v", created)
	}
	return created
}

func postCreateSandbox(t *testing.T, router http.Handler, apiKey, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/sandboxes", strings.NewReader(body))
	req.Header.Set("X-Api-Key", apiKey)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

// A failed create must be diagnosable from the canonical event: the step that
// failed carries the same trace id, so both lines come back from one query.
func TestCreateSandboxFailureIsTraceCorrelated(t *testing.T) {
	logs := captureLogs(t)
	router, _ := newTestGateway(t, "admin-key")

	// No runner ever registered, so the create fails at runner selection.
	if rr := postCreateSandbox(t, router, "admin-key", ""); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("create without runners: expected %d, got %d body=%s", http.StatusServiceUnavailable, rr.Code, rr.Body.String())
	}

	traceIDs := map[string]string{}
	for _, event := range logs() {
		msg, _ := event["msg"].(string)
		traceIDs[msg], _ = event["trace_id"].(string)
	}

	failure := traceIDs["create sandbox failed: no eligible runners"]
	if len(failure) != 32 {
		t.Fatalf("failure event trace_id = %q, want a 32 hex character id", failure)
	}
	if request := traceIDs["request"]; request != failure {
		t.Fatalf("request event trace_id = %q, want the failure event's %q", request, failure)
	}
}

func TestCreateSandboxClientIDCrossTenantConflict(t *testing.T) {
	router, s := newTestGateway(t, "admin-key")
	a := mintTenantKey(t, router, `{"name":"a"}`)
	b := mintTenantKey(t, router, `{"name":"b"}`)

	sid := "11111111-1111-4111-8111-111111111111"
	if err := s.Create(&store.SandboxRecord{
		ID: sid, Status: "running", CreatedAt: 1, LastActiveAt: time.Now().Unix(),
		TenantID: a.Tenant.ID, RunnerHTTPBase: "http://127.0.0.1:9",
	}); err != nil {
		t.Fatalf("seed sandbox: %v", err)
	}

	rr := postCreateSandbox(t, router, b.Key.APIKey, `{"id":"`+sid+`"}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("cross-tenant create: expected %d, got %d body=%s", http.StatusConflict, rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "sandbox id unavailable") {
		t.Fatalf("expected unavailable message, got %s", rr.Body.String())
	}

	// Owner still has the sandbox; foreign claim must not delete it.
	got, err := s.Get(sid)
	if err != nil || got == nil || got.TenantID != a.Tenant.ID {
		t.Fatalf("owner sandbox altered: got=%+v err=%v", got, err)
	}
}

func TestCreateSandboxClientIDAdminOwnedUnavailableToTenant(t *testing.T) {
	router, s := newTestGateway(t, "admin-key")
	tenant := mintTenantKey(t, router, `{"name":"t"}`)

	sid := "22222222-2222-4222-8222-222222222222"
	if err := s.Create(&store.SandboxRecord{
		ID: sid, Status: "running", CreatedAt: 1, LastActiveAt: time.Now().Unix(),
		TenantID: store.AdminTenantID, RunnerHTTPBase: "http://127.0.0.1:9",
	}); err != nil {
		t.Fatalf("seed sandbox: %v", err)
	}

	rr := postCreateSandbox(t, router, tenant.Key.APIKey, `{"id":"`+sid+`"}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("tenant claiming admin id: expected %d, got %d body=%s", http.StatusConflict, rr.Code, rr.Body.String())
	}
}

func TestCreateSandboxClientIDReuseOwn(t *testing.T) {
	router, s := newTestGateway(t, "admin-key")
	tenant := mintTenantKey(t, router, `{"name":"t"}`)

	sid := "33333333-3333-4333-8333-333333333333"
	now := time.Now().Unix()
	if err := s.Create(&store.SandboxRecord{
		ID: sid, Status: "running", CreatedAt: now, LastActiveAt: now,
		TenantID: tenant.Tenant.ID, RunnerHTTPBase: "http://127.0.0.1:9",
	}); err != nil {
		t.Fatalf("seed sandbox: %v", err)
	}

	rr := postCreateSandbox(t, router, tenant.Key.APIKey, `{"id":"`+sid+`"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("reuse own id: expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	var resp SandboxResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != sid {
		t.Fatalf("reuse id: got %q want %q", resp.ID, sid)
	}
}

func TestCreateSandboxClientIDReuseAtQuota(t *testing.T) {
	router, s := newTestGateway(t, "admin-key")
	tenant := mintTenantKey(t, router, `{"name":"capped","max_sandboxes":1}`)

	sid := "44444444-4444-4444-8444-444444444444"
	now := time.Now().Unix()
	if err := s.Create(&store.SandboxRecord{
		ID: sid, Status: "running", CreatedAt: now, LastActiveAt: now,
		TenantID: tenant.Tenant.ID, RunnerHTTPBase: "http://127.0.0.1:9",
	}); err != nil {
		t.Fatalf("seed sandbox: %v", err)
	}

	// Reconnect must succeed even though CountByTenant == max_sandboxes.
	rr := postCreateSandbox(t, router, tenant.Key.APIKey, `{"id":"`+sid+`"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("reuse at quota: expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
}

func TestCreateSandboxQuotaExceeded(t *testing.T) {
	router, s := newTestGateway(t, "admin-key")
	tenant := mintTenantKey(t, router, `{"name":"capped","max_sandboxes":1}`)

	if err := s.Create(&store.SandboxRecord{
		ID: "55555555-5555-4555-8555-555555555555", Status: "running",
		CreatedAt: 1, LastActiveAt: 1, TenantID: tenant.Tenant.ID,
	}); err != nil {
		t.Fatalf("seed sandbox: %v", err)
	}

	// Quota is checked before runner pick, so this returns 403 without runners.
	rr := postCreateSandbox(t, router, tenant.Key.APIKey, "")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("create at quota: expected %d, got %d body=%s", http.StatusForbidden, rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "quota exceeded") {
		t.Fatalf("expected quota message, got %s", rr.Body.String())
	}

	newID := "66666666-6666-4666-8666-666666666666"
	rrID := postCreateSandbox(t, router, tenant.Key.APIKey, `{"id":"`+newID+`"}`)
	if rrID.Code != http.StatusForbidden {
		t.Fatalf("create new id at quota: expected %d, got %d body=%s", http.StatusForbidden, rrID.Code, rrID.Body.String())
	}
}

// newIdleTestGateway is newTestGateway with idle windows and a runner serving fake.
func newIdleTestGateway(t *testing.T, adminKey string, fake *fakeSandboxControl) (http.Handler, store.SandboxStore, *config.APIConfig) {
	t.Helper()
	routers, s, cfg := newIdleTestGatewayPods(t, adminKey, fake, 1)
	return routers[0], s, cfg
}

// newIdleTestGatewayPods is newIdleTestGateway with one router per API pod,
// all sharing one store.
func newIdleTestGatewayPods(t *testing.T, adminKey string, fake *fakeSandboxControl, pods int) ([]http.Handler, store.SandboxStore, *config.APIConfig) {
	t.Helper()
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	cfg := idleSweepConfig()
	cfg.APIKeys = map[string]struct{}{adminKey: {}}
	cfg.MaxFileBytes = 1024
	cfg.DefaultMaxSandboxes = 50

	reg := registry.New(45 * time.Second)
	reg.Upsert("runner-1", "https://127.0.0.1:9", startFakeRunnerControl(t, fake), true, 10, 0, 0)

	routers := make([]http.Handler, pods)
	for i := range routers {
		routers[i], err = NewGatewayRouter(s, cfg, reg, metrics.NewAPIRecorder(false))
		if err != nil {
			t.Fatalf("create gateway router: %v", err)
		}
	}
	return routers, s, cfg
}

// A burst of creates for one tenant leaves exactly its quota, on one API pod or
// spread over two. Each pod counts its own creates in progress, so on one pod
// no create past the quota reaches the runner. Across pods each one lets the
// quota through, so saving has to refuse the extra ones, and they must be
// deleted again on the runner.
func TestCreateSandboxQuotaHoldsUnderConcurrentCreates(t *testing.T) {
	const quota, creates = 2, 6
	for _, pods := range []int{1, 2} {
		t.Run(fmt.Sprintf("pods=%d", pods), func(t *testing.T) {
			reachRunner := pods * quota
			// Hold every create at the runner until each create has either
			// arrived there or returned without reaching it, so none is saved
			// before the last one has been checked. Until then a create that
			// returns never reached the runner, as those are held.
			var settled, arrived atomic.Int32
			allSettled := make(chan struct{})
			release := sync.OnceFunc(func() { close(allSettled) })
			settle := func() {
				if settled.Add(1) == creates {
					release()
				}
			}
			fake := &fakeSandboxControl{createHook: func(context.Context) {
				arrived.Add(1)
				settle()
				select {
				case <-allSettled:
				case <-time.After(10 * time.Second):
				}
			}}
			routers, s, _ := newIdleTestGatewayPods(t, "admin-key", fake, pods)
			tenant := mintTenantKey(t, routers[0], fmt.Sprintf(`{"name":"burst","max_sandboxes":%d}`, quota))

			responses := make([]*httptest.ResponseRecorder, creates)
			var wg sync.WaitGroup
			for i := range creates {
				wg.Go(func() {
					responses[i] = postCreateSandbox(t, routers[i%pods], tenant.Key.APIKey, "")
					settle()
				})
			}
			wg.Wait()

			if n := arrived.Load(); int(n) != reachRunner {
				t.Fatalf("runner creates = %d, want %d: the quota on each of %d pods", n, reachRunner, pods)
			}
			created := map[string]bool{}
			for _, rr := range responses {
				switch rr.Code {
				case http.StatusCreated:
					var resp SandboxResponse
					if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
						t.Fatalf("decode: %v", err)
					}
					created[resp.ID] = true
				case http.StatusForbidden:
					if !strings.Contains(rr.Body.String(), "quota exceeded") {
						t.Fatalf("refused create: body=%s, want the quota message", rr.Body.String())
					}
				default:
					t.Fatalf("create: got %d body=%s, want %d or %d", rr.Code, rr.Body.String(), http.StatusCreated, http.StatusForbidden)
				}
			}
			if len(created) != quota {
				t.Fatalf("%d of %d creates succeeded, want exactly the quota %d", len(created), creates, quota)
			}

			stored, err := s.ListByTenant(tenant.Tenant.ID)
			if err != nil || len(stored) != quota {
				t.Fatalf("stored %d sandboxes err=%v, want %d", len(stored), err, quota)
			}
			for _, rec := range stored {
				if !created[rec.ID] {
					t.Fatalf("stored sandbox %s was not answered with 201", rec.ID)
				}
			}
			_, deleted := fake.calls()
			if len(deleted) != reachRunner-quota {
				t.Fatalf("runner deletes = %v, want the %d creates refused when saving", deleted, reachRunner-quota)
			}
			for _, id := range deleted {
				if created[id] {
					t.Fatalf("runner deleted %s, which was answered with 201", id)
				}
			}
		})
	}
}

// However a create ends, its slot among the tenant's creates in progress is
// freed, or the tenant loses that slot on the pod for good. With a quota of
// one, a lost slot shows as a 403 on the next create.
func TestCreateSandboxFreesQuotaSlotWhenCreateEnds(t *testing.T) {
	for _, tc := range []struct {
		name string
		want int
		// Makes the first create end with want; returns what undoes it.
		fail func(f *fakeSandboxControl, s store.SandboxStore, tenantID string) (undo func())
	}{
		{name: "created", want: http.StatusCreated},
		{name: "runner error", want: http.StatusInternalServerError, fail: func(f *fakeSandboxControl, _ store.SandboxStore, _ string) func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.createErr = status.Error(codes.Internal, "runner broke")
			return func() {
				f.mu.Lock()
				defer f.mu.Unlock()
				f.createErr = nil
			}
		}},
		{name: "egress not applied", want: http.StatusBadGateway, fail: func(f *fakeSandboxControl, _ store.SandboxStore, _ string) func() {
			f.applied = func(string) string { return `{"egress":"none"}` }
			return func() { f.applied = nil }
		}},
		{name: "refused when saving", want: http.StatusForbidden, fail: func(f *fakeSandboxControl, s store.SandboxStore, tenantID string) func() {
			// Saved meanwhile, as by another API pod.
			const other = "12121212-1212-4212-8212-121212121212"
			f.createHook = func(context.Context) {
				if err := s.Create(&store.SandboxRecord{ID: other, Status: "running", TenantID: tenantID}); err != nil {
					t.Errorf("plant other pod's sandbox: %v", err)
				}
			}
			return func() { f.createHook = nil }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeSandboxControl{}
			router, s, _ := newIdleTestGateway(t, "admin-key", fake)
			tenant := mintTenantKey(t, router, `{"name":"capped","max_sandboxes":1}`)

			undo := func() {}
			if tc.fail != nil {
				undo = tc.fail(fake, s, tenant.Tenant.ID)
			}
			if rr := postCreateSandbox(t, router, tenant.Key.APIKey, ""); rr.Code != tc.want {
				t.Fatalf("first create: got %d body=%s, want %d", rr.Code, rr.Body.String(), tc.want)
			}
			undo()
			stored, err := s.ListByTenant(tenant.Tenant.ID)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			for _, rec := range stored {
				if err := s.Delete(rec.ID); err != nil {
					t.Fatalf("delete %s: %v", rec.ID, err)
				}
			}

			if rr := postCreateSandbox(t, router, tenant.Key.APIKey, ""); rr.Code != http.StatusCreated {
				t.Fatalf("create after the first ended: got %d body=%s, want %d: its slot was not freed", rr.Code, rr.Body.String(), http.StatusCreated)
			}
		})
	}
}

// createErrStore returns err from every sandbox Create, after saving the row
// when save is set.
type createErrStore struct {
	store.SandboxStore
	save bool
	err  error
}

func (s createErrStore) Create(r *store.SandboxRecord) error {
	if s.save {
		if err := s.SandboxStore.Create(r); err != nil {
			return err
		}
	}
	return s.err
}

// A save that errored may still have saved the row, say when the reply to its
// commit was lost. Unless the store refused the row, its slot is freed as
// saved, so a reserve reading meanwhile still counts it.
func TestCreateSandboxCountsUnclearSaveAsSaved(t *testing.T) {
	for _, tc := range []struct {
		name      string
		save      bool
		err       error
		wantCode  int
		wantSaved int64
	}{
		{name: "saved, reply lost", save: true, err: errors.New("connection reset"), wantCode: http.StatusOK, wantSaved: 1},
		{name: "refused at quota", err: store.ErrTenantQuotaExceeded, wantCode: http.StatusForbidden},
		{name: "tenant gone", err: store.ErrTenantNotFound, wantCode: http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSweepStore(t)
			const tenantID = "13131313-1313-4313-8313-131313131313"
			if err := s.CreateTenant(&store.Tenant{ID: tenantID, Name: "t", MaxSandboxes: 2}); err != nil {
				t.Fatalf("create tenant: %v", err)
			}
			reg := registry.New(45 * time.Second)
			reg.Upsert("runner-1", "https://127.0.0.1:9", startFakeRunnerControl(t, &fakeSandboxControl{}), true, 10, 0, 0)
			q := newTenantQuota(s.CountByTenant)
			// Keeps the tenant's entry, and so its counts, after the create.
			hold, _, _ := q.reserve(tenantID, 2)
			defer hold(false)

			h := handleCreateSandbox(createErrStore{SandboxStore: s, save: tc.save, err: tc.err}, reg, idleSweepConfig(), metrics.NewAPIRecorder(false), q)
			req := httptest.NewRequest(http.MethodPost, "/sandboxes", nil)
			req = req.WithContext(withAuthIdentity(req.Context(), authIdentity{Role: roleTenant, TenantID: tenantID}))
			rr := httptest.NewRecorder()
			h(rr, req)
			if rr.Code != tc.wantCode {
				t.Fatalf("create: got %d body=%s, want %d", rr.Code, rr.Body.String(), tc.wantCode)
			}
			if e := q.tenants[tenantID]; e == nil || e.inProgress != 1 || e.saved != tc.wantSaved {
				t.Fatalf("tenant entry = %+v, want only the held create in progress and %d saved", e, tc.wantSaved)
			}
		})
	}
}

// blockingWriter holds the response body until unblock is closed.
type blockingWriter struct {
	*httptest.ResponseRecorder
	writing func()
	unblock chan struct{}
}

func (b *blockingWriter) Write(p []byte) (int, error) {
	b.writing()
	<-b.unblock
	return b.ResponseRecorder.Write(p)
}

// A saved create frees its slot right away, not once its response is written.
// If it held the slot until then, the sandbox would count both as saved and in
// progress, and the second create here would be refused below the quota.
func TestCreateSandboxFreesQuotaSlotOnceSaved(t *testing.T) {
	router, _, _ := newIdleTestGateway(t, "admin-key", &fakeSandboxControl{})
	tenant := mintTenantKey(t, router, `{"name":"capped","max_sandboxes":2}`)

	writing := make(chan struct{})
	w := &blockingWriter{
		ResponseRecorder: httptest.NewRecorder(),
		writing:          sync.OnceFunc(func() { close(writing) }),
		unblock:          make(chan struct{}),
	}
	unblock := sync.OnceFunc(func() { close(w.unblock) })
	defer unblock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodPost, "/sandboxes", nil)
		req.Header.Set("X-Api-Key", tenant.Key.APIKey)
		router.ServeHTTP(w, req)
	}()
	select {
	case <-writing:
	case <-done:
		t.Fatalf("first create returned without writing a body: %d", w.Code)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the first create's response")
	}

	if rr := postCreateSandbox(t, router, tenant.Key.APIKey, ""); rr.Code != http.StatusCreated {
		t.Fatalf("create while the first one's response is written: got %d body=%s, want %d", rr.Code, rr.Body.String(), http.StatusCreated)
	}
	unblock()
	<-done
	if w.Code != http.StatusCreated {
		t.Fatalf("first create: got %d, want %d", w.Code, http.StatusCreated)
	}
}

// A client hanging up mid-create must not cancel the runner call: the runner
// finishes the create either way, and without a store row the sandbox would
// hold a slot that quota and the idle sweeper cannot see.
func TestCreateSandboxClientDisconnectStillStoresRow(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	// Release on every exit path so a failed assertion cannot leave the fake blocked.
	releaseRunner := sync.OnceFunc(func() { close(release) })
	defer releaseRunner()
	fake := &fakeSandboxControl{createHook: func(context.Context) {
		close(started)
		<-release
	}}
	router, s, _ := newIdleTestGateway(t, "admin-key", fake)

	ctx, disconnect := context.WithCancel(context.Background())
	defer disconnect()
	req := httptest.NewRequest(http.MethodPost, "/sandboxes", nil).WithContext(ctx)
	req.Header.Set("X-Api-Key", "admin-key")
	rr := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		router.ServeHTTP(rr, req)
	}()

	// If the create returns without reaching the runner, started never closes;
	// fail rather than hang.
	select {
	case <-started:
	case <-done:
		t.Fatalf("create returned before reaching the runner: status %d body=%s", rr.Code, rr.Body.String())
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the runner create to start")
	}
	disconnect()
	releaseRunner()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("create did not finish after the runner was released")
	}

	if rr.Code != http.StatusCreated {
		t.Fatalf("create after disconnect: expected %d, got %d body=%s", http.StatusCreated, rr.Code, rr.Body.String())
	}
	var resp SandboxResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rec, err := s.Get(resp.ID); err != nil || rec == nil {
		t.Fatalf("stored row = %+v err=%v, want the created sandbox", rec, err)
	}
	if _, deleted := fake.calls(); len(deleted) != 0 {
		t.Fatalf("runner deletes = %v, want none", deleted)
	}
}

// createWithTenantDeletedMidCreate posts a create and deletes the tenant while
// the runner is still creating, so storing the row fails and the handler has to
// delete the sandbox on the runner again. inCreate, if set, runs inside the
// runner create first. Asserts the 409 for the lost tenant.
func createWithTenantDeletedMidCreate(t *testing.T, fake *fakeSandboxControl, inCreate func(context.Context)) {
	t.Helper()
	var s store.SandboxStore
	var tenantID string
	fake.createHook = func(ctx context.Context) {
		if inCreate != nil {
			inCreate(ctx)
		}
		if err := s.DeleteTenant(tenantID); err != nil {
			t.Errorf("delete tenant: %v", err)
		}
	}
	var router http.Handler
	router, s, _ = newIdleTestGateway(t, "admin-key", fake)
	tenant := mintTenantKey(t, router, `{"name":"t"}`)
	tenantID = tenant.Tenant.ID

	if rr := postCreateSandbox(t, router, tenant.Key.APIKey, ""); rr.Code != http.StatusConflict {
		t.Fatalf("create with tenant deleted mid-create: expected %d, got %d body=%s", http.StatusConflict, rr.Code, rr.Body.String())
	}
}

// If storing the row fails after the runner created the sandbox, the handler
// must delete it on the runner, or it holds a slot nothing can reclaim. That
// delete needs its own deadline: the create may have used up most of its own.
func TestCreateSandboxStoreFailureDeletesRunnerSandbox(t *testing.T) {
	const createTakes = 200 * time.Millisecond
	remaining := make(chan time.Duration, 1)
	fake := &fakeSandboxControl{deleteHook: func(ctx context.Context) {
		deadline, _ := ctx.Deadline()
		remaining <- time.Until(deadline)
	}}
	// Long enough that a delete reusing the create's deadline is measurably short.
	createWithTenantDeletedMidCreate(t, fake, func(context.Context) { time.Sleep(createTakes) })

	if _, deleted := fake.calls(); len(deleted) != 1 {
		t.Fatalf("runner deletes = %v, want the compensating delete", deleted)
	}
	if got := <-remaining; got < runnerCreateBudget-createTakes/2 {
		t.Fatalf("delete reached the runner with %v left, want about %v: it ran on the create's leftover budget", got, runnerCreateBudget)
	}
}

// If that delete fails, the sandbox is now untracked; the failure must be
// logged, not swallowed.
func TestCreateSandboxStoreFailureLogsFailedRunnerDelete(t *testing.T) {
	logs := captureLogs(t)
	fake := &fakeSandboxControl{}
	fake.failDeletes(status.Error(codes.Unavailable, "runner busy"))
	createWithTenantDeletedMidCreate(t, fake, nil)

	for _, event := range logs() {
		if msg, _ := event["msg"].(string); strings.HasPrefix(msg, "create sandbox failed: compensating runner delete") {
			if event["sandbox_id"] == nil || !strings.Contains(event["error"].(string), "runner busy") {
				t.Fatalf("delete failure event = %v, want sandbox_id and the runner error", event)
			}
			return
		}
	}
	t.Fatal("no log event for the failed compensating delete")
}

func TestCreateSandboxPersistsEphemeral(t *testing.T) {
	router, s, _ := newIdleTestGateway(t, "admin-key", &fakeSandboxControl{})

	rr := postCreateSandbox(t, router, "admin-key", `{"ephemeral":true}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create ephemeral: expected %d, got %d body=%s", http.StatusCreated, rr.Code, rr.Body.String())
	}
	var resp SandboxResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Ephemeral {
		t.Fatalf("create response ephemeral = false, want true: %s", rr.Body.String())
	}
	rec, err := s.Get(resp.ID)
	if err != nil || rec == nil || !rec.Ephemeral {
		t.Fatalf("stored row = %+v err=%v, want ephemeral", rec, err)
	}

	// Default stays false and is reported as such.
	rr = postCreateSandbox(t, router, "admin-key", "")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create default: expected %d, got %d body=%s", http.StatusCreated, rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"ephemeral":false`) {
		t.Fatalf("default create should report ephemeral false: %s", rr.Body.String())
	}
}

// The egress mode reaches the runner in create_options_json, is stored, and is echoed
// on create and on a reconnect to the same id; an unknown value is refused
// before anything is created.
func TestCreateSandboxEgress(t *testing.T) {
	ctl := &fakeSandboxControl{}
	router, s, _ := newIdleTestGateway(t, "admin-key", ctl)

	sid := "abababab-abab-4bab-8bab-abababababab"
	rr := postCreateSandbox(t, router, "admin-key", `{"id":"`+sid+`","egress":"none"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create egress none: expected %d, got %d body=%s", http.StatusCreated, rr.Code, rr.Body.String())
	}
	var resp SandboxResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Egress != "none" {
		t.Fatalf("create response egress = %q, want none: %s", resp.Egress, rr.Body.String())
	}
	if rec, err := s.Get(sid); err != nil || rec == nil || rec.Egress != "none" {
		t.Fatalf("stored row = %+v err=%v, want egress none", rec, err)
	}
	ctl.mu.Lock()
	sent := append([]string(nil), ctl.createOptionsJSON...)
	ctl.mu.Unlock()
	if len(sent) != 1 || sent[0] != `{"egress":"none"}` {
		t.Fatalf("runner create_options_json = %q, want [{\"egress\":\"none\"}]", sent)
	}

	// Reconnecting to the id reports the mode it was created with.
	rr = postCreateSandbox(t, router, "admin-key", `{"id":"`+sid+`","egress":"none"}`)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"egress":"none"`) {
		t.Fatalf("reconnect: got %d body=%s, want 200 with egress none", rr.Code, rr.Body.String())
	}

	// Default is public, and is sent to the runner explicitly.
	rr = postCreateSandbox(t, router, "admin-key", "")
	if rr.Code != http.StatusCreated || !strings.Contains(rr.Body.String(), `"egress":"public"`) {
		t.Fatalf("default create: got %d body=%s, want 201 with egress public", rr.Code, rr.Body.String())
	}
	ctl.mu.Lock()
	last := ctl.createOptionsJSON[len(ctl.createOptionsJSON)-1]
	ctl.mu.Unlock()
	if last != `{"egress":"public"}` {
		t.Fatalf("default create_options_json = %q, want {\"egress\":\"public\"}", last)
	}

	before, _ := s.Count()
	rr = postCreateSandbox(t, router, "admin-key", `{"egress":"allow-all"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid egress: expected %d, got %d body=%s", http.StatusBadRequest, rr.Code, rr.Body.String())
	}
	if after, _ := s.Count(); after != before {
		t.Fatalf("invalid egress created a sandbox: count %d -> %d", before, after)
	}
}

// Egress is fixed at creation, so a reconnect that names a different mode is
// refused rather than answered with a sandbox in the other mode; one that names
// no mode gets whatever the sandbox has, and an empty mode is the default,
// public, not left out. Both places that answer a reconnect enforce this: the
// lookup before the create, and the fallback after a store insert lost a race
// to a concurrent create of the same id.
func TestCreateSandboxReconnectRefusesOtherEgress(t *testing.T) {
	const sid = "cdcdcdcd-cdcd-4cdc-8cdc-cdcdcdcdcdcd"
	for _, tc := range []struct {
		name     string
		existing string // mode the sandbox was created with
		body     string
		want     int
		egress   string // mode a 200 must report
	}{
		{name: "same mode", existing: "public", body: `{"id":"` + sid + `","egress":"public"}`, want: http.StatusOK, egress: "public"},
		{name: "no mode", existing: "none", body: `{"id":"` + sid + `"}`, want: http.StatusOK, egress: "none"},
		{name: "empty mode", existing: "none", body: `{"id":"` + sid + `","egress":""}`, want: http.StatusConflict},
		{name: "other mode", existing: "public", body: `{"id":"` + sid + `","egress":"none"}`, want: http.StatusConflict},
	} {
		check := func(t *testing.T, rr *httptest.ResponseRecorder) {
			t.Helper()
			if rr.Code != tc.want {
				t.Fatalf("got %d body=%s, want %d", rr.Code, rr.Body.String(), tc.want)
			}
			if tc.egress != "" && !strings.Contains(rr.Body.String(), `"egress":"`+tc.egress+`"`) {
				t.Fatalf("body=%s, want egress %s", rr.Body.String(), tc.egress)
			}
		}
		t.Run("existing/"+tc.name, func(t *testing.T) {
			ctl := &fakeSandboxControl{}
			router, _, _ := newIdleTestGateway(t, "admin-key", ctl)
			if rr := postCreateSandbox(t, router, "admin-key", `{"id":"`+sid+`","egress":"`+tc.existing+`"}`); rr.Code != http.StatusCreated {
				t.Fatalf("create: got %d body=%s", rr.Code, rr.Body.String())
			}
			check(t, postCreateSandbox(t, router, "admin-key", tc.body))
		})
		t.Run("raced/"+tc.name, func(t *testing.T) {
			ctl := &fakeSandboxControl{}
			router, s, _ := newIdleTestGateway(t, "admin-key", ctl)
			// A concurrent create of the same id wins while this one is at the runner.
			ctl.createHook = func(context.Context) {
				if err := s.Create(&store.SandboxRecord{ID: sid, Status: "running", TenantID: store.AdminTenantID, Egress: tc.existing}); err != nil {
					t.Errorf("plant existing row: %v", err)
				}
			}
			check(t, postCreateSandbox(t, router, "admin-key", tc.body))
			if _, deleted := ctl.calls(); len(deleted) != 1 {
				t.Fatalf("runner deletes = %v, want the losing create rolled back", deleted)
			}
		})
	}
}

// A sandbox is only tracked when the runner confirms the egress asked for. A
// runner from before egress modes confirms nothing and applied public, so it can
// still serve public sandboxes but not none; a runner reporting a different mode
// is refused too. Either way the runner create is undone and nothing is stored.
func TestCreateSandboxRefusesUnconfirmedEgress(t *testing.T) {
	for _, tc := range []struct {
		name    string
		applied func(string) string
		body    string
		want    int
	}{
		{name: "old runner, none", applied: func(string) string { return "" }, body: `{"egress":"none"}`, want: http.StatusBadGateway},
		{name: "old runner, public", applied: func(string) string { return "" }, body: `{"egress":"public"}`, want: http.StatusCreated},
		{name: "runner applied public for none", applied: func(string) string { return `{"egress":"public"}` }, body: `{"egress":"none"}`, want: http.StatusBadGateway},
		{name: "runner applied none for public", applied: func(string) string { return `{"egress":"none"}` }, body: `{"egress":"public"}`, want: http.StatusBadGateway},
		{name: "unreadable echo", applied: func(string) string { return `{"egress":` }, body: `{"egress":"public"}`, want: http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctl := &fakeSandboxControl{applied: tc.applied}
			router, s, _ := newIdleTestGateway(t, "admin-key", ctl)

			rr := postCreateSandbox(t, router, "admin-key", tc.body)
			if rr.Code != tc.want {
				t.Fatalf("expected %d, got %d body=%s", tc.want, rr.Code, rr.Body.String())
			}
			n, _ := s.Count()
			_, deleted := ctl.calls()
			if tc.want == http.StatusCreated {
				if n != 1 || len(deleted) != 0 {
					t.Fatalf("stored %d sandboxes, runner deletes %v; want 1 and none", n, deleted)
				}
				return
			}
			if n != 0 {
				t.Fatalf("stored %d sandboxes, want none", n)
			}
			if len(deleted) != 1 {
				t.Fatalf("runner deletes = %v, want the refused sandbox removed", deleted)
			}
		})
	}
}

func TestGetSandboxFencesEphemeralAtStopWindow(t *testing.T) {
	router, s, cfg := newIdleTestGateway(t, "admin-key", &fakeSandboxControl{})

	get := func(id string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/sandboxes/"+id, nil)
		req.Header.Set("X-Api-Key", "admin-key")
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}

	now := time.Now()
	fresh := "77777777-7777-4777-8777-777777777777"
	seedRunningSandbox(t, s, fresh, "", now.Unix(), true)
	if rr := get(fresh); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"ephemeral":true`) {
		t.Fatalf("fresh ephemeral GET: got %d body=%s, want 200 with ephemeral true", rr.Code, rr.Body.String())
	}

	// Past IdleStopAfter but well inside IdleDeleteAfter.
	stale := now.Add(-cfg.IdleStopAfter - time.Second).Unix()
	staleEphemeral := "88888888-8888-4888-8888-888888888888"
	staleRegular := "99999999-9999-4999-8999-999999999999"
	seedRunningSandbox(t, s, staleEphemeral, "", stale, true)
	seedRunningSandbox(t, s, staleRegular, "", stale, false)

	if rr := get(staleEphemeral); rr.Code != http.StatusNotFound {
		t.Fatalf("stale ephemeral GET: got %d body=%s, want 404", rr.Code, rr.Body.String())
	}
	if rr := get(staleRegular); rr.Code != http.StatusOK {
		t.Fatalf("stale regular GET: got %d body=%s, want 200 (only past the stop window)", rr.Code, rr.Body.String())
	}
}
