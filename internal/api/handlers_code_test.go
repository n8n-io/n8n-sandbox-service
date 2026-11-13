package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/store"
)

const codeCreateBody = `{"name":"scope-1","runtime":"n8n-js","runtimeVersion":"2.42.0","slots":2,"memoryMb":1024,"idleTimeoutMs":30000,"maxAgeMs":60000}`

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
	if fake.createOptionsJSON[0] != `{"egress":"none"}` {
		t.Fatalf("create options = %q, want egress none", fake.createOptionsJSON[0])
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
		strings.Replace(codeCreateBody, `"slots":2`, `"slots":3`, 1),
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
		"empty":            ``,
		"not json":         `nope`,
		"trailing data":    codeCreateBody + `{}`,
		"unknown field":    strings.Replace(codeCreateBody, `{`, `{"labels":{"scopeKey":"w:1"},`, 1),
		"no name":          strings.Replace(codeCreateBody, `"name":"scope-1",`, ``, 1),
		"bad name":         strings.Replace(codeCreateBody, `"scope-1"`, `"a/b"`, 1),
		"long name":        strings.Replace(codeCreateBody, `"scope-1"`, `"`+strings.Repeat("a", 129)+`"`, 1),
		"bad runtime":      strings.Replace(codeCreateBody, `"n8n-js"`, `"bash"`, 1),
		"no version":       strings.Replace(codeCreateBody, `"2.42.0"`, `""`, 1),
		"bad version":      strings.Replace(codeCreateBody, `"2.42.0"`, `"2 42"`, 1),
		"no slots":         strings.Replace(codeCreateBody, `"slots":2,`, ``, 1),
		"negative memory":  strings.Replace(codeCreateBody, `"memoryMb":1024`, `"memoryMb":-1`, 1),
		"negative idle":    strings.Replace(codeCreateBody, `"idleTimeoutMs":30000`, `"idleTimeoutMs":-1`, 1),
		"negative max age": strings.Replace(codeCreateBody, `"maxAgeMs":60000`, `"maxAgeMs":-1`, 1),
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
	if stored, _ := s.ListByTenant(tenant.Tenant.ID); len(stored) != 0 {
		t.Fatalf("stored %d sandboxes, want none", len(stored))
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

// Concurrent creates of one name on one API pod share one runner create.
func TestCreateCodeSandboxConcurrentCreatesShareOneSandbox(t *testing.T) {
	const creates = 8
	release := make(chan struct{})
	fake := &fakeSandboxControl{createHook: func(context.Context) {
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
	}}
	router, _, _ := newIdleTestGateway(t, "admin-key", fake)
	tenant := mintTenantKey(t, router, `{"name":"code"}`)

	responses := make([]*httptest.ResponseRecorder, creates)
	var wg sync.WaitGroup
	for i := range creates {
		wg.Go(func() { responses[i] = postCreateCodeSandbox(t, router, tenant.Key.APIKey, codeCreateBody) })
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
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
	if stored, _ := s.ListByTenant(tenant.Tenant.ID); len(stored) != 1 || stored[0].ID != id {
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
