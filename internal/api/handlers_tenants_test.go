package api

import (
	"encoding/json"
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

	router, err := NewGatewayRouter(s, &config.APIConfig{
		APIKeys:             map[string]struct{}{adminKey: {}},
		RunnerAPIKey:        "runner-key",
		MaxFileBytes:        1024,
		DefaultMaxSandboxes: 50,
	}, registry.New(45*time.Second), metrics.NewAPIRecorder(false))
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
