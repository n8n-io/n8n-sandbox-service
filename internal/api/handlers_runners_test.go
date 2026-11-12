package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/metrics"
)

const scalerKey = "scaler-key"

func newRunnersGateway(t *testing.T) (http.Handler, *registry.MemoryRegistry) {
	t.Helper()
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	reg := registry.New(45 * time.Second)
	router, err := NewGatewayRouter(s, withRunnerTLS(&config.APIConfig{
		APIKeys:             map[string]struct{}{provAdminKey: {}},
		ProvisionerKeys:     map[string]struct{}{provKey: {}},
		ScalerKeys:          map[string]struct{}{scalerKey: {}},
		RunnerAPIKey:        "runner-key",
		MaxFileBytes:        1024,
		DefaultMaxSandboxes: provMaxDflt,
		HeartbeatGrace:      45 * time.Second,
	}), reg, metrics.NewAPIRecorder(false), NewActivityRecorder(s, nil))
	if err != nil {
		t.Fatalf("create gateway router: %v", err)
	}
	return router, reg
}

func TestListRunnersAsAdmin(t *testing.T) {
	router, reg := newRunnersGateway(t)
	reg.Upsert("r-b", "https://runner-b.internal:8443", "runner-b.internal:9091", false, true, 10, 4, 1)
	reg.Upsert("r-a", "https://runner-a.internal:8443", "runner-a.internal:9091", true, false, 8, 2, 0)

	rr := doJSON(t, router, http.MethodGet, "/admin/runners", provAdminKey, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), ".internal") {
		t.Fatalf("response leaks a runner address: %s", rr.Body.String())
	}
	var got listRunnersResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Runners) != 2 {
		t.Fatalf("expected 2 runners, got %+v", got.Runners)
	}
	a, b := got.Runners[0], got.Runners[1]
	if a.ID != "r-a" || !a.Healthy || a.Cordoned || a.CapacityTotal != 8 || a.CapacityUsed != 2 || a.CapacityStopped != 0 {
		t.Fatalf("r-a = %+v", a)
	}
	if b.ID != "r-b" || b.Healthy || !b.Cordoned || b.CapacityTotal != 10 || b.CapacityUsed != 4 || b.CapacityStopped != 1 {
		t.Fatalf("r-b = %+v", b)
	}
	if age := time.Now().Unix() - a.LastSeen; age < 0 || age > 5 {
		t.Fatalf("r-a last_seen = %d, want about now", a.LastSeen)
	}
}

