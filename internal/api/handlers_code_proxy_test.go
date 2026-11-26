package api

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/codetask"
	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/sandboxproxy"
)

const (
	codeProxySandboxID    = "cccccccc-1111-4111-8111-cccccccccccc"
	taskTestContentType   = "multipart/form-data; boundary=b"
	codeProxyMaxTaskInput = 1024
)

func newCodeProxyTestGateway(t *testing.T) (http.Handler, store.SandboxStore, *ActivityRecorder) {
	t.Helper()
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	cfg := idleSweepConfig()
	cfg.APIKeys = map[string]struct{}{"admin-key": {}}
	cfg.MaxFileBytes = 1 << 20
	cfg.MaxTaskInputBytes = codeProxyMaxTaskInput
	cfg.DefaultMaxSandboxes = 50
	cfg.CodeSandboxesEnabled = true
	cfg.ActivityWriteInterval = time.Hour
	activity := NewActivityRecorder(s, cfg)
	router, err := NewGatewayRouter(s, cfg, registry.New(45*time.Second), metrics.NewAPIRecorder(false), activity)
	if err != nil {
		t.Fatalf("create gateway router: %v", err)
	}
	return router, s, activity
}

func createCodeProxySandbox(t *testing.T, s store.SandboxStore, id, tenantID, runnerURL string, kind store.Kind, lastActiveAt int64) {
	t.Helper()
	if err := s.Create(&store.SandboxRecord{
		ID: id, Status: "running", CreatedAt: lastActiveAt, LastActiveAt: lastActiveAt,
		TenantID: tenantID, RunnerHTTPBase: runnerURL, Ephemeral: true,
		Kind: kind, Name: "scope-" + id,
	}); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
}

