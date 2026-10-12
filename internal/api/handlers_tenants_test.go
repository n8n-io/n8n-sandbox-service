package api

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/metrics"
)

func newTestGateway(t *testing.T, adminKey string) (http.Handler, store.SandboxStore) {
	t.Helper()
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	router, err := NewGatewayRouter(s, withRunnerTLS(&config.APIConfig{
		APIKeys:             map[string]struct{}{adminKey: {}},
		RunnerAPIKey:        "runner-key",
		MaxFileBytes:        1024,
		DefaultMaxSandboxes: 50,
	}), registry.New(45*time.Second), metrics.NewAPIRecorder(false))
	if err != nil {
		t.Fatalf("create gateway router: %v", err)
	}
	return router, s
}

func TestAdminCanCreateTenantAndKey(t *testing.T) {
	router, _ := newTestGateway(t, "admin-key")

	req := httptest.NewRequest(http.MethodPost, "/admin/tenants", strings.NewReader(`{"name":"acme","external_ref":"inst-1"}`))
	req.Header.Set("X-Api-Key", "admin-key")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create tenant: expected %d, got %d body=%s", http.StatusCreated, rr.Code, rr.Body.String())
	}

	var created createTenantResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Tenant.ID == "" || created.Key == nil || created.Key.APIKey == "" {
		t.Fatalf("expected tenant + plaintext key, got %+v", created)
	}
	if !strings.HasPrefix(created.Key.APIKey, "sbk_") {
		t.Fatalf("unexpected key format %q", created.Key.APIKey)
	}

	// Tenant key can list (empty) sandboxes.
	listReq := httptest.NewRequest(http.MethodGet, "/sandboxes", nil)
	listReq.Header.Set("X-Api-Key", created.Key.APIKey)
	listRR := httptest.NewRecorder()
	router.ServeHTTP(listRR, listReq)
	if listRR.Code != http.StatusOK {
		t.Fatalf("tenant list: expected %d, got %d", http.StatusOK, listRR.Code)
	}

	// Tenant cannot hit admin routes.
	adminReq := httptest.NewRequest(http.MethodGet, "/admin/tenants", nil)
	adminReq.Header.Set("X-Api-Key", created.Key.APIKey)
	adminRR := httptest.NewRecorder()
	router.ServeHTTP(adminRR, adminReq)
	if adminRR.Code != http.StatusForbidden {
		t.Fatalf("tenant admin list: expected %d, got %d", http.StatusForbidden, adminRR.Code)
	}
}

func TestCreateTenantRejectsTrailingJSON(t *testing.T) {
	router, s := newTestGateway(t, "admin-key")

	req := httptest.NewRequest(http.MethodPost, "/admin/tenants", strings.NewReader(`{"name":"acme"}{"extra":1}`))
	req.Header.Set("X-Api-Key", "admin-key")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected %d, got %d body=%s", http.StatusBadRequest, rr.Code, rr.Body.String())
	}
	tenants, err := s.ListTenants()
	if err != nil {
		t.Fatalf("list tenants: %v", err)
	}
	if len(tenants) != 0 {
		t.Fatalf("expected no tenant provisioned, got %d", len(tenants))
	}
}

