package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const proxySpec = `{"allow":[{"name":"git","hosts":["git.example.com"]}],` +
	`"repos":[{"host":"git.example.com","path":"org/repo","access":"write"}],` +
	`"push_refs":["refs/heads/factory/i1/"],` +
	`"credentials":[{"host":"git.example.com","secret_ref":"factory/git-token"}]}`

// template and the idle windows are validated, stored and returned; the
// windows never reach the runner, and the default template goes as empty so
// a runner from before templates confirms it.
func TestCreateSandboxTemplateAndIdleWindows(t *testing.T) {
	ctl := &fakeSandboxControl{}
	router, s, cfg := newIdleTestGateway(t, "admin-key", ctl)
	cfg.MaxIdleWindow = 7 * 24 * time.Hour

	const sid = "a1a1a1a1-a1a1-4a1a-8a1a-a1a1a1a1a1a1"
	rr := postCreateSandbox(t, router, "admin-key",
		`{"id":"`+sid+`","template":"default","idle_stop_after":1800,"idle_delete_after":604800}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: got %d body=%s", rr.Code, rr.Body.String())
	}
	var resp SandboxResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Template != "default" || resp.IdleStopAfter != 1800 || resp.IdleDeleteAfter != 604800 {
		t.Fatalf("response = %+v", resp)
	}
	rec, err := s.Get(sid)
	if err != nil || rec == nil || rec.Template != "default" || rec.IdleStopAfter != 1800 || rec.IdleDeleteAfter != 604800 {
		t.Fatalf("stored row = %+v err=%v", rec, err)
	}
	ctl.mu.Lock()
	sent := append([]string(nil), ctl.createOptionsJSON...)
	ctl.mu.Unlock()
	if len(sent) != 1 || sent[0] != `{"egress":"public"}` {
		t.Fatalf("runner create_options_json = %q, want only the egress", sent)
	}

	// A create that names none of them reports the default template and no
	// per-sandbox windows.
	rr = postCreateSandbox(t, router, "admin-key", "")
	if rr.Code != http.StatusCreated || !strings.Contains(rr.Body.String(), `"template":"default"`) || strings.Contains(rr.Body.String(), "idle_") {
		t.Fatalf("default create: got %d body=%s", rr.Code, rr.Body.String())
	}
}

// Every refusal comes before the runner is asked for anything.
func TestCreateSandboxRefusesBadCreateOptions(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		maxIdle    time.Duration
	}{
		{name: "unknown template", body: `{"template":"python"}`},
		{name: "malformed template", body: `{"template":"../x"}`},
		{name: "idle stop below minimum", body: `{"idle_stop_after":59}`},
		{name: "idle stop zero", body: `{"idle_stop_after":0}`},
		{name: "idle delete over cap", body: `{"idle_delete_after":86401}`},
		{name: "idle stop after delete", body: `{"idle_stop_after":7200,"idle_delete_after":3600}`},
		{name: "idle windows disabled", body: `{"idle_stop_after":600}`, maxIdle: -1},
		{name: "idle not an integer", body: `{"idle_stop_after":"10m"}`},
		{name: "proxy without spec", body: `{"egress":"proxy"}`},
		{name: "proxy with null spec", body: `{"egress":"proxy","egress_proxy":null}`},
		{name: "spec without proxy", body: `{"egress":"none","egress_proxy":` + proxySpec + `}`},
		{name: "spec unknown field", body: `{"egress":"proxy","egress_proxy":{"allow":[],"allow_all":true}}`},
		{name: "spec ip literal", body: `{"egress":"proxy","egress_proxy":{"allow":[{"name":"x","hosts":["169.254.169.254"]}]}}`},
		{name: "spec repo path with .git", body: `{"egress":"proxy","egress_proxy":{"allow":[{"name":"g","hosts":["git.example.com"]}],"repos":[{"host":"git.example.com","path":"o/r.git","access":"read"}]}}`},
		{name: "spec label forging a log line", body: `{"egress":"proxy","egress_proxy":{"allow":[],"labels":{"session_id":"a\nlevel=error"}}}`},
		{name: "spec repo host not allowed", body: `{"egress":"proxy","egress_proxy":{"allow":[],"repos":[{"host":"git.example.com","path":"o/r","access":"read"}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctl := &fakeSandboxControl{}
			router, s, cfg := newIdleTestGateway(t, "admin-key", ctl)
			cfg.MaxIdleWindow = 24 * time.Hour
			if tc.maxIdle < 0 {
				cfg.MaxIdleWindow = 0
			}
			rr := postCreateSandbox(t, router, "admin-key", tc.body)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("got %d body=%s, want 400", rr.Code, rr.Body.String())
			}
			wantReason := ""
			if tc.name == "unknown template" {
				wantReason = reasonUnknownTemplate
			}
			var body APIError
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || body.Reason != wantReason {
				t.Fatalf("reason = %q, want %q (body %s)", body.Reason, wantReason, rr.Body.String())
			}
			ctl.mu.Lock()
			calls := len(ctl.createOptionsJSON)
			ctl.mu.Unlock()
			if n, _ := s.Count(); n != 0 || calls != 0 {
				t.Fatalf("stored %d sandboxes and made %d runner creates, want none", n, calls)
			}
		})
	}
}

