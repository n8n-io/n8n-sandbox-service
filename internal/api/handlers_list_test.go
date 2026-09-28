package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/api/store"
)

// Seeds one sandbox per owner (tenant A, tenant B, admin) and returns the
// router plus the two tenant keys and ids.
func newListFixture(t *testing.T) (router http.Handler, tenantA, tenantB, keyA, keyB string) {
	t.Helper()
	router, s := newTestGateway(t, "admin-key")
	a := mintTenantKey(t, router, `{"name":"a"}`)
	b := mintTenantKey(t, router, `{"name":"b"}`)

	seed := func(id, tenantID string, createdAt int64) {
		if err := s.Create(&store.SandboxRecord{
			ID: id, Status: "running", CreatedAt: createdAt, LastActiveAt: createdAt,
			TenantID: tenantID, Egress: "public", RunnerHTTPBase: "http://127.0.0.1:9",
		}); err != nil {
			t.Fatalf("create sandbox %s: %v", id, err)
		}
	}
	seed(sidA, a.Tenant.ID, 1)
	seed(sidB, b.Tenant.ID, 2)
	seed(sidAdmin, store.AdminTenantID, 3)
	return router, a.Tenant.ID, b.Tenant.ID, a.Key.APIKey, b.Key.APIKey
}

const (
	sidA     = "aaaaaaaa-0000-4000-8000-000000000001"
	sidB     = "bbbbbbbb-0000-4000-8000-000000000002"
	sidAdmin = "cccccccc-0000-4000-8000-000000000003"
)

func listSandboxes(t *testing.T, router http.Handler, path, apiKey string) (*httptest.ResponseRecorder, []SandboxResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Api-Key", apiKey)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	var rows []SandboxResponse
	if rr.Code == http.StatusOK {
		if err := json.Unmarshal(rr.Body.Bytes(), &rows); err != nil {
			t.Fatalf("decode %s: %v body=%s", path, err, rr.Body.String())
		}
	}
	return rr, rows
}

func ownersByID(rows []SandboxResponse) map[string]string {
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.ID] = r.TenantID
	}
	return out
}

func TestListSandboxesReportsTenantID(t *testing.T) {
	router, tenantA, tenantB, keyA, _ := newListFixture(t)

	rr, rows := listSandboxes(t, router, "/sandboxes", "admin-key")
	if rr.Code != http.StatusOK || len(rows) != 3 {
		t.Fatalf("admin list: got %d with %d rows body=%s", rr.Code, len(rows), rr.Body.String())
	}
	owners := ownersByID(rows)
	if owners[sidA] != tenantA || owners[sidB] != tenantB || owners[sidAdmin] != store.AdminTenantID {
		t.Fatalf("admin list owners: %v", owners)
	}

	// A tenant sees only its own rows, each carrying its own id.
	rr, rows = listSandboxes(t, router, "/sandboxes", keyA)
	if rr.Code != http.StatusOK || len(rows) != 1 || rows[0].ID != sidA || rows[0].TenantID != tenantA {
		t.Fatalf("tenant list: got %d body=%s", rr.Code, rr.Body.String())
	}

	// GET by id carries the field too.
	req := httptest.NewRequest(http.MethodGet, "/sandboxes/"+sidAdmin, nil)
	req.Header.Set("X-Api-Key", "admin-key")
	getRR := httptest.NewRecorder()
	router.ServeHTTP(getRR, req)
	var one SandboxResponse
	if getRR.Code != http.StatusOK {
		t.Fatalf("get: %d %s", getRR.Code, getRR.Body.String())
	}
	if err := json.Unmarshal(getRR.Body.Bytes(), &one); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if one.TenantID != store.AdminTenantID {
		t.Fatalf("get tenant_id: got %q, want %q", one.TenantID, store.AdminTenantID)
	}
}

// Legacy rows have an empty tenant_id until the startup backfill runs; the
// response must still report the canonical admin marker, never "".
func TestSandboxResponseCanonicalisesAdminTenantID(t *testing.T) {
	for _, stored := range []string{"", store.AdminTenantID} {
		resp := sandboxResponse(&store.SandboxRecord{ID: sidAdmin, TenantID: stored})
		if resp.TenantID != store.AdminTenantID {
			t.Errorf("stored %q: got %q, want %q", stored, resp.TenantID, store.AdminTenantID)
		}
	}
	resp := sandboxResponse(&store.SandboxRecord{ID: sidA, TenantID: "some-tenant"})
	if resp.TenantID != "some-tenant" {
		t.Errorf("tenant row: got %q", resp.TenantID)
	}
}