func TestTenantCannotAccessOtherTenantSandbox(t *testing.T) {
	router, s := newTestGateway(t, "admin-key")

	mint := func(name string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/admin/tenants", strings.NewReader(`{"name":"`+name+`"}`))
		req.Header.Set("X-Api-Key", "admin-key")
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("mint %s: %d %s", name, rr.Code, rr.Body.String())
		}
		var created createTenantResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return created.Key.APIKey
	}

	keyA := mint("a")
	keyB := mint("b")

	// Seed a sandbox owned by tenant A directly in the store.
	tenants, err := s.ListTenants()
	if err != nil || len(tenants) < 2 {
		t.Fatalf("list tenants: %v len=%d", err, len(tenants))
	}
	var tenantA string
	for _, tn := range tenants {
		if tn.Name == "a" {
			tenantA = tn.ID
			break
		}
	}
	sid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	if err := s.Create(&store.SandboxRecord{
		ID: sid, Status: "running", CreatedAt: 1, LastActiveAt: 1,
		TenantID: tenantA, RunnerHTTPBase: "http://127.0.0.1:9",
	}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	getA := httptest.NewRequest(http.MethodGet, "/sandboxes/"+sid, nil)
	getA.Header.Set("X-Api-Key", keyA)
	rrA := httptest.NewRecorder()
	router.ServeHTTP(rrA, getA)
	if rrA.Code != http.StatusOK {
		t.Fatalf("owner get: expected %d, got %d body=%s", http.StatusOK, rrA.Code, rrA.Body.String())
	}

	getB := httptest.NewRequest(http.MethodGet, "/sandboxes/"+sid, nil)
	getB.Header.Set("X-Api-Key", keyB)
	rrB := httptest.NewRecorder()
	router.ServeHTTP(rrB, getB)
	if rrB.Code != http.StatusNotFound {
		t.Fatalf("other tenant get: expected %d, got %d", http.StatusNotFound, rrB.Code)
	}

	listB := httptest.NewRequest(http.MethodGet, "/sandboxes", nil)
	listB.Header.Set("X-Api-Key", keyB)
	rrListB := httptest.NewRecorder()
	router.ServeHTTP(rrListB, listB)
	if rrListB.Code != http.StatusOK || strings.TrimSpace(rrListB.Body.String()) != "[]" {
		t.Fatalf("tenant B list: got %d %s", rrListB.Code, rrListB.Body.String())
	}

	listAdmin := httptest.NewRequest(http.MethodGet, "/sandboxes", nil)
	listAdmin.Header.Set("X-Api-Key", "admin-key")
	rrAdmin := httptest.NewRecorder()
	router.ServeHTTP(rrAdmin, listAdmin)
	if rrAdmin.Code != http.StatusOK || !strings.Contains(rrAdmin.Body.String(), sid) {
		t.Fatalf("admin list: got %d %s", rrAdmin.Code, rrAdmin.Body.String())
	}

	delB := httptest.NewRequest(http.MethodDelete, "/sandboxes/"+sid, nil)
	delB.Header.Set("X-Api-Key", keyB)
	rrDelB := httptest.NewRecorder()
	router.ServeHTTP(rrDelB, delB)
	if rrDelB.Code != http.StatusNotFound {
		t.Fatalf("other tenant delete: expected %d, got %d", http.StatusNotFound, rrDelB.Code)
	}

	getAfterDel := httptest.NewRequest(http.MethodGet, "/sandboxes/"+sid, nil)
	getAfterDel.Header.Set("X-Api-Key", keyA)
	rrAfterDel := httptest.NewRecorder()
	router.ServeHTTP(rrAfterDel, getAfterDel)
	if rrAfterDel.Code != http.StatusOK {
		t.Fatalf("owner get after foreign delete: expected %d, got %d body=%s", http.StatusOK, rrAfterDel.Code, rrAfterDel.Body.String())
	}

	missing := "11111111-2222-3333-4444-555555555555"
	delMissing := httptest.NewRequest(http.MethodDelete, "/sandboxes/"+missing, nil)
	delMissing.Header.Set("X-Api-Key", keyB)
	rrMissing := httptest.NewRecorder()
	router.ServeHTTP(rrMissing, delMissing)
	if rrMissing.Code != http.StatusNotFound {
		t.Fatalf("missing delete: expected %d, got %d", http.StatusNotFound, rrMissing.Code)
	}
}

func TestRevokedTenantKeyRejected(t *testing.T) {
	router, _ := newTestGateway(t, "admin-key")

	req := httptest.NewRequest(http.MethodPost, "/admin/tenants", strings.NewReader(`{}`))
	req.Header.Set("X-Api-Key", "admin-key")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	var created createTenantResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	revoke := httptest.NewRequest(http.MethodDelete, "/admin/tenants/"+created.Tenant.ID+"/keys/"+created.Key.ID, nil)
	revoke.Header.Set("X-Api-Key", "admin-key")
	revokeRR := httptest.NewRecorder()
	router.ServeHTTP(revokeRR, revoke)
	if revokeRR.Code != http.StatusNoContent {
		t.Fatalf("revoke: expected %d, got %d", http.StatusNoContent, revokeRR.Code)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/sandboxes", nil)
	listReq.Header.Set("X-Api-Key", created.Key.APIKey)
	listRR := httptest.NewRecorder()
	router.ServeHTTP(listRR, listReq)
	if listRR.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key: expected %d, got %d", http.StatusUnauthorized, listRR.Code)
	}
}