func postTask(router http.Handler, id, apiKey, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/code/sandboxes/"+id+"/tasks", strings.NewReader(body))
	req.Header.Set("X-Api-Key", apiKey)
	req.Header.Set("Content-Type", taskTestContentType)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

func TestCodeTaskIsProxiedToRunner(t *testing.T) {
	var gotPath, gotKey, gotContentType, gotBody string
	runner := newTestRunnerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotKey, gotContentType = r.URL.Path, r.Header.Get("X-Api-Key"), r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"type":"done"}` + "\n"))
	}))

	router, s, activity := newCodeProxyTestGateway(t)
	tenant := mintTenantKey(t, router, `{"name":"code"}`)
	stale := time.Now().Unix() - 60
	createCodeProxySandbox(t, s, codeProxySandboxID, tenant.Tenant.ID, runner.URL, store.KindCode, stale)

	rr := postTask(router, codeProxySandboxID, tenant.Key.APIKey, "parts")
	if rr.Code != http.StatusOK || rr.Body.String() != `{"type":"done"}`+"\n" {
		t.Fatalf("got %d %q, want the runner's events", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("Content-Type = %q, want application/x-ndjson", ct)
	}
	if want := "/sandboxes/" + codeProxySandboxID + "/tasks"; gotPath != want {
		t.Errorf("runner path = %q, want %q", gotPath, want)
	}
	if gotKey != "runner-key" {
		t.Errorf("runner X-Api-Key = %q, want the runner key, not the tenant's", gotKey)
	}
	if gotContentType != taskTestContentType || gotBody != "parts" {
		t.Errorf("runner got Content-Type %q body %q", gotContentType, gotBody)
	}

	activity.write()
	if rec := mustGet(t, s, codeProxySandboxID); rec.LastActiveAt <= stale {
		t.Errorf("last_active_at = %d, want a fresh timestamp", rec.LastActiveAt)
	}
}

func TestCodeTaskDeleteIsProxiedToRunner(t *testing.T) {
	var runnerHits atomic.Int32
	var gotMethod, gotPath string
	runner := newTestRunnerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runnerHits.Add(1)
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusAccepted)
	}))

	router, s, _ := newCodeProxyTestGateway(t)
	tenant := mintTenantKey(t, router, `{"name":"code"}`)
	createCodeProxySandbox(t, s, codeProxySandboxID, tenant.Tenant.ID, runner.URL, store.KindCode, time.Now().Unix())

	for taskID, want := range map[string]int{"t_1": http.StatusAccepted, "bad.id": http.StatusBadRequest} {
		req := httptest.NewRequest(http.MethodDelete, "/code/sandboxes/"+codeProxySandboxID+"/tasks/"+taskID, nil)
		req.Header.Set("X-Api-Key", tenant.Key.APIKey)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != want {
			t.Errorf("task %q: status = %d, want %d: %s", taskID, rr.Code, want, rr.Body.String())
		}
	}
	if got := runnerHits.Load(); got != 1 {
		t.Fatalf("runner hits = %d, want 1: an invalid task id must not reach the runner", got)
	}
	if want := "/sandboxes/" + codeProxySandboxID + "/tasks/t_1"; gotMethod != http.MethodDelete || gotPath != want {
		t.Errorf("runner got %s %s, want DELETE %s", gotMethod, gotPath, want)
	}
}

// Neither direction is buffered: the runner reads the first part of the upload
// while the client still holds the rest, and the client reads the first event
// while the runner still holds the last.
func TestCodeTaskStreamsBothWays(t *testing.T) {
	firstPartRead := make(chan struct{})
	firstEventRead := make(chan struct{})
	runner := newTestRunnerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, len("first"))
		if _, err := io.ReadFull(r.Body, buf); err != nil || string(buf) != "first" {
			t.Errorf("first part = %q, %v", buf, err)
			return
		}
		close(firstPartRead)
		if rest, _ := io.ReadAll(r.Body); string(rest) != "rest" {
			t.Errorf("rest = %q, want rest", rest)
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"type":"accepted"}` + "\n"))
		w.(http.Flusher).Flush()
		select {
		case <-firstEventRead:
		case <-time.After(5 * time.Second):
			t.Error("the first event did not reach the client before the stream ended")
		}
		_, _ = w.Write([]byte(`{"type":"done"}` + "\n"))
	}))

	router, s, _ := newCodeProxyTestGateway(t)
	createCodeProxySandbox(t, s, codeProxySandboxID, store.AdminTenantID, runner.URL, store.KindCode, time.Now().Unix())
	api := httptest.NewServer(router)
	defer api.Close()

	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte("first"))
		select {
		case <-firstPartRead:
		case <-time.After(5 * time.Second):
			t.Error("the first part did not reach the runner before the upload ended")
		}
		_, _ = pw.Write([]byte("rest"))
		_ = pw.Close()
	}()

	req, err := http.NewRequest(http.MethodPost, api.URL+"/code/sandboxes/"+codeProxySandboxID+"/tasks", pr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Api-Key", "admin-key")
	req.Header.Set("Content-Type", taskTestContentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	lines := bufio.NewReader(resp.Body)
	first, err := lines.ReadString('\n')
	if err != nil || first != `{"type":"accepted"}`+"\n" {
		t.Fatalf("first event = %q, %v", first, err)
	}
	close(firstEventRead)
	last, err := lines.ReadString('\n')
	if err != nil || last != `{"type":"done"}`+"\n" {
		t.Fatalf("last event = %q, %v", last, err)
	}
}

// A runner that refuses a task before the upload ends gets its answer to the
// client while the client still holds the upload open.
func TestCodeTaskRelaysEarlyRejection(t *testing.T) {
	runner := newTestRunnerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).EnableFullDuplex()
		buf := make([]byte, len("spec"))
		if _, err := io.ReadFull(r.Body, buf); err != nil {
			t.Errorf("read spec: %v", err)
		}
		w.Header().Set("Connection", "close")
		_ = http.NewResponseController(w).SetReadDeadline(time.Now())
		http.Error(w, "no free lane", http.StatusTooManyRequests)
	}))

	router, s, _ := newCodeProxyTestGateway(t)
	createCodeProxySandbox(t, s, codeProxySandboxID, store.AdminTenantID, runner.URL, store.KindCode, time.Now().Unix())
	handlerDone := make(chan struct{})
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		router.ServeHTTP(w, r)
	}))
	defer api.Close()

	conn, err := net.DialTimeout("tcp", api.Listener.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// Leave the chunked upload open after the spec.
	if _, err := fmt.Fprintf(conn, "POST /code/sandboxes/%s/tasks HTTP/1.1\r\nHost: localhost\r\nX-Api-Key: admin-key\r\nContent-Type: %s\r\nTransfer-Encoding: chunked\r\n\r\n4\r\nspec\r\n", codeProxySandboxID, taskTestContentType); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusTooManyRequests || !resp.Close {
		t.Fatalf("status = %d, close = %v; want 429 and close", resp.StatusCode, resp.Close)
	}
	// The handler holds the tenant's request slot until it returns.
	select {
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler did not return while the upload stayed open")
	}
}

