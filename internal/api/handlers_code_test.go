package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/metrics"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const codeCreateBody = `{"name":"scope-1","runtime":"n8n-js","runtimeVersion":"2.42.0","maxConcurrency":2,"memoryMb":1024,"idleTimeoutMs":30000,"maxAgeMs":60000}`

func postCreateCodeSandbox(t *testing.T, router http.Handler, apiKey, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/code/sandboxes", strings.NewReader(body))
	req.Header.Set("X-Api-Key", apiKey)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

func decodeCodeSandbox(t *testing.T, rr *httptest.ResponseRecorder) codeSandboxResponse {
	t.Helper()
	var resp codeSandboxResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %q: %v", rr.Body.String(), err)
	}
	return resp
}

func TestCreateCodeSandboxIsIdempotentOnName(t *testing.T) {
	fake := &fakeSandboxControl{}
	router, s, _ := newIdleTestGateway(t, "admin-key", fake)
	tenant := mintTenantKey(t, router, `{"name":"code"}`)

	rr := postCreateCodeSandbox(t, router, tenant.Key.APIKey, codeCreateBody)
	if rr.Code != http.StatusCreated {
		t.Fatalf("first create: got %d body=%s, want %d", rr.Code, rr.Body.String(), http.StatusCreated)
	}
	first := decodeCodeSandbox(t, rr)
	if !first.Created || first.Name != "scope-1" || first.State != "ready" || !isValidUUID(first.ID) {
		t.Fatalf("first create response = %+v", first)
	}

	rec := mustGet(t, s, first.ID)
	if rec.Kind != store.KindCode || rec.Name != "scope-1" || rec.TenantID != tenant.Tenant.ID ||
		!rec.Ephemeral || rec.Egress != "none" || rec.OptionsHash == "" ||
		rec.IdleTimeoutMs != 30000 || rec.MaxAgeMs != 60000 || rec.DrainingSince != 0 {
		t.Fatalf("stored record = %+v", rec)
	}
	if want := `{"egress":"none","code_options":{"memory_mb":1024}}`; fake.createOptionsJSON[0] != want {
		t.Fatalf("create options = %q, want %q", fake.createOptionsJSON[0], want)
	}

	rr = postCreateCodeSandbox(t, router, tenant.Key.APIKey, codeCreateBody)
	if rr.Code != http.StatusOK {
		t.Fatalf("repeated create: got %d body=%s, want %d", rr.Code, rr.Body.String(), http.StatusOK)
	}
	if again := decodeCodeSandbox(t, rr); again.ID != first.ID || again.Created {
		t.Fatalf("repeated create response = %+v, want id %s and created false", again, first.ID)
	}
	if n := fake.creates(); n != 1 {
		t.Fatalf("runner creates = %d, want 1", n)
	}
}

func TestCreateCodeSandboxOtherSettingsConflict(t *testing.T) {
	router, _, _ := newIdleTestGateway(t, "admin-key", &fakeSandboxControl{})
	tenant := mintTenantKey(t, router, `{"name":"code"}`)

	if rr := postCreateCodeSandbox(t, router, tenant.Key.APIKey, codeCreateBody); rr.Code != http.StatusCreated {
		t.Fatalf("create: got %d body=%s", rr.Code, rr.Body.String())
	}
	for _, body := range []string{
		strings.Replace(codeCreateBody, `"maxConcurrency":2`, `"maxConcurrency":3`, 1),
		strings.Replace(codeCreateBody, `"n8n-js"`, `"n8n-python"`, 1),
		strings.Replace(codeCreateBody, `"2.42.0"`, `"2.43.0"`, 1),
		strings.Replace(codeCreateBody, `"memoryMb":1024`, `"memoryMb":2048`, 1),
		strings.Replace(codeCreateBody, `,"idleTimeoutMs":30000`, ``, 1),
		strings.Replace(codeCreateBody, `,"maxAgeMs":60000`, ``, 1),
	} {
		rr := postCreateCodeSandbox(t, router, tenant.Key.APIKey, body)
		if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), reasonSettingsMismatch) {
			t.Fatalf("create %s: got %d body=%s, want %d %s", body, rr.Code, rr.Body.String(), http.StatusConflict, reasonSettingsMismatch)
		}
	}
}

