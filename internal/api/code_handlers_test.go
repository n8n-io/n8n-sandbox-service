package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/store"
)

func postCode(t *testing.T, router http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/code/sandboxes", strings.NewReader(body))
	req.Header.Set("X-Api-Key", "admin-key")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

func TestCreateCodeSandbox(t *testing.T) {
	fake := &fakeSandboxControl{}
	router, s, _ := newIdleTestGateway(t, "admin-key", fake)

	rr := postCode(t, router, `{"runtime":"n8n-js","slots":2,"taskMemoryMb":256,"spareWorkers":true,"reuseWorkers":true,"labels":{"scope":"execution"}}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var resp codeSandboxResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Slots != 2 || resp.State != "ready" || resp.Timings == nil {
		t.Fatalf("response %+v", resp)
	}
	fake.mu.Lock()
	opts := fake.createOptionsJSON[0]
	fake.mu.Unlock()
	for _, want := range []string{`"kind":"code"`, `"egress":"none"`, `"slots":2`, `"taskMemoryMb":256`, `"spareWorkers":true`, `"reuseWorkers":true`} {
		if !strings.Contains(opts, want) {
			t.Errorf("create options %s miss %s", opts, want)
		}
	}
	rec, err := s.Get(resp.ID)
	if err != nil || rec == nil {
		t.Fatalf("record: %v %v", rec, err)
	}
	if rec.Kind != store.KindCode || !rec.Ephemeral || rec.Egress != "none" {
		t.Fatalf("record %+v", rec)
	}

	// The general routes do not serve code sandboxes.
	req := httptest.NewRequest(http.MethodPost, "/sandboxes/"+resp.ID+"/executions", strings.NewReader(`{}`))
	req.Header.Set("X-Api-Key", "admin-key")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("exec on code sandbox: %d", rr.Code)
	}
}

func TestCreateCodeSandboxValidation(t *testing.T) {
	router, _, _ := newIdleTestGateway(t, "admin-key", &fakeSandboxControl{})
	for _, body := range []string{`{"slots":99}`, `{"taskMemoryMb":1}`, `{"runtime":"Bad Runtime"}`} {
		if rr := postCode(t, router, body); rr.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, rr.Code, rr.Body.String())
		}
	}
}

func TestCreateCodeSandboxRunnerWithoutCodeSupport(t *testing.T) {
	// A runner from before code sandboxes ignores the code fields.
	fake := &fakeSandboxControl{applied: func(string) string { return `{"egress":"none"}` }}
	router, s, _ := newIdleTestGateway(t, "admin-key", fake)
	if rr := postCode(t, router, `{}`); rr.Code != http.StatusBadGateway {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	fake.mu.Lock()
	deleted := len(fake.deleted)
	fake.mu.Unlock()
	if deleted != 1 {
		t.Fatalf("rollback deletes = %d, want 1", deleted)
	}
	if n, _ := s.Count(); n != 0 {
		t.Fatalf("records = %d, want 0", n)
	}
}

func TestCodeRoutesRefuseGeneralSandbox(t *testing.T) {
	router, s, _ := newIdleTestGateway(t, "admin-key", &fakeSandboxControl{})
	sid := "77777777-7777-4777-8777-777777777777"
	if err := s.Create(&store.SandboxRecord{ID: sid, Status: "running", CreatedAt: 1, LastActiveAt: time.Now().Unix(),
		TenantID: store.AdminTenantID, RunnerHTTPBase: "https://127.0.0.1:9"}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodDelete, "/v1/code/sandboxes/"+sid, nil)
	req.Header.Set("X-Api-Key", "admin-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("delete general sandbox via Task API: %d", rr.Code)
	}
}

func TestCodeIdleSweeper(t *testing.T) {
	fake := &fakeSandboxControl{}
	_, s, cfg := newIdleTestGateway(t, "admin-key", fake)
	reg := registry.New(45 * time.Second)
	reg.Upsert("runner-1", "https://127.0.0.1:9", startFakeRunnerControl(t, fake), true, 10, 0, 0)

	old := time.Now().Add(-10 * time.Minute).Unix()
	idle := "88888888-8888-4888-8888-888888888888"
	busy := "99999999-9999-4999-8999-999999999999"
	for _, id := range []string{idle, busy} {
		if err := s.Create(&store.SandboxRecord{ID: id, Status: "running", CreatedAt: old, LastActiveAt: old,
			TenantID: store.AdminTenantID, RunnerID: "runner-1", Kind: store.KindCode, Ephemeral: true, Egress: "none"}); err != nil {
			t.Fatal(err)
		}
	}
	activity := newCodeActivity()
	activity.begin(busy, true)
	sweepIdleCodeSandboxes(context.Background(), s, reg, cfg, runnerControlTLS(cfg), activity, 2*time.Minute, time.Now())

	if rec, _ := s.Get(idle); rec != nil {
		t.Fatalf("idle code sandbox not deleted")
	}
	if rec, _ := s.Get(busy); rec == nil {
		t.Fatalf("code sandbox with an open channel was deleted")
	}
	activity.end(busy, true)
	sweepIdleCodeSandboxes(context.Background(), s, reg, cfg, runnerControlTLS(cfg), activity, 2*time.Minute, time.Now())
	if rec, _ := s.Get(busy); rec != nil {
		t.Fatalf("code sandbox not deleted after its channel closed")
	}
}