// A valid proxy spec reaches the runner in canonical form. No runner serves
// it yet: the runner's Unimplemented is a 501 and nothing is stored.
func TestCreateSandboxEgressProxyNotImplemented(t *testing.T) {
	ctl := &fakeSandboxControl{createErr: status.Error(codes.Unimplemented, "egress proxy is not supported by this runner")}
	router, s, _ := newIdleTestGateway(t, "admin-key", ctl)

	rr := postCreateSandbox(t, router, "admin-key", `{"egress":"proxy","egress_proxy":`+proxySpec+`}`)
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("got %d body=%s, want 501", rr.Code, rr.Body.String())
	}
	var body APIError
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || body.Reason != reasonNotImplemented || body.Error != "the requested egress is not supported by the sandbox runners" {
		t.Fatalf("body = %s", rr.Body.String())
	}
	if n, _ := s.Count(); n != 0 {
		t.Fatalf("stored %d sandboxes, want none", n)
	}
	ctl.mu.Lock()
	sent := append([]string(nil), ctl.createOptionsJSON...)
	ctl.mu.Unlock()
	if len(sent) != 1 || !strings.Contains(sent[0], `"egress":"proxy","egress_proxy":{"allow":[{"name":"git"`) {
		t.Fatalf("runner create_options_json = %q", sent)
	}
}

// A runner that echoes a proxy spec other than the one sent is refused like
// any other unconfirmed option.
func TestCreateSandboxRefusesUnconfirmedEgressProxy(t *testing.T) {
	ctl := &fakeSandboxControl{applied: func(sent string) string {
		return strings.Replace(sent, `"refs/heads/factory/i1/"`, `"refs/heads/"`, 1)
	}}
	router, s, _ := newIdleTestGateway(t, "admin-key", ctl)
	rr := postCreateSandbox(t, router, "admin-key", `{"egress":"proxy","egress_proxy":`+proxySpec+`}`)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("got %d body=%s, want 502", rr.Code, rr.Body.String())
	}
	if n, _ := s.Count(); n != 0 {
		t.Fatalf("stored %d sandboxes, want none", n)
	}
	if _, deleted := ctl.calls(); len(deleted) != 1 {
		t.Fatalf("runner deletes = %v, want the create rolled back", deleted)
	}
}

// Like egress, each option is fixed at creation: a reconnect naming another
// value is 409, one leaving it out gets the sandbox as it is.
func TestCreateSandboxReconnectRefusesOtherOptions(t *testing.T) {
	const sid = "b2b2b2b2-b2b2-4b2b-8b2b-b2b2b2b2b2b2"
	ctl := &fakeSandboxControl{}
	router, _, cfg := newIdleTestGateway(t, "admin-key", ctl)
	cfg.MaxIdleWindow = 24 * time.Hour
	if rr := postCreateSandbox(t, router, "admin-key", `{"id":"`+sid+`","idle_stop_after":600}`); rr.Code != http.StatusCreated {
		t.Fatalf("create: got %d body=%s", rr.Code, rr.Body.String())
	}
	for _, tc := range []struct {
		name, fields, reason string
		want                 int
	}{
		{name: "nothing named", fields: ``, want: http.StatusOK},
		{name: "same window", fields: `,"idle_stop_after":600`, want: http.StatusOK},
		{name: "default template", fields: `,"template":"default"`, want: http.StatusOK},
		{name: "empty template is default", fields: `,"template":""`, want: http.StatusOK},
		{name: "other stop window", fields: `,"idle_stop_after":900`, want: http.StatusConflict, reason: reasonIdleWindowMismatch},
		{name: "delete window named", fields: `,"idle_delete_after":3600`, want: http.StatusConflict, reason: reasonIdleWindowMismatch},
		{name: "other egress", fields: `,"egress":"none"`, want: http.StatusConflict, reason: reasonEgressMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := postCreateSandbox(t, router, "admin-key", `{"id":"`+sid+`"`+tc.fields+`}`)
			if rr.Code != tc.want {
				t.Fatalf("got %d body=%s, want %d", rr.Code, rr.Body.String(), tc.want)
			}
			var body APIError
			_ = json.Unmarshal(rr.Body.Bytes(), &body)
			if tc.want == http.StatusConflict && body.Reason != tc.reason {
				t.Fatalf("reason = %q, want %q", body.Reason, tc.reason)
			}
		})
	}
}

