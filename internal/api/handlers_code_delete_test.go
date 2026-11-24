package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/n8n-io/sandbox-service/internal/api/store"
)

func deleteCodeSandboxRequest(t *testing.T, router http.Handler, apiKey, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	req.Header.Set("X-Api-Key", apiKey)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

// seedCodeSandbox saves a code sandbox of the tenant whose runner control
// address is controlAddr, and returns its id.
func seedCodeSandbox(t *testing.T, s store.SandboxStore, tenantID, name, controlAddr string, drainingSince int64) string {
	t.Helper()
	now := time.Now().Unix()
	id := uuid.New().String()
	if err := s.Create(&store.SandboxRecord{
		ID:                    id,
		Status:                "running",
		CreatedAt:             now,
		LastActiveAt:          now,
		RunnerHTTPBase:        "https://127.0.0.1:9",
		RunnerControlGRPCAddr: controlAddr,
		TenantID:              tenantID,
		Ephemeral:             true,
		Kind:                  store.KindCode,
		Name:                  name,
		OptionsHash:           "h",
		DrainingSince:         drainingSince,
	}); err != nil {
		t.Fatalf("seed code sandbox: %v", err)
	}
	return id
}

func createCodeSandboxForTest(t *testing.T, router http.Handler, apiKey string) string {
	t.Helper()
	rr := postCreateCodeSandbox(t, router, apiKey, codeCreateBody)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: got %d body=%s", rr.Code, rr.Body.String())
	}
	return decodeCodeSandbox(t, rr).ID
}

func TestDeleteCodeSandboxByID(t *testing.T) {
	fake := &fakeSandboxControl{}
	router, s, _ := newIdleTestGateway(t, "admin-key", fake)
	tenant := mintTenantKey(t, router, `{"name":"code"}`)
	id := createCodeSandboxForTest(t, router, tenant.Key.APIKey)

	rr := deleteCodeSandboxRequest(t, router, tenant.Key.APIKey, "/code/sandboxes/"+id)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete: got %d body=%s, want %d", rr.Code, rr.Body.String(), http.StatusNoContent)
	}
	if _, deleted := fake.calls(); !slices.Equal(deleted, []string{id}) {
		t.Fatalf("runner deletes = %v, want [%s]", deleted, id)
	}
	if mustGet(t, s, id) != nil {
		t.Fatal("record still stored")
	}

	rr = deleteCodeSandboxRequest(t, router, tenant.Key.APIKey, "/code/sandboxes/"+id)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("second delete: got %d, want %d", rr.Code, http.StatusNotFound)
	}
	rr = postTask(router, id, "admin-key", "spec")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("task after delete: got %d, want %d", rr.Code, http.StatusNotFound)
	}
}

func TestDeleteCodeSandboxByIDDeletesDrainingSandbox(t *testing.T) {
	fake := &fakeSandboxControl{}
	router, s, _ := newIdleTestGateway(t, "admin-key", fake)
	id := seedCodeSandbox(t, s, store.AdminTenantID, "n", startFakeRunnerControl(t, fake), time.Now().Unix())

	rr := deleteCodeSandboxRequest(t, router, "admin-key", "/code/sandboxes/"+id)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete: got %d body=%s, want %d", rr.Code, rr.Body.String(), http.StatusNoContent)
	}
	if mustGet(t, s, id) != nil {
		t.Fatal("record still stored")
	}
}

// The caller gets the same 404 for a sandbox that is missing, of another kind
// or of another tenant, and nothing is deleted.
func TestDeleteCodeSandboxByIDRefusesSandboxesTheCallerCannotUse(t *testing.T) {
	fake := &fakeSandboxControl{}
	router, s, _ := newIdleTestGateway(t, "admin-key", fake)
	owner := mintTenantKey(t, router, `{"name":"owner"}`)
	other := mintTenantKey(t, router, `{"name":"other"}`)
	codeID := createCodeSandboxForTest(t, router, owner.Key.APIKey)
	generalID := "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa"
	seedRunningSandbox(t, s, generalID, "127.0.0.1:9", time.Now().Unix(), false)

	for _, tc := range []struct {
		name, apiKey, id string
	}{
		{"missing", "admin-key", uuid.New().String()},
		{"general sandbox", "admin-key", generalID},
		{"other tenant", other.Key.APIKey, codeID},
		{"admin sandbox for a tenant", owner.Key.APIKey, seedCodeSandbox(t, s, store.AdminTenantID, "admin", "127.0.0.1:9", 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := deleteCodeSandboxRequest(t, router, tc.apiKey, "/code/sandboxes/"+tc.id)
			if rr.Code != http.StatusNotFound {
				t.Fatalf("got %d body=%s, want %d", rr.Code, rr.Body.String(), http.StatusNotFound)
			}
		})
	}
	if rr := deleteCodeSandboxRequest(t, router, "admin-key", "/code/sandboxes/not-a-uuid"); rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid id: got %d, want %d", rr.Code, http.StatusBadRequest)
	}
	if _, deleted := fake.calls(); len(deleted) != 0 {
		t.Fatalf("runner deletes = %v, want none", deleted)
	}
	if mustGet(t, s, codeID) == nil || mustGet(t, s, generalID) == nil {
		t.Fatal("a sandbox was removed")
	}
}

