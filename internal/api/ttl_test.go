package api

import (
	"context"
	"crypto/tls"
	"math"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/grpc/pb"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/store"
)

// fakeSandboxControl stands in for a runner's SandboxControl gRPC and records
// which lifecycle calls reached it.
type fakeSandboxControl struct {
	pb.UnimplementedSandboxControlServer
	mu        sync.Mutex
	stopped   []string
	deleted   []string
	deleteErr error
	// Run at the start of the RPC, on the server-side context, when set.
	createHook func(ctx context.Context)
	stopHook   func(ctx context.Context)
	deleteHook func(ctx context.Context)
}

func (f *fakeSandboxControl) CreateSandbox(ctx context.Context, _ *pb.CreateSandboxRequest) (*pb.CreateSandboxResponse, error) {
	if f.createHook != nil {
		f.createHook(ctx)
	}
	return &pb.CreateSandboxResponse{ContainerIp: "10.0.0.2"}, nil
}

func (f *fakeSandboxControl) StopSandbox(ctx context.Context, req *pb.StopSandboxRequest) (*pb.StopSandboxResponse, error) {
	if f.stopHook != nil {
		f.stopHook(ctx)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, req.GetSandboxId())
	return &pb.StopSandboxResponse{}, nil
}

func (f *fakeSandboxControl) DeleteSandbox(ctx context.Context, req *pb.DeleteSandboxRequest) (*pb.DeleteSandboxResponse, error) {
	if f.deleteHook != nil {
		f.deleteHook(ctx)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	f.deleted = append(f.deleted, req.GetSandboxId())
	return &pb.DeleteSandboxResponse{}, nil
}

func (f *fakeSandboxControl) failDeletes(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteErr = err
}

func (f *fakeSandboxControl) calls() (stopped, deleted []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.stopped...), append([]string(nil), f.deleted...)
}

// startFakeRunnerControl serves f over mTLS from the package PKI, the way a
// runner's control listener does, and returns its host:port.
func startFakeRunnerControl(t *testing.T, f *fakeSandboxControl) string {
	t.Helper()
	pki := testRunnerPKI()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{pki.serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pki.caPool,
		MinVersion:   tls.VersionTLS12,
	})))
	pb.RegisterSandboxControlServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func idleSweepConfig() *config.APIConfig {
	return withRunnerTLS(&config.APIConfig{
		RunnerAPIKey:           "runner-key",
		IdleStopAfter:          time.Hour,
		IdleDeleteAfter:        24 * time.Hour,
		IdleDeleteSafetyBuffer: time.Minute,
	})
}