// A proxy spec fills more than the old 1 KiB body limit; the new limit still
// refuses an oversized body.
func TestCreateSandboxBodyLimit(t *testing.T) {
	ctl := &fakeSandboxControl{}
	router, _, _ := newIdleTestGateway(t, "admin-key", ctl)
	big := `{"egress":"public","pad":"` + strings.Repeat("x", 2048) + `"}`
	if rr := postCreateSandbox(t, router, "admin-key", big); rr.Code != http.StatusCreated {
		t.Fatalf("2 KiB body: got %d body=%s", rr.Code, rr.Body.String())
	}
	huge := `{"egress":"public","pad":"` + strings.Repeat("x", maxCreateBodyBytes) + `"}`
	if rr := postCreateSandbox(t, router, "admin-key", huge); rr.Code != http.StatusBadRequest {
		t.Fatalf("oversized body: got %d, want 400", rr.Code)
	}
}

// Drain is the runner's cordon plus what it still runs: drained only once
// nothing is left, and only an admin may see or change it.
func TestRunnerDrain(t *testing.T) {
	ctl := &fakeSandboxControl{executions: 1, streams: 1}
	router, _, _ := newIdleTestGateway(t, "admin-key", ctl)

	call := func(method, key, runner string) (int, drainState) {
		t.Helper()
		req := httptest.NewRequest(method, "/admin/runners/"+runner+"/drain", nil)
		req.Header.Set("X-Api-Key", key)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		var st drainState
		_ = json.Unmarshal(rr.Body.Bytes(), &st)
		return rr.Code, st
	}

	if code, st := call(http.MethodGet, "admin-key", "runner-1"); code != http.StatusOK || st.Draining || st.Drained {
		t.Fatalf("initial: %d %+v", code, st)
	}
	if code, st := call(http.MethodPost, "admin-key", "runner-1"); code != http.StatusOK || !st.Draining || st.Drained ||
		st.ActiveExecutions != 1 || st.OpenStreams != 1 || st.CapacityUsed != 3 || st.CapacityStopped != 1 || st.RunnerID != "runner-1" {
		t.Fatalf("start with a run in progress: %d %+v", code, st)
	}
	ctl.mu.Lock()
	ctl.executions, ctl.streams = 0, 0
	ctl.mu.Unlock()
	if code, st := call(http.MethodGet, "admin-key", "runner-1"); code != http.StatusOK || !st.Drained {
		t.Fatalf("after the run: %d %+v", code, st)
	}
	if code, st := call(http.MethodDelete, "admin-key", "runner-1"); code != http.StatusOK || st.Draining || st.Drained {
		t.Fatalf("stop: %d %+v", code, st)
	}
	if code, _ := call(http.MethodGet, "admin-key", "runner-9"); code != http.StatusNotFound {
		t.Fatalf("unknown runner: %d", code)
	}
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		if code, _ := call(method, "not-a-key", "runner-1"); code != http.StatusUnauthorized && code != http.StatusForbidden {
			t.Fatalf("%s without a valid key: %d", method, code)
		}
	}
	ctl.mu.Lock()
	cordoned := ctl.cordoned
	ctl.mu.Unlock()
	if cordoned {
		t.Fatal("runner left cordoned")
	}
}

// A tenant key never reaches the runner.
func TestRunnerDrainRefusesTenants(t *testing.T) {
	router, _ := newTestGateway(t, "admin-key")
	tenant := mintTenantKey(t, router, `{"name":"t"}`)
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		req := httptest.NewRequest(method, "/admin/runners/runner-1/drain", nil)
		req.Header.Set("X-Api-Key", tenant.Key.APIKey)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("%s drain: got %d, want 403", method, rr.Code)
		}
	}
}