func TestAdminUpdatesTenantLimits(t *testing.T) {
	router, s := newTestGateway(t, "admin-key")
	created := mintTenantKey(t, router, `{"name":"acme","external_ref":"inst-1","max_sandboxes":5}`)

	// Admin keys may set anything the INTEGER column holds, unlimited included.
	for _, n := range []int{0, 51, math.MaxInt32, 1} {
		rr := doJSON(t, router, http.MethodPatch, "/admin/tenants/"+created.Tenant.ID, "admin-key", fmt.Sprintf(`{"max_sandboxes":%d}`, n))
		if rr.Code != http.StatusOK {
			t.Fatalf("patch %d: expected %d, got %d body=%s", n, http.StatusOK, rr.Code, rr.Body.String())
		}
		// The bare tenant, as GET /admin/tenants/{id} returns it.
		var got tenantResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		want := created.Tenant
		want.MaxSandboxes = n
		if got != want {
			t.Fatalf("patch %d: got %+v, want %+v", n, got, want)
		}
		stored, err := s.GetTenant(created.Tenant.ID)
		if err != nil || stored == nil || stored.MaxSandboxes != n {
			t.Fatalf("patch %d: stored %+v err=%v", n, stored, err)
		}
	}
}

func TestUpdateTenantRejectsInvalidRequests(t *testing.T) {
	router, s := newTestGateway(t, "admin-key")
	created := mintTenantKey(t, router, `{"name":"acme","max_sandboxes":5}`)
	path := "/admin/tenants/" + created.Tenant.ID

	for _, tc := range []struct {
		name, path, body string
		want             int
	}{
		{"invalid id", "/admin/tenants/not-a-uuid", `{"max_sandboxes":1}`, http.StatusBadRequest},
		{"missing tenant", "/admin/tenants/11111111-2222-3333-4444-555555555555", `{"max_sandboxes":1}`, http.StatusNotFound},
		{"empty body", path, "", http.StatusBadRequest},
		{"no fields", path, `{}`, http.StatusBadRequest},
		{"null", path, `{"max_sandboxes":null}`, http.StatusBadRequest},
		{"unknown field", path, `{"max_sandboxes":1,"name":"renamed"}`, http.StatusBadRequest},
		{"trailing JSON", path, `{"max_sandboxes":1}{"max_sandboxes":2}`, http.StatusBadRequest},
		{"string", path, `{"max_sandboxes":"1"}`, http.StatusBadRequest},
		{"fraction", path, `{"max_sandboxes":1.5}`, http.StatusBadRequest},
		{"negative", path, `{"max_sandboxes":-1}`, http.StatusBadRequest},
		{"above INTEGER", path, `{"max_sandboxes":2147483648}`, http.StatusBadRequest},
	} {
		rr := doJSON(t, router, http.MethodPatch, tc.path, "admin-key", tc.body)
		if rr.Code != tc.want {
			t.Errorf("%s: expected %d, got %d body=%s", tc.name, tc.want, rr.Code, rr.Body.String())
		}
	}

	stored, err := s.GetTenant(created.Tenant.ID)
	if err != nil || stored == nil || stored.MaxSandboxes != 5 || stored.Name != "acme" {
		t.Fatalf("tenant changed by refused requests: %+v err=%v", stored, err)
	}
}

func TestTenantKeyCannotUpdateTenantLimits(t *testing.T) {
	router, s := newTestGateway(t, "admin-key")
	created := mintTenantKey(t, router, `{"name":"acme","max_sandboxes":5}`)

	rr := doJSON(t, router, http.MethodPatch, "/admin/tenants/"+created.Tenant.ID, created.Key.APIKey, `{"max_sandboxes":1000}`)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected %d, got %d body=%s", http.StatusForbidden, rr.Code, rr.Body.String())
	}
	stored, err := s.GetTenant(created.Tenant.ID)
	if err != nil || stored == nil || stored.MaxSandboxes != 5 {
		t.Fatalf("tenant changed: %+v err=%v", stored, err)
	}
}