func TestCreateCodeSandboxNamesArePerTenant(t *testing.T) {
	router, _, _ := newIdleTestGateway(t, "admin-key", &fakeSandboxControl{})
	a := mintTenantKey(t, router, `{"name":"a"}`)
	b := mintTenantKey(t, router, `{"name":"b"}`)

	rrA := postCreateCodeSandbox(t, router, a.Key.APIKey, codeCreateBody)
	rrB := postCreateCodeSandbox(t, router, b.Key.APIKey, codeCreateBody)
	if rrA.Code != http.StatusCreated || rrB.Code != http.StatusCreated {
		t.Fatalf("creates: got %d and %d, want %d for both", rrA.Code, rrB.Code, http.StatusCreated)
	}
	if decodeCodeSandbox(t, rrA).ID == decodeCodeSandbox(t, rrB).ID {
		t.Fatal("two tenants got the same sandbox for one name")
	}
}

func TestCreateCodeSandboxRejectsInvalidRequests(t *testing.T) {
	fake := &fakeSandboxControl{}
	router, _, _ := newIdleTestGateway(t, "admin-key", fake)
	tenant := mintTenantKey(t, router, `{"name":"code"}`)

	for name, body := range map[string]string{
		"empty":             ``,
		"not json":          `nope`,
		"trailing data":     codeCreateBody + `{}`,
		"unknown field":     strings.Replace(codeCreateBody, `{`, `{"labels":{"scopeKey":"w:1"},`, 1),
		"no name":           strings.Replace(codeCreateBody, `"name":"scope-1",`, ``, 1),
		"bad name":          strings.Replace(codeCreateBody, `"scope-1"`, `"a/b"`, 1),
		"long name":         strings.Replace(codeCreateBody, `"scope-1"`, `"`+strings.Repeat("a", 129)+`"`, 1),
		"bad runtime":       strings.Replace(codeCreateBody, `"n8n-js"`, `"bash"`, 1),
		"no version":        strings.Replace(codeCreateBody, `"2.42.0"`, `""`, 1),
		"bad version":       strings.Replace(codeCreateBody, `"2.42.0"`, `"2 42"`, 1),
		"no maxConcurrency": strings.Replace(codeCreateBody, `"maxConcurrency":2,`, ``, 1),
		"negative memory":   strings.Replace(codeCreateBody, `"memoryMb":1024`, `"memoryMb":-1`, 1),
		"negative idle":     strings.Replace(codeCreateBody, `"idleTimeoutMs":30000`, `"idleTimeoutMs":-1`, 1),
		"negative max age":  strings.Replace(codeCreateBody, `"maxAgeMs":60000`, `"maxAgeMs":-1`, 1),
	} {
		if rr := postCreateCodeSandbox(t, router, tenant.Key.APIKey, body); rr.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d body=%s, want %d", name, rr.Code, rr.Body.String(), http.StatusBadRequest)
		}
	}
	if n := fake.creates(); n != 0 {
		t.Fatalf("runner creates = %d, want 0", n)
	}
}

