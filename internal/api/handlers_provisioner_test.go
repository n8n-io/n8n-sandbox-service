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

const (
	provAdminKey = "admin-key"
	provKey      = "provisioner-key"
	provMaxDflt  = 50
)

func newProvisionerGateway(t *testing.T) (http.Handler, store.SandboxStore) {
	t.Helper()
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	router, err := NewGatewayRouter(s, withRunnerTLS(&config.APIConfig{
		APIKeys:             map[string]struct{}{provAdminKey: {}},
		ProvisionerKeys:     map[string]struct{}{provKey: {}},
		RunnerAPIKey:        "runner-key",
		MaxFileBytes:        1024,
		DefaultMaxSandboxes: provMaxDflt,
	}), registry.New(45*time.Second), metrics.NewAPIRecorder(false))
	if err != nil {
		t.Fatalf("create gateway router: %v", err)
	}
	return router, s
}

func doJSON(t *testing.T, router http.Handler, method, path, apiKey, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	var req *http.Request
	if rdr != nil {
		req = httptest.NewRequest(method, path, rdr)
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("X-Api-Key", apiKey)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

func provisionerCreateTenant(t *testing.T, router http.Handler, body string) createTenantResponse {
	t.Helper()
	rr := doJSON(t, router, http.MethodPost, "/admin/tenants", provKey, body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("provisioner create tenant: expected %d, got %d body=%s", http.StatusCreated, rr.Code, rr.Body.String())
	}
	var created createTenantResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return created
}

func TestProvisionerCreatesTenantWhoseKeyWorks(t *testing.T) {
	router, _ := newProvisionerGateway(t)

	created := provisionerCreateTenant(t, router, `{"name":"inst","external_ref":"inst-1"}`)
	if created.Tenant.ID == "" || created.Key == nil || created.Key.APIKey == "" {
		t.Fatalf("expected tenant + plaintext key, got %+v", created)
	}
	if created.Tenant.MaxSandboxes != provMaxDflt {
		t.Fatalf("omitted max_sandboxes: expected default %d, got %d", provMaxDflt, created.Tenant.MaxSandboxes)
	}

	rr := doJSON(t, router, http.MethodGet, "/sandboxes", created.Key.APIKey, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("tenant key list: expected %d, got %d body=%s", http.StatusOK, rr.Code, rr.Body.String())
	}
}

func TestProvisionerDeletesEmptyTenantOnly(t *testing.T) {
	router, s := newProvisionerGateway(t)
	created := provisionerCreateTenant(t, router, `{"name":"del"}`)

	sandboxID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	if err := s.Create(&store.SandboxRecord{
		ID: sandboxID, Status: "running", CreatedAt: 1, LastActiveAt: 1, TenantID: created.Tenant.ID,
	}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if rr := doJSON(t, router, http.MethodDelete, "/admin/tenants/"+created.Tenant.ID, provKey, ""); rr.Code != http.StatusConflict {
		t.Fatalf("delete with sandboxes: expected %d, got %d body=%s", http.StatusConflict, rr.Code, rr.Body.String())
	}

	if err := s.Delete(sandboxID); err != nil {
		t.Fatalf("delete sandbox: %v", err)
	}
	if rr := doJSON(t, router, http.MethodDelete, "/admin/tenants/"+created.Tenant.ID, provKey, ""); rr.Code != http.StatusNoContent {
		t.Fatalf("delete empty tenant: expected %d, got %d body=%s", http.StatusNoContent, rr.Code, rr.Body.String())
	}
	// Idempotent, like admin.
	if rr := doJSON(t, router, http.MethodDelete, "/admin/tenants/"+created.Tenant.ID, provKey, ""); rr.Code != http.StatusNoContent {
		t.Fatalf("delete gone tenant: expected %d, got %d", http.StatusNoContent, rr.Code)
	}
}

func TestProvisionerMaxSandboxesBounds(t *testing.T) {
	router, _ := newProvisionerGateway(t)

	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"max_sandboxes":0}`, http.StatusBadRequest},
		{`{"max_sandboxes":51}`, http.StatusBadRequest},
		{`{"max_sandboxes":-1}`, http.StatusBadRequest},
		{`{"max_sandboxes":1}`, http.StatusCreated},
		{`{"max_sandboxes":50}`, http.StatusCreated},
	} {
		rr := doJSON(t, router, http.MethodPost, "/admin/tenants", provKey, tc.body)
		if rr.Code != tc.want {
			t.Errorf("%s: expected %d, got %d body=%s", tc.body, tc.want, rr.Code, rr.Body.String())
		}
	}

	// Admin keeps unlimited and above-default.
	for _, body := range []string{`{"max_sandboxes":0}`, `{"max_sandboxes":51}`} {
		rr := doJSON(t, router, http.MethodPost, "/admin/tenants", provAdminKey, body)
		if rr.Code != http.StatusCreated {
			t.Errorf("admin %s: expected %d, got %d body=%s", body, http.StatusCreated, rr.Code, rr.Body.String())
		}
	}
}

func TestProvisionerDeniedRoutes(t *testing.T) {
	router, s := newProvisionerGateway(t)
	created := provisionerCreateTenant(t, router, `{"name":"victim"}`)
	tenantID := created.Tenant.ID
	keyID := created.Key.ID

	sandboxID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	if err := s.Create(&store.SandboxRecord{
		ID: sandboxID, Status: "running", CreatedAt: 1, LastActiveAt: 1,
		TenantID: tenantID, RunnerHTTPBase: "https://runner.invalid",
	}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	routes := []struct{ method, path, body string }{
		{http.MethodGet, "/admin/tenants", ""},
		{http.MethodGet, "/admin/tenants/" + tenantID, ""},
		{http.MethodGet, "/admin/tenants/" + tenantID + "/keys", ""},
		{http.MethodPost, "/admin/tenants/" + tenantID + "/keys", ""},
		{http.MethodDelete, "/admin/tenants/" + tenantID + "/keys/" + keyID, ""},
		{http.MethodGet, "/sandboxes", ""},
		{http.MethodPost, "/sandboxes", `{}`},
		{http.MethodGet, "/sandboxes/" + sandboxID, ""},
		{http.MethodDelete, "/sandboxes/" + sandboxID, ""},
		{http.MethodPost, "/sandboxes/" + sandboxID + "/executions", `{"command":"id"}`},
		{http.MethodGet, "/sandboxes/" + sandboxID + "/executions/exec-1", ""},
		{http.MethodDelete, "/sandboxes/" + sandboxID + "/executions/exec-1", ""},
		{http.MethodGet, "/sandboxes/" + sandboxID + "/files?path=/tmp", ""},
		{http.MethodGet, "/sandboxes/" + sandboxID + "/files/content?path=/tmp/x", ""},
		{http.MethodPut, "/sandboxes/" + sandboxID + "/files?path=/tmp/x", "data"},
		{http.MethodPost, "/sandboxes/" + sandboxID + "/files?path=/tmp/x", "data"},
		{http.MethodDelete, "/sandboxes/" + sandboxID + "/files?path=/tmp/x", ""},
		{http.MethodPost, "/sandboxes/" + sandboxID + "/files/copy", `{"src":"/a","dest":"/b"}`},
		{http.MethodPost, "/sandboxes/" + sandboxID + "/files/move", `{"src":"/a","dest":"/b"}`},
		{http.MethodPost, "/sandboxes/" + sandboxID + "/mkdir?path=/tmp/d", ""},
		{http.MethodGet, "/sandboxes/" + sandboxID + "/stat?path=/tmp/x", ""},
	}
	for _, rt := range routes {
		rr := doJSON(t, router, rt.method, rt.path, provKey, rt.body)
		if rr.Code != http.StatusForbidden {
			t.Errorf("%s %s: expected %d, got %d body=%s", rt.method, rt.path, http.StatusForbidden, rr.Code, rr.Body.String())
		}
	}

	// The key list is untouched: no key was minted or revoked.
	keys, err := s.ListAPIKeysByTenant(tenantID)
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if len(keys) != 1 || keys[0].ID != keyID || keys[0].RevokedAt != 0 {
		t.Fatalf("expected the single original active key, got %+v", keys)
	}
}

// The create handler used to fall back to the admin pseudo-tenant for any role
// other than tenant. A provisioner must get 403 with no record written.
func TestProvisionerCannotCreateAdminOwnedSandbox(t *testing.T) {
	router, s := newProvisionerGateway(t)

	rr := doJSON(t, router, http.MethodPost, "/sandboxes", provKey, `{}`)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected %d, got %d body=%s", http.StatusForbidden, rr.Code, rr.Body.String())
	}
	records, err := s.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("expected no sandbox record, got %d", len(records))
	}
}

// The handler-level role checks hold on their own, without the middleware's
// path allowlist in front of them.
func TestSandboxHandlersRejectUnknownRoleWithoutMiddleware(t *testing.T) {
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	cfg := withRunnerTLS(&config.APIConfig{DefaultMaxSandboxes: provMaxDflt})

	for name, h := range map[string]http.Handler{
		"list":   handleListSandboxes(s),
		"create": handleCreateSandbox(s, registry.New(45*time.Second), cfg, metrics.NewAPIRecorder(false)),
	} {
		method := http.MethodGet
		if name == "create" {
			method = http.MethodPost
		}
		req := httptest.NewRequest(method, "/sandboxes", strings.NewReader(`{}`))
		req = req.WithContext(withAuthIdentity(req.Context(), authIdentity{Role: roleProvisioner}))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Errorf("%s: expected %d, got %d body=%s", name, http.StatusForbidden, rr.Code, rr.Body.String())
		}
	}
	records, err := s.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("expected no sandbox record, got %d", len(records))
	}
}
