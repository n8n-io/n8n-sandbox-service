package api

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/metrics"
)

const (
	limitTestSandboxA = "aaaaaaaa-0000-4000-8000-000000000001"
	limitTestSandboxB = "bbbbbbbb-0000-4000-8000-000000000002"
)

// blockingRunner stands in for a runner whose every request is a stream that
// stays open until the test releases it or the caller goes away.
type blockingRunner struct {
	*httptest.Server
	hits    atomic.Int64
	release chan struct{}
}

func newBlockingRunner(t *testing.T) *blockingRunner {
	t.Helper()
	br := &blockingRunner{release: make(chan struct{})}
	br.Server = newTestRunnerServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		br.hits.Add(1)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-br.release:
		case <-r.Context().Done():
		}
	}))
	// Registered after the server's own cleanup, so it runs first and Close
	// is not left waiting on handlers that are still blocked.
	t.Cleanup(br.releaseAll)
	return br
}

func (br *blockingRunner) releaseAll() {
	select {
	case <-br.release:
	default:
		close(br.release)
	}
}

func (br *blockingRunner) waitForHits(t *testing.T, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for br.hits.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("runner hits = %d, want %d", br.hits.Load(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type limitTestGateway struct {
	router http.Handler
	rec    *metrics.APIRecorder
	keyA   string
	keyB   string
}

// newLimitTestGateway builds a gateway with the given limits and two tenants,
// A and B, each owning one sandbox on runner.
func newLimitTestGateway(t *testing.T, runner *blockingRunner, tune func(*config.APIConfig)) *limitTestGateway {
	t.Helper()
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	cfg := &config.APIConfig{
		APIKeys:             map[string]struct{}{"admin-key": {}},
		RunnerAPIKey:        "runner-key",
		MaxFileBytes:        1024,
		DefaultMaxSandboxes: 50,
	}
	tune(cfg)
	rec := metrics.NewAPIRecorder(true)
	router, err := NewGatewayRouter(s, withRunnerTLS(cfg), registry.New(45*time.Second), rec)
	if err != nil {
		t.Fatalf("create gateway router: %v", err)
	}

	a := mintTenantKey(t, router, `{"name":"a"}`)
	b := mintTenantKey(t, router, `{"name":"b"}`)
	for id, tenant := range map[string]string{limitTestSandboxA: a.Tenant.ID, limitTestSandboxB: b.Tenant.ID} {
		if err := s.Create(&store.SandboxRecord{
			ID: id, Status: "running", CreatedAt: 1, LastActiveAt: time.Now().Unix(),
			TenantID: tenant, RunnerHTTPBase: runner.URL,
		}); err != nil {
			t.Fatalf("create sandbox: %v", err)
		}
	}
	return &limitTestGateway{router: router, rec: rec, keyA: a.Key.APIKey, keyB: b.Key.APIKey}
}

func (g *limitTestGateway) call(ctx context.Context, method, path, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(ctx, method, path, nil)
	req.Header.Set("X-Api-Key", key)
	rr := httptest.NewRecorder()
	g.router.ServeHTTP(rr, req)
	return rr
}

// hold starts n requests that stay open in the runner until the test ends or
// cancel is called, and returns once the runner has seen all of them.
func (g *limitTestGateway) hold(t *testing.T, runner *blockingRunner, n int, method, path, key string) (cancel func()) {
	t.Helper()
	ctx, cancelCtx := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	before := runner.hits.Load()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g.call(ctx, method, path, key)
		}()
	}
	runner.waitForHits(t, before+int64(n))
	stop := func() {
		cancelCtx()
		wg.Wait()
	}
	t.Cleanup(stop)
	return stop
}

func assertLimitResponse(t *testing.T, rr *httptest.ResponseRecorder, wantStatus int, wantReason string) {
	t.Helper()
	if rr.Code != wantStatus {
		t.Fatalf("status = %d, want %d: %s", rr.Code, wantStatus, rr.Body.String())
	}
	if got := rr.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}
	var body APIError
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", rr.Body.String(), err)
	}
	if body.Reason != wantReason {
		t.Errorf("reason = %q, want %q", body.Reason, wantReason)
	}
	if body.Code != wantStatus {
		t.Errorf("code = %d, want %d", body.Code, wantStatus)
	}
}

// Every proxied sandbox route counts, streams or not: a slow file download
// holds a connection and a goroutine through every tier just as an exec does.
func TestTenantLimitRefusesRequestsOverCap(t *testing.T) {
	runner := newBlockingRunner(t)
	g := newLimitTestGateway(t, runner, func(c *config.APIConfig) { c.MaxInflightPerTenant = 2 })
	sandboxA := "/sandboxes/" + limitTestSandboxA

	g.hold(t, runner, 1, http.MethodPost, sandboxA+"/executions", g.keyA)
	g.hold(t, runner, 1, http.MethodGet, sandboxA+"/files/content?path=/tmp/big", g.keyA)

	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/executions"},
		{http.MethodGet, "/executions/exec-1?follow=true"},
		{http.MethodGet, "/stat?path=/tmp"},
		{http.MethodPut, "/files?path=/tmp/x"},
	} {
		rr := g.call(context.Background(), route.method, sandboxA+route.path, g.keyA)
		assertLimitResponse(t, rr, http.StatusTooManyRequests, reasonTenantRequestLimit)
	}
	if hits := runner.hits.Load(); hits != 2 {
		t.Errorf("runner hits = %d, want 2: a refused request must not reach the runner", hits)
	}
	if got := g.rec.RequestsLimitedCount(metrics.LimitTenant); got != 4 {
		t.Errorf("tenant limited count = %v, want 4", got)
	}

	// The cap is per tenant: B is unaffected by A being full.
	release := g.hold(t, runner, 1, http.MethodGet, "/sandboxes/"+limitTestSandboxB+"/files", g.keyB)
	release()
}

