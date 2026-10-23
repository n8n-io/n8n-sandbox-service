package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A tenant's max_idle_window narrows the server's cap for its creates and
// PATCHes; null on the tenant PATCH clears it.
func TestTenantMaxIdleWindowCapsCreates(t *testing.T) {
	router, _, cfg := newIdleTestGateway(t, "admin-key", &fakeSandboxControl{})
	cfg.MaxIdleWindow = 24 * time.Hour

	if rr := doJSON(t, router, http.MethodPost, "/admin/tenants", "admin-key", `{"name":"x","max_idle_window":90000}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("cap above the server's: %d %s", rr.Code, rr.Body.String())
	}
	created := mintTenantKey(t, router, `{"name":"t","max_idle_window":600}`)
	if created.Tenant.MaxIdleWindow != 600 {
		t.Fatalf("tenant = %+v", created.Tenant)
	}
	key := created.Key.APIKey
	if rr := postCreateSandbox(t, router, key, `{"idle_stop_after":900}`); rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "600") {
		t.Fatalf("over the tenant cap: %d %s", rr.Code, rr.Body.String())
	}
	if rr := postCreateSandbox(t, router, key, `{"idle_stop_after":600}`); rr.Code != http.StatusCreated {
		t.Fatalf("at the tenant cap: %d %s", rr.Code, rr.Body.String())
	}
	// Admin-owned sandboxes keep the server's cap.
	if rr := postCreateSandbox(t, router, "admin-key", `{"idle_stop_after":3600}`); rr.Code != http.StatusCreated {
		t.Fatalf("admin create: %d %s", rr.Code, rr.Body.String())
	}

	path := "/admin/tenants/" + created.Tenant.ID
	if rr := doJSON(t, router, http.MethodPatch, path, "admin-key", `{"max_idle_window":null}`); rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), "max_idle_window") {
		t.Fatalf("clear: %d %s", rr.Code, rr.Body.String())
	}
	if rr := postCreateSandbox(t, router, key, `{"idle_stop_after":900}`); rr.Code != http.StatusCreated {
		t.Fatalf("after clearing the cap: %d %s", rr.Code, rr.Body.String())
	}
	for _, body := range []string{`{}`, `{"max_idle_window":30}`, `{"max_idle_window":"1h"}`, `{"max_idle_window":600,"x":1}`} {
		if rr := doJSON(t, router, http.MethodPatch, path, "admin-key", body); rr.Code != http.StatusBadRequest {
			t.Fatalf("tenant PATCH %s: %d %s", body, rr.Code, rr.Body.String())
		}
	}
	rr := doJSON(t, router, http.MethodPatch, path, "admin-key", `{"max_idle_window":1200}`)
	var tr tenantResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &tr); err != nil || rr.Code != http.StatusOK || tr.MaxIdleWindow != 1200 || tr.MaxSandboxes != 50 {
		t.Fatalf("set without max_sandboxes: %d %s", rr.Code, rr.Body.String())
	}
}

func TestPatchSandboxIdleWindows(t *testing.T) {
	router, s, cfg := newIdleTestGateway(t, "admin-key", &fakeSandboxControl{})
	cfg.MaxIdleWindow = 7 * 24 * time.Hour
	owner := mintTenantKey(t, router, `{"name":"owner","max_idle_window":86400}`)
	other := mintTenantKey(t, router, `{"name":"other"}`)

	const sid = "d4d4d4d4-d4d4-4d4d-8d4d-d4d4d4d4d4d4"
	if rr := postCreateSandbox(t, router, owner.Key.APIKey, `{"id":"`+sid+`","idle_stop_after":600}`); rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	path := "/sandboxes/" + sid
	patch := func(key, body string) *httptest.ResponseRecorder {
		return doJSON(t, router, http.MethodPatch, path, key, body)
	}

	rr := patch(owner.Key.APIKey, `{"idle_delete_after":3600}`)
	var resp SandboxResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil || rr.Code != http.StatusOK || resp.IdleStopAfter != 600 || resp.IdleDeleteAfter != 3600 {
		t.Fatalf("set delete: %d %s", rr.Code, rr.Body.String())
	}
	if rec, _ := s.Get(sid); rec.IdleStopAfter != 600 || rec.IdleDeleteAfter != 3600 {
		t.Fatalf("stored = %+v", rec)
	}
	if rr := patch(owner.Key.APIKey, `{"idle_stop_after":null}`); rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), "idle_stop_after") {
		t.Fatalf("clear stop: %d %s", rr.Code, rr.Body.String())
	}
	for _, tc := range []struct {
		key, body string
		want      int
	}{
		{owner.Key.APIKey, `{}`, http.StatusBadRequest},
		{owner.Key.APIKey, `{"idle_stop_after":7200}`, http.StatusBadRequest},     // after delete 3600
		{owner.Key.APIKey, `{"idle_delete_after":172800}`, http.StatusBadRequest}, // over the tenant's 1 day
		{owner.Key.APIKey, `{"idle_stop_after":0}`, http.StatusBadRequest},
		{owner.Key.APIKey, `{"idle_archive_after":600}`, http.StatusBadRequest},
		{other.Key.APIKey, `{"idle_stop_after":600}`, http.StatusNotFound},
		// An admin is held to the owner's cap too.
		{"admin-key", `{"idle_delete_after":172800}`, http.StatusBadRequest},
		{"admin-key", `{"idle_delete_after":86400}`, http.StatusOK},
	} {
		if rr := patch(tc.key, tc.body); rr.Code != tc.want {
			t.Errorf("PATCH %s: %d %s, want %d", tc.body, rr.Code, rr.Body.String(), tc.want)
		}
	}
	if rr := doJSON(t, router, http.MethodPatch, "/sandboxes/e5e5e5e5-e5e5-4e5e-8e5e-e5e5e5e5e5e5", "admin-key", `{"idle_stop_after":600}`); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown sandbox: %d", rr.Code)
	}
}