// Admin keys share the admin pseudo-tenant, whose names are its own.
func TestCreateCodeSandboxWithAdminKey(t *testing.T) {
	router, s, _ := newIdleTestGateway(t, "admin-key", &fakeSandboxControl{})
	tenant := mintTenantKey(t, router, `{"name":"code"}`)

	rr := postCreateCodeSandbox(t, router, "admin-key", codeCreateBody)
	if rr.Code != http.StatusCreated {
		t.Fatalf("admin create: got %d body=%s, want %d", rr.Code, rr.Body.String(), http.StatusCreated)
	}
	adminID := decodeCodeSandbox(t, rr).ID
	if rec := mustGet(t, s, adminID); rec.TenantID != store.AdminTenantID || rec.Kind != store.KindCode {
		t.Fatalf("stored record = %+v, want an admin-owned code sandbox", rec)
	}
	if rr := postCreateCodeSandbox(t, router, "admin-key", codeCreateBody); rr.Code != http.StatusOK || decodeCodeSandbox(t, rr).ID != adminID {
		t.Fatalf("repeated admin create: got %d body=%s, want %d with id %s", rr.Code, rr.Body.String(), http.StatusOK, adminID)
	}
	rr = postCreateCodeSandbox(t, router, tenant.Key.APIKey, codeCreateBody)
	if rr.Code != http.StatusCreated || decodeCodeSandbox(t, rr).ID == adminID {
		t.Fatalf("tenant create of the admin's name: got %d body=%s, want %d with its own sandbox", rr.Code, rr.Body.String(), http.StatusCreated)
	}
}

func TestCreateCodeSandboxRejectsUnknownKey(t *testing.T) {
	router, _, _ := newIdleTestGateway(t, "admin-key", &fakeSandboxControl{})
	if rr := postCreateCodeSandbox(t, router, "wrong-key", codeCreateBody); rr.Code != http.StatusUnauthorized {
		t.Fatalf("unknown key create: got %d body=%s, want %d", rr.Code, rr.Body.String(), http.StatusUnauthorized)
	}
}

func TestCreateCodeSandboxNoRunners(t *testing.T) {
	router, _ := newTestGateway(t, "admin-key")
	tenant := mintTenantKey(t, router, `{"name":"code"}`)
	rr := postCreateCodeSandbox(t, router, tenant.Key.APIKey, codeCreateBody)
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), reasonNoCapacity) || rr.Header().Get("Retry-After") == "" {
		t.Fatalf("create: got %d body=%s, want %d %s with Retry-After", rr.Code, rr.Body.String(), http.StatusServiceUnavailable, reasonNoCapacity)
	}
}

func TestCreateCodeSandboxCountsAgainstQuota(t *testing.T) {
	router, _, _ := newIdleTestGateway(t, "admin-key", &fakeSandboxControl{})
	tenant := mintTenantKey(t, router, `{"name":"capped","max_sandboxes":1}`)

	if rr := postCreateCodeSandbox(t, router, tenant.Key.APIKey, codeCreateBody); rr.Code != http.StatusCreated {
		t.Fatalf("create: got %d body=%s", rr.Code, rr.Body.String())
	}
	// The current sandbox of a name is returned at quota.
	if rr := postCreateCodeSandbox(t, router, tenant.Key.APIKey, codeCreateBody); rr.Code != http.StatusOK {
		t.Fatalf("repeated create at quota: got %d body=%s, want %d", rr.Code, rr.Body.String(), http.StatusOK)
	}
	other := strings.Replace(codeCreateBody, `"scope-1"`, `"scope-2"`, 1)
	if rr := postCreateCodeSandbox(t, router, tenant.Key.APIKey, other); rr.Code != http.StatusForbidden {
		t.Fatalf("create of another name at quota: got %d body=%s, want %d", rr.Code, rr.Body.String(), http.StatusForbidden)
	}
}

func TestCreateCodeSandboxRunnerDidNotApplyEgress(t *testing.T) {
	fake := &fakeSandboxControl{applied: func(string) string { return `{"egress":"public"}` }}
	router, s, _ := newIdleTestGateway(t, "admin-key", fake)
	tenant := mintTenantKey(t, router, `{"name":"code"}`)

	if rr := postCreateCodeSandbox(t, router, tenant.Key.APIKey, codeCreateBody); rr.Code != http.StatusBadGateway {
		t.Fatalf("create: got %d body=%s, want %d", rr.Code, rr.Body.String(), http.StatusBadGateway)
	}
	if _, deleted := fake.calls(); len(deleted) != 1 {
		t.Fatalf("runner deletes = %v, want the rolled back create", deleted)
	}
	if stored, _ := s.List(store.SandboxFilter{Kinds: []store.Kind{store.KindCode}, TenantID: tenant.Tenant.ID}); len(stored) != 0 {
		t.Fatalf("stored %d sandboxes, want none", len(stored))
	}
}

