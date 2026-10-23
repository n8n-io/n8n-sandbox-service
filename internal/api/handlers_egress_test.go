package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// seedProxySandbox stores a proxy sandbox on the fake runner, as a create
// against a runner that serves the proxy would.
func seedProxySandbox(t *testing.T, router http.Handler, id string) {
	t.Helper()
	if rr := postCreateSandbox(t, router, "admin-key", `{"id":"`+id+`","egress":"proxy","egress_proxy":`+proxySpec+`}`); rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
}

func TestPutEgressProxy(t *testing.T) {
	ctl := &fakeSandboxControl{}
	router, s, _ := newIdleTestGateway(t, "admin-key", ctl)
	const id = "f1f1f1f1-f1f1-4f1f-8f1f-f1f1f1f1f1f1"
	seedProxySandbox(t, router, id)
	path := "/sandboxes/" + id + "/egress-proxy"

	newSpec := `{"allow":[{"name":"git","hosts":["git.example.com"]}],"push_refs":["refs/heads/factory-wip/s2"],"labels":{"session_id":"s2"}}`
	if rr := doJSON(t, router, http.MethodPut, path, "admin-key", newSpec); rr.Code != http.StatusNoContent {
		t.Fatalf("put: %d %s", rr.Code, rr.Body.String())
	}
	rec, _ := s.Get(id)
	if len(ctl.egressSpecs) != 1 || ctl.egressSpecs[0] != rec.EgressProxy || rec.EgressProxy == "" {
		t.Fatalf("runner got %v, store has %q", ctl.egressSpecs, rec.EgressProxy)
	}
	// A reconnect naming the new spec matches it; the old one is a mismatch.
	if rr := postCreateSandbox(t, router, "admin-key", `{"id":"`+id+`","egress":"proxy","egress_proxy":`+newSpec+`}`); rr.Code != http.StatusOK {
		t.Fatalf("reconnect with the new spec: %d %s", rr.Code, rr.Body.String())
	}
	if rr := postCreateSandbox(t, router, "admin-key", `{"id":"`+id+`","egress":"proxy","egress_proxy":`+proxySpec+`}`); rr.Code != http.StatusConflict {
		t.Fatalf("reconnect with the old spec: %d", rr.Code)
	}

	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"allow":[],"x":1}`, http.StatusBadRequest},
		{`{"allow":[{"name":"x","hosts":["10.0.0.1"]}]}`, http.StatusBadRequest},
		{`not json`, http.StatusBadRequest},
	} {
		if rr := doJSON(t, router, http.MethodPut, path, "admin-key", tc.body); rr.Code != tc.want {
			t.Errorf("%s: %d", tc.body, rr.Code)
		}
	}

	ctl.mu.Lock()
	ctl.egressErr = status.Error(codes.FailedPrecondition, "unknown secret_ref")
	ctl.mu.Unlock()
	rr := doJSON(t, router, http.MethodPut, path, "admin-key", newSpec)
	var body APIError
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if rr.Code != http.StatusBadRequest || body.Reason != reasonUnknownSecretRef {
		t.Fatalf("runner refusal: %d %s", rr.Code, rr.Body.String())
	}
	ctl.mu.Lock()
	ctl.egressErr = status.Error(codes.Unavailable, "down")
	ctl.mu.Unlock()
	if rr := doJSON(t, router, http.MethodPut, path, "admin-key", newSpec); rr.Code != http.StatusServiceUnavailable || rr.Header().Get("Retry-After") == "" {
		t.Fatalf("runner down: %d", rr.Code)
	}

	// A sandbox that is not a proxy sandbox has no spec to replace.
	const plain = "f2f2f2f2-f2f2-4f2f-8f2f-f2f2f2f2f2f2"
	if rr := postCreateSandbox(t, router, "admin-key", `{"id":"`+plain+`"}`); rr.Code != http.StatusCreated {
		t.Fatal(rr.Body.String())
	}
	rr = doJSON(t, router, http.MethodPut, "/sandboxes/"+plain+"/egress-proxy", "admin-key", newSpec)
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if rr.Code != http.StatusConflict || body.Reason != reasonEgressMismatch {
		t.Fatalf("public sandbox: %d %s", rr.Code, rr.Body.String())
	}
}

func TestModelEgress(t *testing.T) {
	ctl := &fakeSandboxControl{}
	router, _, _ := newIdleTestGateway(t, "admin-key", ctl)
	const id = "f3f3f3f3-f3f3-4f3f-8f3f-f3f3f3f3f3f3"
	seedProxySandbox(t, router, id)
	path := "/sandboxes/" + id + "/model-egress"
	get := func() bool {
		rr := doJSON(t, router, http.MethodGet, path, "admin-key", "")
		var b modelEgressResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &b); err != nil || rr.Code != http.StatusOK {
			t.Fatalf("get: %d %s", rr.Code, rr.Body.String())
		}
		return b.Blocked
	}
	if get() {
		t.Fatal("blocked at create")
	}
	if rr := doJSON(t, router, http.MethodPut, path, "admin-key", `{"blocked":true}`); rr.Code != http.StatusOK || !get() {
		t.Fatalf("block: %d %s", rr.Code, rr.Body.String())
	}
	if rr := doJSON(t, router, http.MethodPut, path, "admin-key", `{"blocked":false}`); rr.Code != http.StatusOK || get() {
		t.Fatalf("unblock: %d", rr.Code)
	}
	if len(ctl.modelBlocked) != 2 || !ctl.modelBlocked[0] || ctl.modelBlocked[1] {
		t.Fatalf("runner got %v", ctl.modelBlocked)
	}
	for _, body := range []string{`{}`, `{"blocked":"yes"}`, `{"blocked":true,"x":1}`} {
		if rr := doJSON(t, router, http.MethodPut, path, "admin-key", body); rr.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", body, rr.Code)
		}
	}
	other := mintTenantKey(t, router, `{"name":"other"}`)
	if rr := doJSON(t, router, http.MethodPut, path, other.Key.APIKey, `{"blocked":true}`); rr.Code != http.StatusNotFound {
		t.Fatalf("other tenant: %d", rr.Code)
	}
}

// A runner refusing a create for a secret the tenant does not have is 400.
func TestCreateSandboxUnknownSecretRef(t *testing.T) {
	ctl := &fakeSandboxControl{createErr: status.Error(codes.FailedPrecondition, `unknown secret_ref "factory/git-token"`)}
	router, s, _ := newIdleTestGateway(t, "admin-key", ctl)
	rr := postCreateSandbox(t, router, "admin-key", `{"egress":"proxy","egress_proxy":`+proxySpec+`}`)
	var body APIError
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if rr.Code != http.StatusBadRequest || body.Reason != reasonUnknownSecretRef {
		t.Fatalf("got %d %s", rr.Code, rr.Body.String())
	}
	if n, _ := s.Count(); n != 0 {
		t.Fatalf("stored %d", n)
	}
}
