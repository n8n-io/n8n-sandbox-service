package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/n8n-io/sandbox-service/internal/api/store"
)

// The /sandboxes routes serve general sandboxes only: a code sandbox is not
// found, even by its owner, and nothing reaches its runner.
func TestGeneralRoutesDoNotServeCodeSandboxes(t *testing.T) {
	var upstreamHits atomic.Int64
	runner := newTestRunnerServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer runner.Close()

	router, s := newTestGateway(t, "admin-key")
	codeID, generalID := "c0de0000-0000-0000-0000-000000000001", "def00000-0000-0000-0000-000000000001"
	for _, rec := range []*store.SandboxRecord{
		{ID: codeID, Kind: store.KindCode, Name: "n"},
		{ID: generalID, Kind: store.KindGeneral},
	} {
		rec.Status, rec.CreatedAt, rec.LastActiveAt = "running", 1, 1
		rec.TenantID, rec.RunnerHTTPBase = store.AdminTenantID, runner.URL
		if err := s.Create(rec); err != nil {
			t.Fatalf("create %s: %v", rec.ID, err)
		}
	}

	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("X-Api-Key", "admin-key")
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}

	for _, route := range []struct{ method, path, body string }{
		{http.MethodGet, "", ""},
		{http.MethodDelete, "", ""},
		{http.MethodPost, "/executions", `{"command":"echo"}`},
		{http.MethodGet, "/files", ""},
		{http.MethodPut, "/files?path=/tmp/x", "x"},
		{http.MethodDelete, "/executions/exec-1", ""},
	} {
		if rr := call(route.method, "/sandboxes/"+codeID+route.path, route.body); rr.Code != http.StatusNotFound {
			t.Errorf("%s /sandboxes/{code id}%s: got %d %s, want 404", route.method, route.path, rr.Code, rr.Body.String())
		}
	}
	if hits := upstreamHits.Load(); hits != 0 {
		t.Fatalf("%d requests for a code sandbox reached its runner", hits)
	}
	if got, err := s.GetCodeSandbox(codeID); err != nil || got == nil {
		t.Fatalf("code sandbox after DELETE /sandboxes/{id}: %+v err=%v, want kept", got, err)
	}

	rr := call(http.MethodGet, "/sandboxes", "")
	var listed []SandboxResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil || rr.Code != http.StatusOK {
		t.Fatalf("list: %d %s err=%v", rr.Code, rr.Body.String(), err)
	}
	if len(listed) != 1 || listed[0].ID != generalID {
		t.Fatalf("list: %+v, want only the general sandbox", listed)
	}

	// The id of a code sandbox cannot be taken by a general sandbox.
	if rr := call(http.MethodPost, "/sandboxes", `{"id":"`+codeID+`"}`); rr.Code != http.StatusConflict {
		t.Fatalf("create with a code sandbox's id: got %d %s, want 409", rr.Code, rr.Body.String())
	}

	// The routes still serve general sandboxes.
	if rr := call(http.MethodGet, "/sandboxes/"+generalID+"/files", ""); rr.Code != http.StatusOK || upstreamHits.Load() != 1 {
		t.Fatalf("general sandbox proxy: got %d, %d upstream hits", rr.Code, upstreamHits.Load())
	}
}
