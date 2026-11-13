package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/store"
)

func decodeCode(t *testing.T, rr *httptest.ResponseRecorder) codeSandboxResponse {
	t.Helper()
	var resp codeSandboxResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %s: %v", rr.Body.String(), err)
	}
	return resp
}

func deleteCodeName(t *testing.T, router http.Handler, name string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/v1/code/sandboxes/names/"+name, nil)
	req.Header.Set("X-Api-Key", "admin-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr.Code
}

func TestCreateCodeSandboxByName(t *testing.T) {
	fake := &fakeSandboxControl{}
	router, s, _ := newIdleTestGateway(t, "admin-key", fake)

	body := `{"name":"n1","slots":2,"labels":{"scopeKey":"w:1","n8nProcessId":"p1"}}`
	rr := postCode(t, router, body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("first create: %d %s", rr.Code, rr.Body.String())
	}
	first := decodeCode(t, rr)
	if first.Name != "n1" || first.Created == nil || !*first.Created {
		t.Fatalf("first response %+v", first)
	}

	// Another process: same name and options, other labels.
	rr = postCode(t, router, `{"name":"n1","slots":2,"labels":{"scopeKey":"w:1","n8nProcessId":"p2"}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("second create: %d %s", rr.Code, rr.Body.String())
	}
	second := decodeCode(t, rr)
	if second.ID != first.ID || second.Created == nil || *second.Created {
		t.Fatalf("second response %+v, want id %s", second, first.ID)
	}

	if rr := postCode(t, router, `{"name":"n1","slots":3}`); rr.Code != http.StatusConflict {
		t.Fatalf("other options: %d %s", rr.Code, rr.Body.String())
	}

	fake.mu.Lock()
	creates := len(fake.createOptionsJSON)
	fake.mu.Unlock()
	if creates != 1 {
		t.Fatalf("runner creates = %d, want 1", creates)
	}
	rec, _ := s.Get(first.ID)
	if rec.Name != "n1" || !strings.Contains(rec.Labels, `"p1"`) || rec.OptionsKey == "" {
		t.Fatalf("record %+v", rec)
	}
}

func TestCreateCodeSandboxByNameConcurrent(t *testing.T) {
	release := make(chan struct{})
	fake := &fakeSandboxControl{createHook: func(context.Context) { <-release }}
	router, _, _ := newIdleTestGateway(t, "admin-key", fake)

	const n = 5
	ids := make([]string, n)
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rr := postCode(t, router, `{"name":"same"}`)
			codes[i] = rr.Code
			ids[i] = decodeCode(t, rr).ID
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	created := 0
	for i := range n {
		if codes[i] == http.StatusCreated {
			created++
		} else if codes[i] != http.StatusOK {
			t.Fatalf("create %d: %d", i, codes[i])
		}
		if ids[i] != ids[0] {
			t.Fatalf("ids differ: %v", ids)
		}
	}
	if created != 1 {
		t.Fatalf("created = %d, want 1", created)
	}
}

func TestCreateCodeSandboxNameValidation(t *testing.T) {
	router, _, _ := newIdleTestGateway(t, "admin-key", &fakeSandboxControl{})
	for _, body := range []string{
		`{"name":"a/b"}`,
		`{"name":"` + strings.Repeat("a", 129) + `"}`,
		`{"idleTimeoutMs":10}`,
		`{"maxAgeMs":-1}`,
	} {
		if rr := postCode(t, router, body); rr.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, rr.Code, rr.Body.String())
		}
	}
}

func TestCreateCodeSandboxCapsIdleTimeout(t *testing.T) {
	router, s, cfg := newIdleTestGateway(t, "admin-key", &fakeSandboxControl{})
	cfg.CodeIdleDeleteAfter = 10 * time.Second
	rr := postCode(t, router, `{"idleTimeoutMs":600000,"maxAgeMs":60000}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	rec, _ := s.Get(decodeCode(t, rr).ID)
	if rec.IdleTimeoutMs != 10_000 {
		t.Fatalf("idle timeout = %d, want 10000", rec.IdleTimeoutMs)
	}
	if want := time.Now().Unix() + 60; rec.DrainAt < want-2 || rec.DrainAt > want {
		t.Fatalf("drain at = %d, want about %d", rec.DrainAt, want)
	}
}

func TestDeleteCodeSandboxByName(t *testing.T) {
	fake := &fakeSandboxControl{}
	router, s, _ := newIdleTestGateway(t, "admin-key", fake)

	first := decodeCode(t, postCode(t, router, `{"name":"e:1"}`))
	// A draining sandbox of the name is deleted too.
	if err := s.MarkDraining(first.ID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	second := decodeCode(t, postCode(t, router, `{"name":"e:1"}`))
	if second.ID == first.ID {
		t.Fatal("draining sandbox was returned for its name")
	}
	other := decodeCode(t, postCode(t, router, `{"name":"e:2"}`))

	if code := deleteCodeName(t, router, "e:1"); code != http.StatusNoContent {
		t.Fatalf("delete by name: %d", code)
	}
	for _, id := range []string{first.ID, second.ID} {
		if rec, _ := s.Get(id); rec != nil {
			t.Fatalf("sandbox %s of the name not deleted", id)
		}
	}
	if rec, _ := s.Get(other.ID); rec == nil {
		t.Fatal("sandbox of another name was deleted")
	}
	// Deleting a name without sandboxes is fine.
	if code := deleteCodeName(t, router, "e:1"); code != http.StatusNoContent {
		t.Fatalf("repeat delete: %d", code)
	}
	third := postCode(t, router, `{"name":"e:1"}`)
	if third.Code != http.StatusCreated {
		t.Fatalf("create after delete: %d", third.Code)
	}
}

func TestDeleteCodeSandboxByNameOtherTenant(t *testing.T) {
	router, s, _ := newIdleTestGateway(t, "admin-key", &fakeSandboxControl{})
	sb := decodeCode(t, postCode(t, router, `{"name":"shared"}`))
	other := mintTenantKey(t, router, `{"name":"other"}`)
	req := httptest.NewRequest(http.MethodDelete, "/v1/code/sandboxes/names/shared", nil)
	req.Header.Set("X-Api-Key", other.Key.APIKey)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rr.Code)
	}
	if rec, _ := s.Get(sb.ID); rec == nil {
		t.Fatal("other tenant deleted the sandbox")
	}
}

func TestCodeTaskOnDrainingSandbox(t *testing.T) {
	router, s, _ := newIdleTestGateway(t, "admin-key", &fakeSandboxControl{})
	sb := decodeCode(t, postCode(t, router, `{"name":"w:1","maxAgeMs":60000}`))
	rec, _ := s.Get(sb.ID)
	if err := s.SetCodeSettings(sb.ID, store.CodeSettings{Name: rec.Name, OptionsKey: rec.OptionsKey, DrainAt: time.Now().Unix() - 1}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/code/sandboxes/"+sb.ID+"/tasks", strings.NewReader("x"))
	req.Header.Set("X-Api-Key", "admin-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusGone {
		t.Fatalf("task on draining sandbox: %d %s", rr.Code, rr.Body.String())
	}

	// The name moves to a new sandbox at the next create.
	rr = postCode(t, router, `{"name":"w:1","maxAgeMs":60000}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create after max age: %d %s", rr.Code, rr.Body.String())
	}
	if decodeCode(t, rr).ID == sb.ID {
		t.Fatal("name did not move")
	}
	old, _ := s.Get(sb.ID)
	if old == nil || old.DrainingSince == 0 {
		t.Fatalf("old sandbox %+v, want draining", old)
	}
}

func TestCodeSweeperLifecycle(t *testing.T) {
	fake := &fakeSandboxControl{}
	_, s, cfg := newIdleTestGateway(t, "admin-key", fake)
	reg := registry.New(45 * time.Second)
	reg.Upsert("runner-1", "https://127.0.0.1:9", startFakeRunnerControl(t, fake), true, 10, 0, 0)

	now := time.Now()
	recent := now.Add(-5 * time.Second).Unix()
	mk := func(id string, mod func(*store.SandboxRecord)) {
		r := &store.SandboxRecord{ID: id, Status: "running", CreatedAt: recent, LastActiveAt: recent,
			TenantID: store.AdminTenantID, RunnerID: "runner-1", Kind: store.KindCode, Ephemeral: true, Egress: "none"}
		mod(r)
		if err := s.Create(r); err != nil {
			t.Fatal(err)
		}
	}
	shortIdle := "11111111-1111-4111-8111-111111111111"
	expired := "22222222-2222-4222-8222-222222222222"
	drained := "33333333-3333-4333-8333-333333333333"
	busyDrained := "44444444-4444-4444-8444-444444444444"
	fresh := "55555555-5555-4555-8555-555555555555"
	mk(shortIdle, func(r *store.SandboxRecord) { r.IdleTimeoutMs = 2000 })
	mk(expired, func(r *store.SandboxRecord) { r.DrainAt = now.Unix() - 1 })
	mk(drained, func(r *store.SandboxRecord) { r.DrainingSince = now.Unix() - 1 })
	mk(busyDrained, func(r *store.SandboxRecord) { r.DrainingSince = now.Unix() - 1 })
	mk(fresh, func(r *store.SandboxRecord) { r.DrainAt = now.Unix() + 60 })

	activity := newCodeActivity()
	activity.begin(busyDrained)
	sweepIdleCodeSandboxes(context.Background(), s, reg, cfg, runnerControlTLS(cfg), activity, 2*time.Minute, now)

	for _, id := range []string{shortIdle, drained} {
		if rec, _ := s.Get(id); rec != nil {
			t.Errorf("sandbox %s not deleted", id)
		}
	}
	if rec, _ := s.Get(expired); rec == nil || rec.DrainingSince == 0 {
		t.Errorf("expired sandbox %+v, want draining", rec)
	}
	for _, id := range []string{busyDrained, fresh} {
		if rec, _ := s.Get(id); rec == nil {
			t.Errorf("sandbox %s deleted", id)
		}
	}
}
