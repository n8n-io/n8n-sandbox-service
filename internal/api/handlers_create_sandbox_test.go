package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

	router, err := NewGatewayRouter(s, cfg, reg, metrics.NewAPIRecorder(false))
	if err != nil {
		t.Fatalf("create gateway router: %v", err)
	}
	return router, s, cfg
}

// A client that disconnects while the runner is still creating must not
// cancel the runner call: the runner finishes either way, and without a store
// row the sandbox would hold a slot that neither quota nor the idle sweeper can
// see. The create runs to completion and the row is stored.
func TestCreateSandboxClientDisconnectStillStoresRow(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	// Released on every exit path, so a failing run does not leave the fake's
	// create blocked forever.
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

	// Bounded waits: if the create returns before reaching the runner, started
	// never closes, and the suite must fail here rather than stall.
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

// A store write that fails after the runner has created the sandbox must still
// get the runner-side sandbox deleted, or it holds a slot nothing can reclaim.
// The create RPC may have used most of the create budget, so the compensating
// delete cannot run on what is left of it: the deadline the runner sees on the
// delete has to be set after the create finished, not inherited from the create.
func TestCreateSandboxStoreFailureDeletesRunnerSandbox(t *testing.T) {
	const createTakes = 200 * time.Millisecond
	var s store.SandboxStore
	var tenantID string
	createDeadline := make(chan time.Time, 1)
	deleteDeadline := make(chan time.Time, 1)
	fake := &fakeSandboxControl{}
	fake.createHook = func(ctx context.Context) {
		deadline, _ := ctx.Deadline()
		createDeadline <- deadline
		// Long enough that a delete deadline inherited from the create sits
		// measurably before one set after it.
		time.Sleep(createTakes)
		// The tenant goes away mid-create, so the store refuses the row.
		if err := s.DeleteTenant(tenantID); err != nil {
			t.Errorf("delete tenant: %v", err)
		}
	}
	fake.deleteHook = func(ctx context.Context) error {
		deadline, _ := ctx.Deadline()
		deleteDeadline <- deadline
		return nil
	}
	var router http.Handler
	router, s, _ = newIdleTestGateway(t, "admin-key", fake)
	tenant := mintTenantKey(t, router, `{"name":"t"}`)
	tenantID = tenant.Tenant.ID

	rr := postCreateSandbox(t, router, tenant.Key.APIKey, "")
	if rr.Code != http.StatusConflict {
		t.Fatalf("create with tenant deleted mid-create: expected %d, got %d body=%s", http.StatusConflict, rr.Code, rr.Body.String())
	}
	if _, deleted := fake.calls(); len(deleted) != 1 {
		t.Fatalf("runner deletes = %v, want the compensating delete", deleted)
	}
	// A delete on the create's leftover budget carries the create's deadline; one
	// on a budget of its own is later by at least the time the create took.
	if gap := (<-deleteDeadline).Sub(<-createDeadline); gap < createTakes/2 {
		t.Fatalf("delete deadline is %v after the create's, want at least %v: the delete inherited the create's budget", gap, createTakes/2)
	}
}

// A compensating delete that fails is what leaves an untracked sandbox behind,
// so it has to be reported, not swallowed.
func TestCreateSandboxStoreFailureLogsFailedRunnerDelete(t *testing.T) {
	logs := captureLogs(t)
	var s store.SandboxStore
	var tenantID string
	fake := &fakeSandboxControl{}
	fake.createHook = func(context.Context) {
		if err := s.DeleteTenant(tenantID); err != nil {
			t.Errorf("delete tenant: %v", err)
		}
	}
	fake.failDeletes(status.Error(codes.Unavailable, "runner busy"))
	var router http.Handler
	router, s, _ = newIdleTestGateway(t, "admin-key", fake)
	tenant := mintTenantKey(t, router, `{"name":"t"}`)
	tenantID = tenant.Tenant.ID

	if rr := postCreateSandbox(t, router, tenant.Key.APIKey, ""); rr.Code != http.StatusConflict {
		t.Fatalf("create with tenant deleted mid-create: expected %d, got %d body=%s", http.StatusConflict, rr.Code, rr.Body.String())
	}
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
