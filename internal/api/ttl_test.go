package api

import (
	"context"
	"crypto/tls"
	"net"
	"sync"
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
}

func (f *fakeSandboxControl) CreateSandbox(_ context.Context, _ *pb.CreateSandboxRequest) (*pb.CreateSandboxResponse, error) {
	return &pb.CreateSandboxResponse{ContainerIp: "10.0.0.2"}, nil
}

func (f *fakeSandboxControl) StopSandbox(_ context.Context, req *pb.StopSandboxRequest) (*pb.StopSandboxResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, req.GetSandboxId())
	return &pb.StopSandboxResponse{}, nil
}

func (f *fakeSandboxControl) DeleteSandbox(_ context.Context, req *pb.DeleteSandboxRequest) (*pb.DeleteSandboxResponse, error) {
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

	sweepIdleStopSandboxes(context.Background(), s, registry.New(45*time.Second), cfg, runnerControlTLS(cfg), now)

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

	sweepIdleStopSandboxes(context.Background(), s, registry.New(45*time.Second), cfg, runnerControlTLS(cfg), now)

	if stopped, deleted := fake.calls(); len(stopped) != 0 || len(deleted) != 0 {
		t.Fatalf("runner calls inside buffer: stops=%v deletes=%v, want none", stopped, deleted)
	}
	if rec := mustGet(t, s, id); rec == nil || rec.Status != "running" {
		t.Fatalf("row = %+v, want untouched running row", rec)
	}
}

// A failed runner delete leaves the row exactly as it was, so the next sweep
// retries; it must not fall through to the stop path either.
func TestIdleStopSweepRetriesEphemeralDeleteAfterRunnerFailure(t *testing.T) {
	fake := &fakeSandboxControl{}
	addr := startFakeRunnerControl(t, fake)
	s := newSweepStore(t)
	cfg := idleSweepConfig()
	reg := registry.New(45 * time.Second)
	tlsCfg := runnerControlTLS(cfg)

	now := time.Now()
	stale := now.Add(-cfg.IdleStopAfter - cfg.IdleDeleteSafetyBuffer - time.Second).Unix()
	const id = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	seedRunningSandbox(t, s, id, addr, stale, true)

	fake.failDeletes(status.Error(codes.Unavailable, "runner busy"))
	sweepIdleStopSandboxes(context.Background(), s, reg, cfg, tlsCfg, now)

	if stopped, _ := fake.calls(); len(stopped) != 0 {
		t.Fatalf("runner stops after failed delete = %v, want none", stopped)
	}
	if rec := mustGet(t, s, id); rec == nil || rec.Status != "running" || !rec.Ephemeral {
		t.Fatalf("row after failed delete = %+v, want unchanged running ephemeral row", rec)
	}

	fake.failDeletes(nil)
	sweepIdleStopSandboxes(context.Background(), s, reg, cfg, tlsCfg, now.Add(cfg.IdleSweepInterval))

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