// The upstream counter proves each refusal happens before anything leaves the
// API.
func TestCodeTaskRefusesSandboxesTheCallerCannotUse(t *testing.T) {
	var upstreamHits atomic.Int64
	runner := newTestRunnerServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))

	router, s, _ := newCodeProxyTestGateway(t)
	a := mintTenantKey(t, router, `{"name":"a"}`)
	b := mintTenantKey(t, router, `{"name":"b"}`)
	now := time.Now().Unix()

	const (
		ownCode   = "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa"
		ownIdle   = "aaaaaaaa-2222-4222-8222-aaaaaaaaaaaa"
		ownOther  = "aaaaaaaa-3333-4333-8333-aaaaaaaaaaaa"
		adminCode = "aaaaaaaa-4444-4444-8444-aaaaaaaaaaaa"
		missing   = "aaaaaaaa-5555-4555-8555-aaaaaaaaaaaa"
	)
	createCodeProxySandbox(t, s, ownCode, a.Tenant.ID, runner.URL, store.KindCode, now)
	createCodeProxySandbox(t, s, ownIdle, a.Tenant.ID, runner.URL, store.KindCode, 1)
	createCodeProxySandbox(t, s, ownOther, a.Tenant.ID, runner.URL, store.KindGeneral, now)
	createCodeProxySandbox(t, s, adminCode, store.AdminTenantID, runner.URL, store.KindCode, now)

	for _, tc := range []struct {
		name, id, key string
		want          int
	}{
		{"other tenant's sandbox", ownCode, b.Key.APIKey, http.StatusNotFound},
		{"admin sandbox with a tenant key", adminCode, a.Key.APIKey, http.StatusNotFound},
		{"past its idle window", ownIdle, a.Key.APIKey, http.StatusNotFound},
		{"general sandbox", ownOther, a.Key.APIKey, http.StatusNotFound},
		{"missing", missing, a.Key.APIKey, http.StatusNotFound},
		{"invalid id", "not-a-uuid", a.Key.APIKey, http.StatusBadRequest},
		{"unknown key", ownCode, "nope", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rr := postTask(router, tc.id, tc.key, "parts"); rr.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rr.Code, tc.want, rr.Body.String())
			}
		})
	}
	if n := upstreamHits.Load(); n != 0 {
		t.Fatalf("runner hits = %d, want 0", n)
	}

	if rr := postTask(router, ownCode, a.Key.APIKey, "parts"); rr.Code != http.StatusOK {
		t.Fatalf("owner: status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
}

// The body may exceed the input limit by the room for the spec and the
// multipart headers, and no more.
func TestCodeTaskCapsBody(t *testing.T) {
	maxBody := codetask.MaxBodyBytes(codeProxyMaxTaskInput)
	for _, tc := range []struct {
		size int64
		want int
	}{
		{maxBody, http.StatusOK},
		{maxBody + 1, http.StatusBadRequest},
	} {
		var received atomic.Int64
		runner := newTestRunnerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n, _ := io.Copy(io.Discard, r.Body)
			received.Add(n)
			w.WriteHeader(http.StatusOK)
		}))

		router, s, _ := newCodeProxyTestGateway(t)
		createCodeProxySandbox(t, s, codeProxySandboxID, store.AdminTenantID, runner.URL, store.KindCode, time.Now().Unix())

		rr := postTask(router, codeProxySandboxID, "admin-key", strings.Repeat("x", int(tc.size)))
		if rr.Code != tc.want {
			t.Fatalf("%d-byte body: status = %d, want %d: %s", tc.size, rr.Code, tc.want, rr.Body.String())
		}
		if n := received.Load(); n > maxBody {
			t.Fatalf("%d-byte body: runner received %d bytes, past the %d-byte cap", tc.size, n, maxBody)
		}
	}
}

// A full sandbox's 429 is the client's signal to try the next shard, so it
// reaches the client as the runner sent it.
func TestCodeTaskRelaysRunnerRejection(t *testing.T) {
	const body = `{"error":"sandbox is full","reason":"sandbox_full"}`
	runner := newTestRunnerServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(body))
	}))

	router, s, _ := newCodeProxyTestGateway(t)
	createCodeProxySandbox(t, s, codeProxySandboxID, store.AdminTenantID, runner.URL, store.KindCode, time.Now().Unix())

	rr := postTask(router, codeProxySandboxID, "admin-key", "parts")
	if rr.Code != http.StatusTooManyRequests || rr.Body.String() != body || rr.Header().Get("Retry-After") != "1" {
		t.Fatalf("got %d %q Retry-After %q, want the runner's 429", rr.Code, rr.Body.String(), rr.Header().Get("Retry-After"))
	}
}

func TestCodeTaskDropsSandboxTheRunnerNoLongerKnows(t *testing.T) {
	runner := newTestRunnerServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sandboxproxy.MarkSandboxGone(w.Header())
		w.WriteHeader(http.StatusNotFound)
	}))

	router, s, _ := newCodeProxyTestGateway(t)
	createCodeProxySandbox(t, s, codeProxySandboxID, store.AdminTenantID, runner.URL, store.KindCode, time.Now().Unix())

	if rr := postTask(router, codeProxySandboxID, "admin-key", "parts"); rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rr.Code, rr.Body.String())
	}
	rec, err := s.GetCodeSandbox(codeProxySandboxID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if rec != nil {
		t.Fatal("record kept after the runner reported the sandbox gone")
	}
}

// Over plain http the runner API key would travel in the clear.
func TestCodeTaskRefusesNonHTTPSRunner(t *testing.T) {
	router, s, _ := newCodeProxyTestGateway(t)
	createCodeProxySandbox(t, s, codeProxySandboxID, store.AdminTenantID, "http://127.0.0.1:9", store.KindCode, time.Now().Unix())

	if rr := postTask(router, codeProxySandboxID, "admin-key", "parts"); rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", rr.Code, rr.Body.String())
	}
}
