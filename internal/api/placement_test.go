package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/runnerctl"
	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/metrics"
)

// refusal is what a runner at capacity answers a create with.
var refusal = runnerctl.CapacityRefusal("firecracker runner capacity exhausted")

// newPlacementTestGateway serves the API against one runner per fake, named
// runner-1, runner-2, ... and reporting equal load, so placement tries them in
// that order.
func newPlacementTestGateway(t *testing.T, fakes ...*fakeSandboxControl) (http.Handler, store.SandboxStore, *metrics.APIRecorder) {
	t.Helper()
	s := newSweepStore(t)
	cfg := idleSweepConfig()
	cfg.APIKeys = map[string]struct{}{"admin-key": {}}
	cfg.MaxFileBytes = 1024
	cfg.DefaultMaxSandboxes = 50

	reg := registry.New(45 * time.Second)
	for i, fake := range fakes {
		reg.Upsert(fmt.Sprintf("runner-%d", i+1), "https://127.0.0.1:9", startFakeRunnerControl(t, fake), true, false, 10, 0, 0)
	}
	rec := metrics.NewAPIRecorder(true)
	router, err := NewGatewayRouter(s, cfg, reg, rec)
	if err != nil {
		t.Fatalf("create gateway router: %v", err)
	}
	return router, s, rec
}

func assertPlacements(t *testing.T, rec *metrics.APIRecorder, want map[string]float64) {
	t.Helper()
	for _, result := range []string{metrics.PlacementFirstChoice, metrics.PlacementRetried, metrics.PlacementExhausted, metrics.PlacementNoRunner} {
		if got := rec.PlacementCount(result); got != want[result] {
			t.Errorf("placements{result=%q} = %v, want %v", result, got, want[result])
		}
	}
}

// assertNoCapacity checks the answer to a create no runner took: a 503 the
// client may repeat, with the reason that says nothing was created.
func assertNoCapacity(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected %d, got %d body=%s", http.StatusServiceUnavailable, rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Retry-After"); got != capacityRetryAfter {
		t.Errorf("Retry-After = %q, want %q", got, capacityRetryAfter)
	}
	var body APIError
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	if body.Reason != reasonNoCapacity || body.Code != http.StatusServiceUnavailable {
		t.Errorf("body = %+v, want reason %q and code 503", body, reasonNoCapacity)
	}
}

func assertNothingStored(t *testing.T, s store.SandboxStore) {
	t.Helper()
	if n, err := s.Count(); err != nil || n != 0 {
		t.Fatalf("stored %d sandboxes (err=%v), want none", n, err)
	}
}

// A runner at capacity refuses before creating anything, so the create goes to
// the next runner and the refusing one has nothing to clean up.
func TestCreateSandboxRetriesRefusedCreateOnNextRunner(t *testing.T) {
	logs := captureLogs(t)
	full := &fakeSandboxControl{createErr: refusal}
	free := &fakeSandboxControl{}
	router, s, rec := newPlacementTestGateway(t, full, free)

	rr := postCreateSandbox(t, router, "admin-key", "")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected %d, got %d body=%s", http.StatusCreated, rr.Code, rr.Body.String())
	}
	var resp SandboxResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if stored, err := s.Get(resp.ID); err != nil || stored == nil || stored.RunnerID != "runner-2" {
		t.Fatalf("stored row = %+v err=%v, want it on runner-2", stored, err)
	}
	if full.creates() != 1 || free.creates() != 1 {
		t.Fatalf("creates = %d on the full runner, %d on the free one; want 1 and 1", full.creates(), free.creates())
	}
	if _, deleted := full.calls(); len(deleted) != 0 {
		t.Fatalf("deletes on the refusing runner = %v, want none", deleted)
	}
	assertPlacements(t, rec, map[string]float64{metrics.PlacementRetried: 1})

	for _, event := range logs() {
		if event["msg"] != "request" {
			continue
		}
		if event["runner_id"] != "runner-2" || event["placement_attempts"] != float64(2) {
			t.Fatalf("request event = %v, want runner_id runner-2 and placement_attempts 2", event)
		}
		return
	}
	t.Fatal("no request event")
}