// Same promise end-to-end: the store keeps whatever tenant_id it is given, so
// a row seeded with "" is a legacy row, and both list and get must still report
// the admin marker over the wire.
func TestListAndGetSandboxCanonicaliseLegacyTenantID(t *testing.T) {
	router, s := newTestGateway(t, "admin-key")
	const legacy = "dddddddd-0000-4000-8000-000000000004"
	if err := s.Create(&store.SandboxRecord{
		ID: legacy, Status: "running", CreatedAt: 1, LastActiveAt: 1,
		TenantID: "", Egress: "public", RunnerHTTPBase: "http://127.0.0.1:9",
	}); err != nil {
		t.Fatalf("create legacy sandbox: %v", err)
	}

	rr, rows := listSandboxes(t, router, "/sandboxes", "admin-key")
	if rr.Code != http.StatusOK || len(rows) != 1 || rows[0].ID != legacy {
		t.Fatalf("list: got %d body=%s", rr.Code, rr.Body.String())
	}
	if rows[0].TenantID != store.AdminTenantID {
		t.Fatalf("list tenant_id: got %q, want %q", rows[0].TenantID, store.AdminTenantID)
	}

	req := httptest.NewRequest(http.MethodGet, "/sandboxes/"+legacy, nil)
	req.Header.Set("X-Api-Key", "admin-key")
	getRR := httptest.NewRecorder()
	router.ServeHTTP(getRR, req)
	if getRR.Code != http.StatusOK {
		t.Fatalf("get: %d %s", getRR.Code, getRR.Body.String())
	}
	var one SandboxResponse
	if err := json.Unmarshal(getRR.Body.Bytes(), &one); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if one.TenantID != store.AdminTenantID {
		t.Fatalf("get tenant_id: got %q, want %q", one.TenantID, store.AdminTenantID)
	}
}

func TestAdminCanFilterSandboxesByTenant(t *testing.T) {
	router, tenantA, _, _, _ := newListFixture(t)

	rr, rows := listSandboxes(t, router, "/sandboxes?tenant_id="+tenantA, "admin-key")
	if rr.Code != http.StatusOK || len(rows) != 1 || rows[0].ID != sidA {
		t.Fatalf("filter by tenant: got %d body=%s", rr.Code, rr.Body.String())
	}

	rr, rows = listSandboxes(t, router, "/sandboxes?tenant_id="+store.AdminTenantID, "admin-key")
	if rr.Code != http.StatusOK || len(rows) != 1 || rows[0].ID != sidAdmin {
		t.Fatalf("filter by admin: got %d body=%s", rr.Code, rr.Body.String())
	}

	// An unknown tenant is an empty list, not an error: the tenant may have
	// just been deleted, and the caller is an admin either way.
	rr, rows = listSandboxes(t, router, "/sandboxes?tenant_id=11111111-2222-4333-8444-555555555555", "admin-key")
	if rr.Code != http.StatusOK || len(rows) != 0 {
		t.Fatalf("filter by unknown tenant: got %d body=%s", rr.Code, rr.Body.String())
	}

	for _, bad := range []string{"", "not-a-uuid", "admin", "11111111-2222-4333-8444", "'; DROP TABLE sandboxes; --"} {
		rr, _ := listSandboxes(t, router, "/sandboxes?tenant_id="+url.QueryEscape(bad), "admin-key")
		if rr.Code != http.StatusBadRequest {
			t.Errorf("tenant_id=%q: expected %d, got %d body=%s", bad, http.StatusBadRequest, rr.Code, rr.Body.String())
		}
	}

	// A repeated parameter is ambiguous rather than first-wins.
	rr, _ = listSandboxes(t, router, "/sandboxes?tenant_id="+tenantA+"&tenant_id="+store.AdminTenantID, "admin-key")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("repeated tenant_id: expected %d, got %d body=%s", http.StatusBadRequest, rr.Code, rr.Body.String())
	}
}

// A tenant key never gets to name a tenant, not even its own: the parameter
// exists for admins, and accepting it from tenants would let them probe ids.
func TestTenantCannotFilterSandboxesByTenant(t *testing.T) {
	router, tenantA, tenantB, keyA, _ := newListFixture(t)

	for _, target := range []string{tenantA, tenantB, store.AdminTenantID, "garbage"} {
		rr, _ := listSandboxes(t, router, "/sandboxes?tenant_id="+target, keyA)
		if rr.Code != http.StatusForbidden {
			t.Errorf("tenant_id=%q as tenant: expected %d, got %d body=%s", target, http.StatusForbidden, rr.Code, rr.Body.String())
		}
	}

	// Without the parameter the tenant listing is unchanged.
	rr, rows := listSandboxes(t, router, "/sandboxes", keyA)
	if rr.Code != http.StatusOK || len(rows) != 1 || rows[0].ID != sidA {
		t.Fatalf("plain tenant list: got %d body=%s", rr.Code, rr.Body.String())
	}
}
