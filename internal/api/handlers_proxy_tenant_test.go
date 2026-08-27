package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/api/store"
)

// TestTenantCannotProxyToOtherTenantSandbox covers the exec and file routes,
// which unlike GET/DELETE forward to a runner. The upstream counter proves the
// tenant check runs before anything leaves the API, not just that the status
// code looks right.
func TestTenantCannotProxyToOtherTenantSandbox(t *testing.T) {
	var upstreamHits atomic.Int64
	runner := newTestRunnerServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer runner.Close()

	router, s := newTestGateway(t, "admin-key")
	a := mintTenantKey(t, router, `{"name":"a"}`)
	b := mintTenantKey(t, router, `{"name":"b"}`)
	keyA, keyB := a.Key.APIKey, b.Key.APIKey

	sid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	if err := s.Create(&store.SandboxRecord{
		ID: sid, Status: "running", CreatedAt: 1, LastActiveAt: 1,
		TenantID: a.Tenant.ID, RunnerHTTPBase: runner.URL,
	}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	missing := "11111111-2222-3333-4444-555555555555"
	routes := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/executions", `{"command":"echo pwned"}`},
		{http.MethodGet, "/executions/exec-1", ""},
		{http.MethodDelete, "/executions/exec-1", ""},
		{http.MethodGet, "/files", ""},
		{http.MethodGet, "/files/content?path=/tmp/x", ""},
		{http.MethodPut, "/files?path=/tmp/x", "pwned"},
		{http.MethodPost, "/files?path=/tmp/x", "pwned"},
		{http.MethodDelete, "/files?path=/tmp/x", ""},
		{http.MethodPost, "/files/copy", `{"src":"/tmp/a","dest":"/tmp/b"}`},
		{http.MethodPost, "/files/move", `{"src":"/tmp/a","dest":"/tmp/b"}`},
		{http.MethodPost, "/mkdir?path=/tmp/pwned", ""},
		{http.MethodGet, "/stat?path=/tmp/x", ""},
	}

	call := func(method, path, key, body string) *httptest.ResponseRecorder {
		t.Helper()
		var req *http.Request
		if body == "" {
			req = httptest.NewRequest(method, path, nil)
		} else {
			req = httptest.NewRequest(method, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("X-Api-Key", key)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}

	for _, route := range routes {
		foreign := call(route.method, "/sandboxes/"+sid+route.path, keyB, route.body)
		if foreign.Code != http.StatusNotFound {
			t.Errorf("%s %s as other tenant: expected %d, got %d body=%s",
				route.method, route.path, http.StatusNotFound, foreign.Code, foreign.Body.String())
		}

		// A sandbox that never existed must be indistinguishable from one the
		// caller may not see, or the status becomes an enumeration oracle.
		absent := call(route.method, "/sandboxes/"+missing+route.path, keyB, route.body)
		if absent.Code != foreign.Code || absent.Body.String() != foreign.Body.String() {
			t.Errorf("%s %s: cross-tenant (%d %s) differs from missing (%d %s)",
				route.method, route.path,
				foreign.Code, strings.TrimSpace(foreign.Body.String()),
				absent.Code, strings.TrimSpace(absent.Body.String()))
		}
	}

	if hits := upstreamHits.Load(); hits != 0 {
		t.Fatalf("expected no request to reach the runner, got %d", hits)
	}

	// The owner still gets through, so the assertion above is about
	// authorization rather than a proxy that never worked.
	owner := call(http.MethodGet, "/sandboxes/"+sid+"/files", keyA, "")
	if owner.Code != http.StatusOK {
		t.Fatalf("owner proxy: expected %d, got %d body=%s", http.StatusOK, owner.Code, owner.Body.String())
	}
	if hits := upstreamHits.Load(); hits != 1 {
		t.Fatalf("expected exactly one upstream request, got %d", hits)
	}
}