func TestCreateSandboxEveryRunnerRefuses(t *testing.T) {
	fakes := []*fakeSandboxControl{{createErr: refusal}, {createErr: refusal}, {createErr: refusal}, {createErr: refusal}}
	router, s, rec := newPlacementTestGateway(t, fakes...)

	assertNoCapacity(t, postCreateSandbox(t, router, "admin-key", ""))
	for i, fake := range fakes {
		want := 1
		if i >= createPlacementAttempts {
			want = 0
		}
		if got := fake.creates(); got != want {
			t.Errorf("runner-%d creates = %d, want %d", i+1, got, want)
		}
		if _, deleted := fake.calls(); len(deleted) != 0 {
			t.Errorf("runner-%d deletes = %v, want none", i+1, deleted)
		}
	}
	assertNothingStored(t, s)
	assertPlacements(t, rec, map[string]float64{metrics.PlacementExhausted: 1})
}

func TestCreateSandboxNoEligibleRunner(t *testing.T) {
	router, s, rec := newPlacementTestGateway(t)

	assertNoCapacity(t, postCreateSandbox(t, router, "admin-key", ""))
	assertNothingStored(t, s)
	assertPlacements(t, rec, map[string]float64{metrics.PlacementNoRunner: 1})
}

// Only a refusal guarantees the runner created nothing. After any other error it
// may have, so trying another runner could leave the sandbox on two. That
// includes a ResourceExhausted without the runner's marker, which gRPC itself
// reports for transport conditions.
func TestCreateSandboxRunnerErrorIsNotRetried(t *testing.T) {
	for _, code := range []codes.Code{codes.Internal, codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted} {
		t.Run(code.String(), func(t *testing.T) {
			failing := &fakeSandboxControl{createErr: status.Error(code, "boom")}
			free := &fakeSandboxControl{}
			router, s, rec := newPlacementTestGateway(t, failing, free)

			rr := postCreateSandbox(t, router, "admin-key", "")
			if rr.Code != http.StatusInternalServerError {
				t.Fatalf("expected %d, got %d body=%s", http.StatusInternalServerError, rr.Code, rr.Body.String())
			}
			if free.creates() != 0 {
				t.Fatalf("second runner creates = %d, want none", free.creates())
			}
			assertNothingStored(t, s)
			assertPlacements(t, rec, map[string]float64{metrics.PlacementFirstChoice: 1})
		})
	}
}

func TestCreateSandboxPlacementStopsAtBudget(t *testing.T) {
	previous := createPlacementBudget
	createPlacementBudget = 20 * time.Millisecond
	t.Cleanup(func() { createPlacementBudget = previous })

	slow := &fakeSandboxControl{createErr: refusal, createHook: func(context.Context) { time.Sleep(50 * time.Millisecond) }}
	free := &fakeSandboxControl{}
	router, s, rec := newPlacementTestGateway(t, slow, free)

	assertNoCapacity(t, postCreateSandbox(t, router, "admin-key", ""))
	if free.creates() != 0 {
		t.Fatalf("second runner creates = %d, want none once the budget is spent", free.creates())
	}
	assertNothingStored(t, s)
	assertPlacements(t, rec, map[string]float64{metrics.PlacementExhausted: 1})
}

// After a refusal nothing is in flight, so a client that has hung up gets no
// further attempt.
func TestCreateSandboxPlacementStopsForGoneClient(t *testing.T) {
	ctx, disconnect := context.WithCancel(context.Background())
	defer disconnect()
	full := &fakeSandboxControl{createErr: refusal, createHook: func(context.Context) { disconnect() }}
	free := &fakeSandboxControl{}
	router, s, rec := newPlacementTestGateway(t, full, free)

	req := httptest.NewRequest(http.MethodPost, "/sandboxes", nil).WithContext(ctx)
	req.Header.Set("X-Api-Key", "admin-key")
	router.ServeHTTP(httptest.NewRecorder(), req)

	if free.creates() != 0 {
		t.Fatalf("second runner creates = %d, want none for a client that has gone", free.creates())
	}
	assertNothingStored(t, s)
	assertPlacements(t, rec, map[string]float64{metrics.PlacementExhausted: 1})
}