// A runner from before code sandboxes ignores the code options and starts a
// general sandbox. Its echo has no code, so the sandbox is rolled back.
func TestCreateCodeSandboxRunnerDidNotApplyCode(t *testing.T) {
	fake := &fakeSandboxControl{applied: func(string) string { return `{"egress":"none"}` }}
	router, s, _ := newIdleTestGateway(t, "admin-key", fake)
	tenant := mintTenantKey(t, router, `{"name":"code"}`)

	rr := postCreateCodeSandbox(t, router, tenant.Key.APIKey, codeCreateBody)
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "does not support code sandboxes") {
		t.Fatalf("create: got %d body=%s, want %d", rr.Code, rr.Body.String(), http.StatusBadGateway)
	}
	if _, deleted := fake.calls(); len(deleted) != 1 {
		t.Fatalf("runner deletes = %v, want the rolled back create", deleted)
	}
	if stored, _ := s.List(store.SandboxFilter{Kinds: []store.Kind{store.KindCode}, TenantID: tenant.Tenant.ID}); len(stored) != 0 {
		t.Fatalf("stored %d sandboxes, want none", len(stored))
	}
}

// A runner refuses a code sandbox before it creates anything, so nothing is
// rolled back or stored.
func TestCreateCodeSandboxRunnerRefusals(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		wantCode int
	}{
		{name: "code sandboxes disabled", err: status.Error(codes.FailedPrecondition, "runner does not support code sandboxes"), wantCode: http.StatusBadGateway},
		{name: "memory over the runner maximum", err: status.Error(codes.InvalidArgument, "memory_mb over the maximum"), wantCode: http.StatusBadRequest},
		{name: "code image not ready", err: status.Error(codes.Unavailable, "code image not yet available"), wantCode: http.StatusServiceUnavailable},
		{name: "other runner error", err: status.Error(codes.Internal, "docker container create --name sandbox-x registry:5000/code-image"), wantCode: http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeSandboxControl{createErr: tc.err}
			router, s, _ := newIdleTestGateway(t, "admin-key", fake)
			tenant := mintTenantKey(t, router, `{"name":"code"}`)

			rr := postCreateCodeSandbox(t, router, tenant.Key.APIKey, codeCreateBody)
			if rr.Code != tc.wantCode {
				t.Fatalf("create: got %d body=%s, want %d", rr.Code, rr.Body.String(), tc.wantCode)
			}
			// The runner's message can hold runner details.
			if strings.Contains(rr.Body.String(), "docker") || strings.Contains(rr.Body.String(), "registry") {
				t.Fatalf("create body %s has the runner's message", rr.Body.String())
			}
			if _, deleted := fake.calls(); len(deleted) != 0 {
				t.Fatalf("runner deletes = %v, want none", deleted)
			}
			if stored, _ := s.List(store.SandboxFilter{Kinds: []store.Kind{store.KindCode}, TenantID: tenant.Tenant.ID}); len(stored) != 0 {
				t.Fatalf("stored %d sandboxes, want none", len(stored))
			}
		})
	}
}

