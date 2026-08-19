package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/store"
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