// A failed runner delete keeps the record, so a retry can delete the sandbox.
func TestDeleteCodeSandboxByIDKeepsRecordWhenRunnerFails(t *testing.T) {
	fake := &fakeSandboxControl{}
	router, s, _ := newIdleTestGateway(t, "admin-key", fake)
	id := createCodeSandboxForTest(t, router, "admin-key")
	fake.failDeletes(errors.New("runner down"))

	rr := deleteCodeSandboxRequest(t, router, "admin-key", "/code/sandboxes/"+id)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("delete: got %d body=%s, want %d", rr.Code, rr.Body.String(), http.StatusBadGateway)
	}
	if mustGet(t, s, id) == nil {
		t.Fatal("record removed although the runner delete failed")
	}

	fake.failDeletes(nil)
	if rr := deleteCodeSandboxRequest(t, router, "admin-key", "/code/sandboxes/"+id); rr.Code != http.StatusNoContent {
		t.Fatalf("retry: got %d body=%s, want %d", rr.Code, rr.Body.String(), http.StatusNoContent)
	}
}

// Delete by name removes the current sandbox and the draining ones of the
// caller's tenant, and leaves the same name of other tenants alone.
func TestDeleteCodeSandboxByNameDeletesDrainingSandboxes(t *testing.T) {
	fake := &fakeSandboxControl{}
	router, s, _ := newIdleTestGateway(t, "admin-key", fake)
	tenant := mintTenantKey(t, router, `{"name":"code"}`)
	other := mintTenantKey(t, router, `{"name":"other"}`)
	controlAddr := startFakeRunnerControl(t, fake)

	current := createCodeSandboxForTest(t, router, tenant.Key.APIKey)
	drained := seedCodeSandbox(t, s, tenant.Tenant.ID, "scope-1", controlAddr, time.Now().Unix())
	otherTenant := createCodeSandboxForTest(t, router, other.Key.APIKey)
	adminSandbox := createCodeSandboxForTest(t, router, "admin-key")
	otherName := seedCodeSandbox(t, s, tenant.Tenant.ID, "scope-2", controlAddr, 0)

	rr := deleteCodeSandboxRequest(t, router, tenant.Key.APIKey, "/code/sandboxes/names/scope-1")
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete: got %d body=%s, want %d", rr.Code, rr.Body.String(), http.StatusNoContent)
	}
	_, deleted := fake.calls()
	slices.Sort(deleted)
	want := []string{current, drained}
	slices.Sort(want)
	if !slices.Equal(deleted, want) {
		t.Fatalf("runner deletes = %v, want %v", deleted, want)
	}
	for _, id := range want {
		if mustGet(t, s, id) != nil {
			t.Fatalf("sandbox %s still stored", id)
		}
	}
	for _, id := range []string{otherTenant, adminSandbox, otherName} {
		if mustGet(t, s, id) == nil {
			t.Fatalf("sandbox %s removed", id)
		}
	}

	rr = deleteCodeSandboxRequest(t, router, tenant.Key.APIKey, "/code/sandboxes/names/scope-1")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("second delete: got %d, want %d", rr.Code, http.StatusNotFound)
	}
}

// Admin keys delete by name in the admin pseudo-tenant only.
func TestDeleteCodeSandboxByNameAsAdmin(t *testing.T) {
	fake := &fakeSandboxControl{}
	router, s, _ := newIdleTestGateway(t, "admin-key", fake)
	tenant := mintTenantKey(t, router, `{"name":"code"}`)
	tenantSandbox := createCodeSandboxForTest(t, router, tenant.Key.APIKey)
	adminSandbox := createCodeSandboxForTest(t, router, "admin-key")

	rr := deleteCodeSandboxRequest(t, router, "admin-key", "/code/sandboxes/names/scope-1")
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete: got %d body=%s, want %d", rr.Code, rr.Body.String(), http.StatusNoContent)
	}
	if mustGet(t, s, adminSandbox) != nil {
		t.Fatal("admin sandbox still stored")
	}
	if mustGet(t, s, tenantSandbox) == nil {
		t.Fatal("tenant sandbox removed")
	}
}

func TestDeleteCodeSandboxByNameRefusals(t *testing.T) {
	fake := &fakeSandboxControl{}
	router, _, _ := newIdleTestGateway(t, "admin-key", fake)

	for _, tc := range []struct {
		name, path string
		want       int
	}{
		{"unknown name", "/code/sandboxes/names/missing", http.StatusNotFound},
		{"invalid name", "/code/sandboxes/names/not*a*name", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := deleteCodeSandboxRequest(t, router, "admin-key", tc.path)
			if rr.Code != tc.want {
				t.Fatalf("got %d body=%s, want %d", rr.Code, rr.Body.String(), tc.want)
			}
		})
	}
}

// One failed runner delete does not stop the others. The answer is 502 and
// the failed record stays for a retry.
func TestDeleteCodeSandboxByNameTriesEverySandbox(t *testing.T) {
	fake := &fakeSandboxControl{}
	router, s, _ := newIdleTestGateway(t, "admin-key", fake)
	failing := &fakeSandboxControl{}
	failing.failDeletes(errors.New("runner down"))

	current := createCodeSandboxForTest(t, router, "admin-key")
	stuck := seedCodeSandbox(t, s, store.AdminTenantID, "scope-1", startFakeRunnerControl(t, failing), time.Now().Unix())

	rr := deleteCodeSandboxRequest(t, router, "admin-key", "/code/sandboxes/names/scope-1")
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("delete: got %d body=%s, want %d", rr.Code, rr.Body.String(), http.StatusBadGateway)
	}
	if mustGet(t, s, current) != nil {
		t.Fatal("sandbox on the working runner still stored")
	}
	if mustGet(t, s, stuck) == nil {
		t.Fatal("record removed although its runner delete failed")
	}

	failing.failDeletes(nil)
	rr = deleteCodeSandboxRequest(t, router, "admin-key", "/code/sandboxes/names/scope-1")
	if rr.Code != http.StatusNoContent {
		t.Fatalf("retry: got %d body=%s, want %d", rr.Code, rr.Body.String(), http.StatusNoContent)
	}
	if mustGet(t, s, stuck) != nil {
		t.Fatal("record still stored after the retry")
	}
}