// A save that errored may still have saved the row. Its runner sandbox is kept
// when the row is there, so the name never points to a deleted sandbox, and
// deleted when it is not.
func TestCreateCodeSandboxReconcilesUnclearSave(t *testing.T) {
	for _, tc := range []struct {
		name        string
		save        bool
		wantCode    int
		wantDeletes int
	}{
		{name: "saved, reply lost", save: true, wantCode: http.StatusCreated},
		{name: "not saved", wantCode: http.StatusInternalServerError, wantDeletes: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeSandboxControl{}
			router, s := newCodeTestGateway(t, fake, func(s store.SandboxStore) store.SandboxStore {
				return createErrStore{SandboxStore: s, save: tc.save, err: errors.New("connection reset")}
			})
			tenant := mintTenantKey(t, router, `{"name":"code"}`)

			rr := postCreateCodeSandbox(t, router, tenant.Key.APIKey, codeCreateBody)
			if rr.Code != tc.wantCode {
				t.Fatalf("create: got %d body=%s, want %d", rr.Code, rr.Body.String(), tc.wantCode)
			}
			if _, deleted := fake.calls(); len(deleted) != tc.wantDeletes {
				t.Fatalf("runner deletes = %v, want %d", deleted, tc.wantDeletes)
			}
			stored, err := s.List(store.SandboxFilter{Kinds: []store.Kind{store.KindCode}, TenantID: tenant.Tenant.ID})
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if tc.save != (len(stored) == 1) {
				t.Fatalf("stored = %v, want a row only when it was saved", stored)
			}
		})
	}
}

// A name whose sandbox is past its idle window gets a new sandbox, and the old
// one is deleted.
func TestCreateCodeSandboxReplacesIdleSandbox(t *testing.T) {
	fake := &fakeSandboxControl{}
	router, s, cfg := newIdleTestGateway(t, "admin-key", fake)
	tenant := mintTenantKey(t, router, `{"name":"code"}`)

	rr := postCreateCodeSandbox(t, router, tenant.Key.APIKey, codeCreateBody)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: got %d body=%s", rr.Code, rr.Body.String())
	}
	old := mustGet(t, s, decodeCodeSandbox(t, rr).ID)
	if err := s.Delete(old.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	old.LastActiveAt = time.Now().Add(-2 * cfg.IdleStopAfter).Unix()
	if err := s.Create(old); err != nil {
		t.Fatalf("re-create: %v", err)
	}

	rr = postCreateCodeSandbox(t, router, tenant.Key.APIKey, codeCreateBody)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create after idle: got %d body=%s, want %d", rr.Code, rr.Body.String(), http.StatusCreated)
	}
	if id := decodeCodeSandbox(t, rr).ID; id == old.ID {
		t.Fatal("got the idle sandbox back")
	}
	if _, deleted := fake.calls(); len(deleted) != 1 || deleted[0] != old.ID {
		t.Fatalf("runner deletes = %v, want [%s]", deleted, old.ID)
	}
	if mustGet(t, s, old.ID) != nil {
		t.Fatal("idle sandbox still stored")
	}
}

// newCodeTestGateway is newIdleTestGateway with its store wrapped by wrap.
func newCodeTestGateway(t *testing.T, fake *fakeSandboxControl, wrap func(store.SandboxStore) store.SandboxStore) (http.Handler, store.SandboxStore) {
	t.Helper()
	s := newSweepStore(t)
	cfg := idleSweepConfig()
	cfg.APIKeys = map[string]struct{}{"admin-key": {}}
	cfg.MaxFileBytes = 1024
	cfg.DefaultMaxSandboxes = 50
	reg := registry.New(45 * time.Second)
	reg.Upsert("runner-1", "https://127.0.0.1:9", startFakeRunnerControl(t, fake), true, false, 10, 0, 0)
	router, err := NewGatewayRouter(wrap(s), cfg, reg, metrics.NewAPIRecorder(false), NewActivityRecorder(s, nil))
	if err != nil {
		t.Fatalf("create gateway router: %v", err)
	}
	return router, s
}

// getTenantCountingStore counts GetTenant calls, which a tenant's create makes
// once before it joins the creates of its name.
type getTenantCountingStore struct {
	store.SandboxStore
	calls *atomic.Int32
}

func (s getTenantCountingStore) GetTenant(id string) (*store.Tenant, error) {
	s.calls.Add(1)
	return s.SandboxStore.GetTenant(id)
}