// Lowering max_sandboxes below the tenant's count refuses new creates and leaves
// the existing sandboxes alone; raising it again lifts the refusal.
func TestLoweredTenantLimitBlocksCreatesAndKeepsSandboxes(t *testing.T) {
	router, s := newTestGateway(t, "admin-key")
	tenant := mintTenantKey(t, router, `{"name":"shrink","max_sandboxes":5}`)
	path := "/admin/tenants/" + tenant.Tenant.ID

	sandboxIDs := []string{
		"a1111111-1111-4111-8111-111111111111",
		"a2222222-2222-4222-8222-222222222222",
		"a3333333-3333-4333-8333-333333333333",
	}
	for _, id := range sandboxIDs {
		if err := s.Create(&store.SandboxRecord{
			ID: id, Status: "running", CreatedAt: 1, LastActiveAt: 1, TenantID: tenant.Tenant.ID,
		}); err != nil {
			t.Fatalf("seed sandbox: %v", err)
		}
	}

	if rr := doJSON(t, router, http.MethodPatch, path, "admin-key", `{"max_sandboxes":1}`); rr.Code != http.StatusOK {
		t.Fatalf("lower limit: expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	rr := postCreateSandbox(t, router, tenant.Key.APIKey, "")
	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "quota exceeded") {
		t.Fatalf("create above lowered limit: expected %d quota exceeded, got %d body=%s", http.StatusForbidden, rr.Code, rr.Body.String())
	}
	for _, id := range sandboxIDs {
		rec, err := s.Get(id)
		if err != nil || rec == nil || rec.Status != "running" {
			t.Fatalf("sandbox %s after lowering the limit: %+v err=%v", id, rec, err)
		}
	}

	if rr := doJSON(t, router, http.MethodPatch, path, "admin-key", `{"max_sandboxes":4}`); rr.Code != http.StatusOK {
		t.Fatalf("raise limit: expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
	// Past the quota check; no runner is registered, so placement fails.
	if rr := postCreateSandbox(t, router, tenant.Key.APIKey, ""); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("create below raised limit: expected %d, got %d body=%s", http.StatusServiceUnavailable, rr.Code, rr.Body.String())
	}
}

func TestDeleteTenantConflictWhenSandboxesExist(t *testing.T) {
	router, s := newTestGateway(t, "admin-key")

	req := httptest.NewRequest(http.MethodPost, "/admin/tenants", strings.NewReader(`{"name":"busy"}`))
	req.Header.Set("X-Api-Key", "admin-key")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	var created createTenantResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if err := s.Create(&store.SandboxRecord{
		ID:     "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		Status: "running", CreatedAt: 1, LastActiveAt: 1,
		TenantID: created.Tenant.ID,
	}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	del := httptest.NewRequest(http.MethodDelete, "/admin/tenants/"+created.Tenant.ID, nil)
	del.Header.Set("X-Api-Key", "admin-key")
	delRR := httptest.NewRecorder()
	router.ServeHTTP(delRR, del)
	if delRR.Code != http.StatusConflict {
		t.Fatalf("delete with sandboxes: expected %d, got %d body=%s", http.StatusConflict, delRR.Code, delRR.Body.String())
	}

	if err := s.Delete("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"); err != nil {
		t.Fatalf("delete sandbox: %v", err)
	}
	del2 := httptest.NewRequest(http.MethodDelete, "/admin/tenants/"+created.Tenant.ID, nil)
	del2.Header.Set("X-Api-Key", "admin-key")
	del2RR := httptest.NewRecorder()
	router.ServeHTTP(del2RR, del2)
	if del2RR.Code != http.StatusNoContent {
		t.Fatalf("delete after sandboxes gone: expected %d, got %d", http.StatusNoContent, del2RR.Code)
	}
}