func TestListRunnersEmptyIsArray(t *testing.T) {
	router, _ := newRunnersGateway(t)
	rr := doJSON(t, router, http.MethodGet, "/admin/runners", provAdminKey, "")
	if rr.Code != http.StatusOK || strings.TrimSpace(rr.Body.String()) != `{"runners":[]}` {
		t.Fatalf("expected 200 {\"runners\":[]}, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestListRunnersRejectsProvisionerAndTenantKeys(t *testing.T) {
	router, _ := newRunnersGateway(t)
	created := provisionerCreateTenant(t, router, `{"name":"inst","external_ref":"inst-1"}`)
	if created.Key == nil {
		t.Fatal("expected a tenant key")
	}

	for name, key := range map[string]string{"provisioner": provKey, "tenant": created.Key.APIKey} {
		rr := doJSON(t, router, http.MethodGet, "/admin/runners", key, "")
		if rr.Code != http.StatusForbidden {
			t.Errorf("%s key: expected 403, got %d body=%s", name, rr.Code, rr.Body.String())
		}
	}
}

func TestListRunnersAsScaler(t *testing.T) {
	router, reg := newRunnersGateway(t)
	reg.Upsert("r-a", "https://runner-a.internal:8443", "runner-a.internal:9091", true, false, 8, 2, 0)

	rr := doJSON(t, router, http.MethodGet, "/admin/runners", scalerKey, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	var got listRunnersResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Runners) != 1 || got.Runners[0].ID != "r-a" {
		t.Fatalf("expected r-a, got %+v", got.Runners)
	}
}

func TestScalerDeniedRoutes(t *testing.T) {
	router, _ := newRunnersGateway(t)
	created := provisionerCreateTenant(t, router, `{"name":"victim"}`)
	tenantID := created.Tenant.ID
	keyID := created.Key.ID
	sandboxID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	routes := []struct{ method, path, body string }{
		{http.MethodGet, "/admin/scaler", ""},
		{http.MethodGet, "/admin/runners/", ""},
		{http.MethodGet, "/admin/tenants", ""},
		{http.MethodPost, "/admin/tenants", `{"name":"x"}`},
		{http.MethodGet, "/admin/tenants/" + tenantID, ""},
		{http.MethodPatch, "/admin/tenants/" + tenantID, `{"max_sandboxes":1}`},
		{http.MethodDelete, "/admin/tenants/" + tenantID, ""},
		{http.MethodPost, "/admin/tenants/" + tenantID + "/keys", ""},
		{http.MethodDelete, "/admin/tenants/" + tenantID + "/keys/" + keyID, ""},
		{http.MethodGet, "/sandboxes", ""},
		{http.MethodPost, "/sandboxes", `{}`},
		{http.MethodGet, "/sandboxes/" + sandboxID, ""},
		{http.MethodDelete, "/sandboxes/" + sandboxID, ""},
		{http.MethodPost, "/sandboxes/" + sandboxID + "/executions", `{"command":"id"}`},
		{http.MethodGet, "/sandboxes/" + sandboxID + "/files?path=/tmp", ""},
	}
	for _, rt := range routes {
		rr := doJSON(t, router, rt.method, rt.path, scalerKey, rt.body)
		if rr.Code != http.StatusForbidden {
			t.Errorf("%s %s: expected %d, got %d body=%s", rt.method, rt.path, http.StatusForbidden, rr.Code, rr.Body.String())
		}
	}
}

func TestRequireAdminRejectsScaler(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/admin/scaler", nil)
	req = req.WithContext(withAuthIdentity(req.Context(), authIdentity{Role: roleScaler}))
	rr := httptest.NewRecorder()
	if requireAdmin(rr, req) || rr.Code != http.StatusForbidden {
		t.Fatalf("expected requireAdmin to reject a scaler, got %d", rr.Code)
	}
}

func TestSetRunnerCordon(t *testing.T) {
	router, reg := newRunnersGateway(t)
	fake := &fakeSandboxControl{}
	reg.Upsert("r-a", "https://127.0.0.1:9", startFakeRunnerControl(t, fake), true, false, 8, 2, 0)

	for _, step := range []struct {
		method, key string
		want        bool
	}{
		{http.MethodPut, provAdminKey, true},
		{http.MethodPut, scalerKey, true},
		{http.MethodDelete, scalerKey, false},
	} {
		rr := doJSON(t, router, step.method, "/admin/runners/r-a/cordon", step.key, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d body=%s", step.method, rr.Code, rr.Body.String())
		}
		var got runnerCordonResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.ID != "r-a" || got.Cordoned != step.want {
			t.Fatalf("%s: response = %+v, want r-a cordoned=%v", step.method, got, step.want)
		}
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if want := []bool{true, true, false}; !slices.Equal(fake.cordonCalls, want) {
		t.Fatalf("SetCordon calls = %v, want %v", fake.cordonCalls, want)
	}
	for _, key := range fake.cordonKeys {
		if key != "runner-key" {
			t.Fatalf("SetCordon reached the runner with x-api-key %q, want the runner key", key)
		}
	}
	for _, id := range fake.cordonIDs {
		if id != "r-a" {
			t.Fatalf("SetCordon named runner %q, want r-a", id)
		}
	}
}

func TestSetRunnerCordonErrors(t *testing.T) {
	router, reg := newRunnersGateway(t)
	refusing := &fakeSandboxControl{cordonErr: status.Error(codes.Unauthenticated, "invalid api key")}
	reg.Upsert("r-refusing", "https://127.0.0.1:9", startFakeRunnerControl(t, refusing), true, false, 8, 0, 0)
	reg.Upsert("r-gone", "https://127.0.0.1:9", "127.0.0.1:1", true, false, 8, 0, 0)
	// Another runner answers at the address the registry has for this one.
	moved := &fakeSandboxControl{cordonErr: status.Error(codes.FailedPrecondition, `this is runner "r-new"`)}
	reg.Upsert("r-moved", "https://127.0.0.1:9", startFakeRunnerControl(t, moved), true, false, 8, 0, 0)
	// No heartbeat for longer than the grace: the API does not call it.
	stale := &fakeSandboxControl{}
	reg.Upsert("r-stale", "https://127.0.0.1:9", startFakeRunnerControl(t, stale), true, false, 8, 0, 0)
	if run, ok := reg.Get("r-stale"); ok {
		run.LastSeen = time.Now().Add(-time.Hour)
	}

	for _, tc := range []struct {
		runner string
		want   int
	}{
		{"r-unknown", http.StatusNotFound},
		{"r-refusing", http.StatusBadGateway},
		{"r-gone", http.StatusServiceUnavailable},
		{"r-moved", http.StatusConflict},
		{"r-stale", http.StatusServiceUnavailable},
	} {
		rr := doJSON(t, router, http.MethodPut, "/admin/runners/"+tc.runner+"/cordon", provAdminKey, "")
		if rr.Code != tc.want {
			t.Errorf("%s: expected %d, got %d body=%s", tc.runner, tc.want, rr.Code, rr.Body.String())
		}
	}
	stale.mu.Lock()
	defer stale.mu.Unlock()
	if len(stale.cordonCalls) != 0 {
		t.Fatalf("the API called a stale runner: %v", stale.cordonCalls)
	}
}

func TestSetRunnerCordonRejectsProvisionerAndTenantKeys(t *testing.T) {
	router, reg := newRunnersGateway(t)
	fake := &fakeSandboxControl{}
	reg.Upsert("r-a", "https://127.0.0.1:9", startFakeRunnerControl(t, fake), true, false, 8, 2, 0)
	created := provisionerCreateTenant(t, router, `{"name":"inst","external_ref":"inst-1"}`)
	if created.Key == nil {
		t.Fatal("expected a tenant key")
	}

	for name, key := range map[string]string{"provisioner": provKey, "tenant": created.Key.APIKey} {
		for _, method := range []string{http.MethodPut, http.MethodDelete} {
			rr := doJSON(t, router, method, "/admin/runners/r-a/cordon", key, "")
			if rr.Code != http.StatusForbidden {
				t.Errorf("%s key %s: expected 403, got %d body=%s", name, method, rr.Code, rr.Body.String())
			}
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.cordonCalls) != 0 {
		t.Fatalf("a rejected caller reached the runner: %v", fake.cordonCalls)
	}
}

func TestScalerMayCall(t *testing.T) {
	for path, want := range map[string]bool{
		"/admin/runners":                    true,
		"/admin/runners/r-a/cordon":         true,
		"/admin/runners/":                   false,
		"/admin/runners/a/b/cordon":         false,
		"/admin/runners/r-a/cordon/x":       false,
		"/admin/runners/r-a":                false,
		"/admin/runnersx/r-a/cordon":        false,
		"/admin/tenants/r-a/cordon":         false,
		"/admin/runners/../tenants/cordon":  false,
		"/admin/runners/r-a/../../x/cordon": false,
	} {
		if got := scalerMayCall(path); got != want {
			t.Errorf("scalerMayCall(%q) = %v, want %v", path, got, want)
		}
	}
}