func newSweepStore(t *testing.T) store.SandboxStore {
	t.Helper()
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seedRunningSandbox(t *testing.T, s store.SandboxStore, id, controlAddr string, lastActiveAt int64, ephemeral bool) {
	t.Helper()
	if err := s.Create(&store.SandboxRecord{
		ID:                    id,
		Status:                "running",
		CreatedAt:             lastActiveAt,
		LastActiveAt:          lastActiveAt,
		RunnerHTTPBase:        "https://127.0.0.1:9",
		RunnerControlGRPCAddr: controlAddr,
		TenantID:              store.AdminTenantID,
		Ephemeral:             ephemeral,
	}); err != nil {
		t.Fatalf("seed sandbox %s: %v", id, err)
	}
}

func mustGet(t *testing.T, s store.SandboxStore, id string) *store.SandboxRecord {
	t.Helper()
	rec, err := s.Get(id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return rec
}

// runSweep runs one full sweep pass exactly as StartIdleSweeper would, so the
// tests cover the config gating as well as each sweep's own logic.
func runSweep(t *testing.T, s store.SandboxStore, cfg *config.APIConfig, now time.Time) {
	t.Helper()
	if err := sweepIdleSandboxes(context.Background(), s, registry.New(45*time.Second), cfg, runnerControlTLS(cfg), now); err != nil {
		t.Fatalf("sweep: %v", err)
	}
}

// An ephemeral sandbox that would be stopped for idleness is deleted instead
// and never passes through the stopped state; a regular one still stops.
func TestIdleStopSweepDeletesEphemeralInsteadOfStopping(t *testing.T) {
	fake := &fakeSandboxControl{}
	addr := startFakeRunnerControl(t, fake)
	s := newSweepStore(t)
	cfg := idleSweepConfig()

	now := time.Now()
	// Past the stop window and past the delete safety buffer.
	stale := now.Add(-cfg.IdleStopAfter - cfg.IdleDeleteSafetyBuffer - time.Second).Unix()
	const ephemeralID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	const regularID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	seedRunningSandbox(t, s, ephemeralID, addr, stale, true)
	seedRunningSandbox(t, s, regularID, addr, stale, false)

	runSweep(t, s, cfg, now)

	stopped, deleted := fake.calls()
	if len(deleted) != 1 || deleted[0] != ephemeralID {
		t.Fatalf("runner deletes = %v, want [%s]", deleted, ephemeralID)
	}
	if len(stopped) != 1 || stopped[0] != regularID {
		t.Fatalf("runner stops = %v, want [%s]", stopped, regularID)
	}
	if rec := mustGet(t, s, ephemeralID); rec != nil {
		t.Fatalf("ephemeral row still present after sweep: %+v", rec)
	}
	if rec := mustGet(t, s, regularID); rec == nil || rec.Status != "stopped" {
		t.Fatalf("regular row = %+v, want status stopped", rec)
	}
}

// With idle stop disabled, ephemeral rows are deleted at the idle-delete window
// even though they never become "stopped".
func TestIdleSweepDeletesEphemeralWhenIdleStopDisabled(t *testing.T) {
	fake := &fakeSandboxControl{}
	addr := startFakeRunnerControl(t, fake)
	s := newSweepStore(t)
	cfg := idleSweepConfig()
	cfg.IdleStopAfter = 0

	now := time.Now()
	stale := now.Add(-cfg.IdleDeleteAfter - cfg.IdleDeleteSafetyBuffer - time.Second).Unix()
	const ephemeralID = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	const regularID = "abababab-abab-4bab-8bab-abababababab"
	seedRunningSandbox(t, s, ephemeralID, addr, stale, true)
	seedRunningSandbox(t, s, regularID, addr, stale, false)

	if !isPastIdleDeleteWindow(mustGet(t, s, ephemeralID), cfg, now.Unix()) {
		t.Fatal("request path should refuse the ephemeral sandbox past the delete window")
	}

	runSweep(t, s, cfg, now)

	stopped, deleted := fake.calls()
	if len(deleted) != 1 || deleted[0] != ephemeralID {
		t.Fatalf("runner deletes = %v, want [%s]", deleted, ephemeralID)
	}
	if len(stopped) != 0 {
		t.Fatalf("runner stops = %v, want none with idle stop disabled", stopped)
	}
	if rec := mustGet(t, s, ephemeralID); rec != nil {
		t.Fatalf("ephemeral row still present after sweep: %+v", rec)
	}
	if rec := mustGet(t, s, regularID); rec == nil || rec.Status != "running" {
		t.Fatalf("regular row = %+v, want untouched running row", rec)
	}
}

// The request path refuses an ephemeral sandbox as soon as its stop window
// passes, but the sweeper waits out the safety buffer before the irreversible
// delete, so the fence is always up first.
func TestIdleStopSweepHoldsEphemeralInsideSafetyBuffer(t *testing.T) {
	fake := &fakeSandboxControl{}
	addr := startFakeRunnerControl(t, fake)
	s := newSweepStore(t)
	cfg := idleSweepConfig()

	now := time.Now()
	// Past the stop window, inside the buffer.
	lastActive := now.Add(-cfg.IdleStopAfter - time.Second).Unix()
	const id = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	seedRunningSandbox(t, s, id, addr, lastActive, true)

	if !isPastIdleDeleteWindow(mustGet(t, s, id), cfg, now.Unix()) {
		t.Fatal("request path should already refuse the sandbox")
	}

	runSweep(t, s, cfg, now)

	if stopped, deleted := fake.calls(); len(stopped) != 0 || len(deleted) != 0 {
		t.Fatalf("runner calls inside buffer: stops=%v deletes=%v, want none", stopped, deleted)
	}
	if rec := mustGet(t, s, id); rec == nil || rec.Status != "running" {
		t.Fatalf("row = %+v, want untouched running row", rec)
	}
}

// A sub-second buffer must still keep the delete behind the fence.
func TestIdleStopSweepRoundsSubSecondBufferUp(t *testing.T) {
	fake := &fakeSandboxControl{}
	addr := startFakeRunnerControl(t, fake)
	s := newSweepStore(t)
	cfg := idleSweepConfig()
	cfg.IdleDeleteSafetyBuffer = 500 * time.Millisecond

	now := time.Now()
	// Exactly on the stop cutoff: listed as a stop candidate, but the fence
	// (now > lastActive + stop) is not up yet.
	lastActive := now.Add(-cfg.IdleStopAfter).Unix()
	const id = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	seedRunningSandbox(t, s, id, addr, lastActive, true)

	if isPastIdleDeleteWindow(mustGet(t, s, id), cfg, now.Unix()) {
		t.Fatal("precondition: request path should still serve the sandbox")
	}

	runSweep(t, s, cfg, now)

	if _, deleted := fake.calls(); len(deleted) != 0 {
		t.Fatalf("runner deletes = %v, want none while the sandbox is still reachable", deleted)
	}
	if rec := mustGet(t, s, id); rec == nil {
		t.Fatal("row deleted while the request path still served it")
	}
}

func TestIdleSeconds(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want int64
	}{
		{0, 0},
		{time.Millisecond, 1},
		{500 * time.Millisecond, 1},
		{time.Second, 1},
		{1500 * time.Millisecond, 2},
		{time.Minute, 60},
		{time.Hour + 500*time.Millisecond, 3601},
		// ParseDuration accepts up to MaxInt64 ns; rounding up must not overflow
		// into a negative cutoff that makes every sandbox look idle.
		{time.Duration(math.MaxInt64), math.MaxInt64/int64(time.Second) + 1},
	}
	for _, tc := range cases {
		if got := idleSeconds(tc.in); got != tc.want {
			t.Errorf("idleSeconds(%s) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// A failed runner delete leaves the row exactly as it was, so the next sweep
// retries; it must not fall through to the stop path either.
func TestIdleStopSweepRetriesEphemeralDeleteAfterRunnerFailure(t *testing.T) {
	fake := &fakeSandboxControl{}
	addr := startFakeRunnerControl(t, fake)
	s := newSweepStore(t)
	cfg := idleSweepConfig()

	now := time.Now()
	stale := now.Add(-cfg.IdleStopAfter - cfg.IdleDeleteSafetyBuffer - time.Second).Unix()
	const id = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	seedRunningSandbox(t, s, id, addr, stale, true)

	fake.failDeletes(status.Error(codes.Unavailable, "runner busy"))
	runSweep(t, s, cfg, now)

	if stopped, _ := fake.calls(); len(stopped) != 0 {
		t.Fatalf("runner stops after failed delete = %v, want none", stopped)
	}
	if rec := mustGet(t, s, id); rec == nil || rec.Status != "running" || !rec.Ephemeral {
		t.Fatalf("row after failed delete = %+v, want unchanged running ephemeral row", rec)
	}

	fake.failDeletes(nil)
	runSweep(t, s, cfg, now.Add(cfg.IdleSweepInterval))

	if _, deleted := fake.calls(); len(deleted) != 1 || deleted[0] != id {
		t.Fatalf("runner deletes on retry = %v, want [%s]", deleted, id)
	}
	if rec := mustGet(t, s, id); rec != nil {
		t.Fatalf("row still present after retry: %+v", rec)
	}
}

func TestIsPastIdleDeleteWindow(t *testing.T) {
	const now int64 = 1_000_000
	cfg := &config.APIConfig{IdleStopAfter: 100 * time.Second, IdleDeleteAfter: 1000 * time.Second}
	noStop := &config.APIConfig{IdleDeleteAfter: 1000 * time.Second}
	noDelete := &config.APIConfig{IdleStopAfter: 100 * time.Second}
	// Fractional windows round up to whole seconds, never down.
	fractional := &config.APIConfig{IdleStopAfter: 100*time.Second + 500*time.Millisecond, IdleDeleteAfter: 1000*time.Second + 500*time.Millisecond}

	cases := []struct {
		name         string
		cfg          *config.APIConfig
		ephemeral    bool
		lastActiveAt int64
		want         bool
	}{
		{"regular inside delete window", cfg, false, now - 999, false},
		{"regular past delete window", cfg, false, now - 1001, true},
		{"regular past stop window only", cfg, false, now - 101, false},
		{"ephemeral inside stop window", cfg, true, now - 99, false},
		{"ephemeral past stop window", cfg, true, now - 101, true},
		{"ephemeral falls back to delete window when stop disabled", noStop, true, now - 101, false},
		{"ephemeral past delete window when stop disabled", noStop, true, now - 1001, true},
		{"ephemeral past stop window when delete disabled", noDelete, true, now - 101, true},
		{"regular never fenced when delete disabled", noDelete, false, now - 100_000, false},
		{"regular fractional window not fenced at truncated edge", fractional, false, now - 1001, false},
		{"regular fractional window fenced past rounded-up edge", fractional, false, now - 1002, true},
		{"ephemeral fractional window not fenced at truncated edge", fractional, true, now - 101, false},
		{"ephemeral fractional window fenced past rounded-up edge", fractional, true, now - 102, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &store.SandboxRecord{LastActiveAt: tc.lastActiveAt, Ephemeral: tc.ephemeral}
			if got := isPastIdleDeleteWindow(rec, tc.cfg, now); got != tc.want {
				t.Fatalf("isPastIdleDeleteWindow = %v, want %v", got, tc.want)
			}
		})
	}
}

func shrinkRunnerLifecycleBudget(t *testing.T, d time.Duration) {
	t.Helper()
	prev := runnerLifecycleBudget
	runnerLifecycleBudget = d
	t.Cleanup(func() { runnerLifecycleBudget = prev })
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func staleID(n int) string {
	const hex = "0123456789abcdef"
	c := hex[n%len(hex)]
	return strings.Repeat(string(c), 8) + "-" + strings.Repeat(string(c), 4) + "-4" + strings.Repeat(string(c), 3) + "-8" + strings.Repeat(string(c), 3) + "-" + strings.Repeat(string(c), 12)
}

func TestInterleaveByRunner(t *testing.T) {
	rec := func(id, runner string) *store.SandboxRecord {
		return &store.SandboxRecord{ID: id, RunnerID: runner}
	}
	in := []*store.SandboxRecord{
		rec("a1", "A"), rec("a2", "A"), rec("a3", "A"),
		nil,
		rec("b1", "B"),
		rec("c1", ""), // no runner id: keyed by its stored control address
	}
	in[5].RunnerControlGRPCAddr = "c:9091"

	var got []string
	for _, r := range interleaveByRunner(in) {
		got = append(got, r.ID)
	}
	want := []string{"a1", "b1", "c1", "a2", "a3"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// Exactly the configured number of runner calls are in flight: no fewer, no more.
func TestIdleStopSweepBoundsConcurrency(t *testing.T) {
	const limit, candidates = 3, 6

	var inFlight, peak atomic.Int32
	release := make(chan struct{})
	fake := &fakeSandboxControl{stopHook: func(ctx context.Context) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
	}}
	addr := startFakeRunnerControl(t, fake)
	s := newSweepStore(t)
	cfg := idleSweepConfig()
	cfg.IdleSweepConcurrency = limit

	now := time.Now()
	stale := now.Add(-cfg.IdleStopAfter - time.Second).Unix()
	for i := 0; i < candidates; i++ {
		seedRunningSandbox(t, s, staleID(i), addr, stale, false)
	}

	done := make(chan sweepStats, 1)
	go func() {
		done <- sweepIdleStopSandboxes(context.Background(), s, registry.New(45*time.Second), cfg, runnerControlTLS(cfg), now)
	}()

	waitFor(t, "the pool to fill", func() bool { return inFlight.Load() == limit })
	time.Sleep(100 * time.Millisecond) // room for a fourth call to arrive if unbounded
	if got := peak.Load(); got != limit {
		t.Fatalf("peak in-flight stops = %d, want %d", got, limit)
	}
	close(release)

	stats := <-done
	if stats.candidates != candidates || stats.acted != candidates {
		t.Fatalf("stats = %+v, want %d candidates all acted", stats, candidates)
	}
	if got := peak.Load(); got != limit {
		t.Fatalf("peak in-flight stops after drain = %d, want %d", got, limit)
	}
	for i := 0; i < candidates; i++ {
		if rec := mustGet(t, s, staleID(i)); rec == nil || rec.Status != "stopped" {
			t.Fatalf("row %d = %+v, want stopped", i, rec)
		}
	}
}

// A hanging runner does not hold up the healthy one; its stops are abandoned at
// the deadline and retried next sweep.
func TestIdleStopSweepIsolatesHangingRunner(t *testing.T) {
	shrinkRunnerLifecycleBudget(t, 300*time.Millisecond)

	healthy := &fakeSandboxControl{}
	healthyAddr := startFakeRunnerControl(t, healthy)

	var hang atomic.Bool
	hang.Store(true)
	hanging := &fakeSandboxControl{stopHook: func(ctx context.Context) {
		if hang.Load() {
			<-ctx.Done()
		}
	}}
	hangingAddr := startFakeRunnerControl(t, hanging)

	s := newSweepStore(t)
	cfg := idleSweepConfig()
	cfg.IdleSweepConcurrency = 4

	now := time.Now()
	stale := now.Add(-cfg.IdleStopAfter - time.Second).Unix()
	healthyIDs := []string{staleID(0), staleID(1)}
	hangingIDs := []string{staleID(2), staleID(3)}
	for _, id := range healthyIDs {
		seedRunningSandbox(t, s, id, healthyAddr, stale, false)
	}
	for _, id := range hangingIDs {
		seedRunningSandbox(t, s, id, hangingAddr, stale, false)
	}

	start := time.Now()
	stats := sweepIdleStopSandboxes(context.Background(), s, registry.New(45*time.Second), cfg, runnerControlTLS(cfg), now)
	if elapsed := time.Since(start); elapsed > 10*runnerLifecycleBudget {
		t.Fatalf("sweep took %s with a %s per-call budget: the hanging runner held the sweep", elapsed, runnerLifecycleBudget)
	}
	if stats.acted != 2 || stats.failed != 2 || stats.skippedUnreachable != 0 {
		t.Fatalf("stats = %+v, want 2 acted, 2 failed (deadline), 0 skipped", stats)
	}
	for _, id := range healthyIDs {
		if rec := mustGet(t, s, id); rec == nil || rec.Status != "stopped" {
			t.Fatalf("healthy runner row %s = %+v, want stopped in the same sweep", id, rec)
		}
	}
	for _, id := range hangingIDs {
		if rec := mustGet(t, s, id); rec == nil || rec.Status != "running" {
			t.Fatalf("hanging runner row %s = %+v, want left running for retry", id, rec)
		}
	}

	hang.Store(false)
	stats = sweepIdleStopSandboxes(context.Background(), s, registry.New(45*time.Second), cfg, runnerControlTLS(cfg), now.Add(cfg.IdleSweepInterval))
	if stats.acted != 2 || stats.failed != 0 {
		t.Fatalf("retry stats = %+v, want the 2 abandoned stops to succeed", stats)
	}
	for _, id := range hangingIDs {
		if rec := mustGet(t, s, id); rec == nil || rec.Status != "stopped" {
			t.Fatalf("row %s after retry = %+v, want stopped", id, rec)
		}
	}
}

// An unreachable runner is dialled once per sweep; its other candidates are skipped.
func TestIdleStopSweepSkipsUnreachableRunnerAfterFirstFailure(t *testing.T) {
	healthy := &fakeSandboxControl{}
	healthyAddr := startFakeRunnerControl(t, healthy)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadAddr := lis.Addr().String()
	_ = lis.Close()

	s := newSweepStore(t)
	cfg := idleSweepConfig()
	cfg.IdleSweepConcurrency = 1 // serial, so the skip count is exact

	now := time.Now()
	stale := now.Add(-cfg.IdleStopAfter - time.Second).Unix()
	deadIDs := []string{staleID(0), staleID(1), staleID(2)}
	for _, id := range deadIDs {
		seedRunningSandbox(t, s, id, deadAddr, stale, false)
	}
	healthyID := staleID(3)
	seedRunningSandbox(t, s, healthyID, healthyAddr, stale, false)

	stats := sweepIdleStopSandboxes(context.Background(), s, registry.New(45*time.Second), cfg, runnerControlTLS(cfg), now)
	if stats.failed != 1 || stats.skippedUnreachable != 2 || stats.acted != 1 {
		t.Fatalf("stats = %+v, want 1 failed (the probe), 2 skipped, 1 acted", stats)
	}
	if rec := mustGet(t, s, healthyID); rec == nil || rec.Status != "stopped" {
		t.Fatalf("healthy row = %+v, want stopped", rec)
	}
	for _, id := range deadIDs {
		if rec := mustGet(t, s, id); rec == nil || rec.Status != "running" {
			t.Fatalf("dead runner row %s = %+v, want left running for retry", id, rec)
		}
	}
	if stopped, _ := healthy.calls(); len(stopped) != 1 || stopped[0] != healthyID {
		t.Fatalf("healthy runner stops = %v, want [%s]", stopped, healthyID)
	}
}