// Concurrent creates of one name on one API pod share one runner create.
func TestCreateCodeSandboxConcurrentCreatesShareOneSandbox(t *testing.T) {
	const creates = 8
	var calls atomic.Int32
	allStarted := make(chan struct{})
	// The first runner create, and so the save, waits until every create has
	// got past its tenant lookup, so none can find the sandbox already saved
	// instead of joining the shared create.
	fake := &fakeSandboxControl{createHook: func(context.Context) {
		select {
		case <-allStarted:
		case <-time.After(10 * time.Second):
		}
	}}
	router, _ := newCodeTestGateway(t, fake, func(s store.SandboxStore) store.SandboxStore {
		return getTenantCountingStore{SandboxStore: s, calls: &calls}
	})
	tenant := mintTenantKey(t, router, `{"name":"code"}`)
	calls.Store(0)

	responses := make([]*httptest.ResponseRecorder, creates)
	var wg sync.WaitGroup
	for i := range creates {
		wg.Go(func() { responses[i] = postCreateCodeSandbox(t, router, tenant.Key.APIKey, codeCreateBody) })
	}
	waitFor(t, "every create to look up its tenant", func() bool { return calls.Load() >= creates })
	close(allStarted)
	wg.Wait()

	assertOneSandboxForName(t, responses)
	if n := fake.creates(); n != 1 {
		t.Fatalf("runner creates = %d, want 1", n)
	}
}

// Concurrent creates of one name on two API pods both reach a runner. The one
// saved second deletes its runner sandbox and returns the one saved first.
func TestCreateCodeSandboxConcurrentCreatesAcrossPods(t *testing.T) {
	var arrived atomic.Int32
	bothArrived := make(chan struct{})
	fake := &fakeSandboxControl{createHook: func(context.Context) {
		if arrived.Add(1) == 2 {
			close(bothArrived)
		}
		select {
		case <-bothArrived:
		case <-time.After(10 * time.Second):
		}
	}}
	routers, s, _ := newIdleTestGatewayPods(t, "admin-key", fake, 2)
	tenant := mintTenantKey(t, routers[0], `{"name":"code"}`)

	responses := make([]*httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for i := range responses {
		wg.Go(func() { responses[i] = postCreateCodeSandbox(t, routers[i], tenant.Key.APIKey, codeCreateBody) })
	}
	wg.Wait()

	id := assertOneSandboxForName(t, responses)
	if n := fake.creates(); n != 2 {
		t.Fatalf("runner creates = %d, want 2", n)
	}
	_, deleted := fake.calls()
	if len(deleted) != 1 || deleted[0] == id {
		t.Fatalf("runner deletes = %v, want the losing create, not %s", deleted, id)
	}
	if stored, _ := s.List(store.SandboxFilter{Kinds: []store.Kind{store.KindCode}, TenantID: tenant.Tenant.ID}); len(stored) != 1 || stored[0].ID != id {
		t.Fatalf("stored = %v, want only %s", stored, id)
	}
}

// assertOneSandboxForName checks that responses are one 201 and otherwise 200,
// all for the same sandbox, and returns its id.
func assertOneSandboxForName(t *testing.T, responses []*httptest.ResponseRecorder) string {
	t.Helper()
	id := ""
	created := 0
	for _, rr := range responses {
		switch rr.Code {
		case http.StatusCreated:
			created++
		case http.StatusOK:
		default:
			t.Fatalf("create: got %d body=%s", rr.Code, rr.Body.String())
		}
		resp := decodeCodeSandbox(t, rr)
		if resp.Created != (rr.Code == http.StatusCreated) {
			t.Fatalf("created = %v with status %d", resp.Created, rr.Code)
		}
		if id == "" {
			id = resp.ID
		} else if resp.ID != id {
			t.Fatalf("creates got sandboxes %s and %s", id, resp.ID)
		}
	}
	if created != 1 {
		t.Fatalf("%d creates answered 201, want 1", created)
	}
	return id
}