// Execution DELETE is how a tenant stops work, so it is counted apart from the
// tenant's other requests: it gets through while they fill their limit, and
// takes none of their slots. Its own limit is bounded: a DELETE to a daemon the
// guest has starved holds its slot too, and without a bound one tenant could
// fill the process with them.
func TestTenantLimitCountsExecutionDeletesApart(t *testing.T) {
	runner := newBlockingRunner(t)
	g := newLimitTestGateway(t, runner, func(c *config.APIConfig) { c.MaxInflightPerTenant = 1 })
	sandboxA := "/sandboxes/" + limitTestSandboxA

	releaseExec := g.hold(t, runner, 1, http.MethodPost, sandboxA+"/executions", g.keyA)
	g.hold(t, runner, maxExecutionDeletesPerTenant, http.MethodDelete, sandboxA+"/executions/exec-1", g.keyA)

	rr := g.call(context.Background(), http.MethodDelete, sandboxA+"/executions/exec-1", g.keyA)
	assertLimitResponse(t, rr, http.StatusTooManyRequests, reasonTenantRequestLimit)

	releaseExec()
	g.hold(t, runner, 1, http.MethodPost, sandboxA+"/executions", g.keyA)
}

func TestTenantLimitOffLeavesExecutionDeletesUnlimited(t *testing.T) {
	runner := newBlockingRunner(t)
	g := newLimitTestGateway(t, runner, func(c *config.APIConfig) { c.MaxInflightPerTenant = 0 })

	g.hold(t, runner, maxExecutionDeletesPerTenant+1, http.MethodDelete, "/sandboxes/"+limitTestSandboxA+"/executions/exec-1", g.keyA)
}

// A create holds its slot until the runner finishes, even once the client has
// gone, so creates count like any other request in progress.
func TestTenantLimitCountsSandboxCreate(t *testing.T) {
	runner := newBlockingRunner(t)
	g := newLimitTestGateway(t, runner, func(c *config.APIConfig) { c.MaxInflightPerTenant = 1 })

	g.hold(t, runner, 1, http.MethodPost, "/sandboxes/"+limitTestSandboxA+"/executions", g.keyA)

	rr := g.call(context.Background(), http.MethodPost, "/sandboxes", g.keyA)
	assertLimitResponse(t, rr, http.StatusTooManyRequests, reasonTenantRequestLimit)
}

func TestTenantLimitSkipsAdminKeys(t *testing.T) {
	runner := newBlockingRunner(t)
	g := newLimitTestGateway(t, runner, func(c *config.APIConfig) { c.MaxInflightPerTenant = 1 })

	// Three at once on a tenant's sandbox, over a cap of one.
	g.hold(t, runner, 3, http.MethodPost, "/sandboxes/"+limitTestSandboxA+"/executions", "admin-key")
}

// A slot is held for the life of the stream, so it has to come back when the
// client goes away mid-stream rather than when the runner eventually finishes.
func TestTenantLimitFreesSlotWhenClientDisconnects(t *testing.T) {
	runner := newBlockingRunner(t)
	g := newLimitTestGateway(t, runner, func(c *config.APIConfig) { c.MaxInflightPerTenant = 1 })
	sandboxA := "/sandboxes/" + limitTestSandboxA

	disconnect := g.hold(t, runner, 1, http.MethodPost, sandboxA+"/executions", g.keyA)
	if rr := g.call(context.Background(), http.MethodPost, sandboxA+"/executions", g.keyA); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status with the slot held = %d, want 429", rr.Code)
	}

	disconnect()
	g.hold(t, runner, 1, http.MethodPost, sandboxA+"/executions", g.keyA)
}

// The runner's HTTPS listener and the API's transport negotiate HTTP/2, so
// the API holds a few connections to each runner however many requests are in
// progress. The runner's file-descriptor budget in docs/configuration.md
// counts requests and leaves these connections to its headroom. The server
// here is built the way internal/runner/app builds the runner's: a tls.Config
// without NextProtos, served by ServeTLS.
func TestRunnerHopUsesHTTP2(t *testing.T) {
	pki := testRunnerPKI()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{pki.serverCert},
			ClientAuth:   tls.VerifyClientCertIfGiven,
			ClientCAs:    pki.caPool,
			MinVersion:   tls.VersionTLS12,
		},
	}
	go func() { _ = srv.ServeTLS(lis, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })

	rt, err := newRunnerTransport(withRunnerTLS(&config.APIConfig{}))
	if err != nil {
		t.Fatalf("build transport: %v", err)
	}
	tr := rt.(*http.Transport)
	tr.Proxy = nil
	resp, err := (&http.Client{Transport: tr}).Get("https://" + lis.Addr().String() + "/")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatalf("API to runner negotiated %s, want HTTP/2", resp.Proto)
	}
}
