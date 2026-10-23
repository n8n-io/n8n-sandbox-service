package api

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/metrics"
)

type archiveHarness struct {
	router http.Handler
	s      store.SandboxStore
	ctl    *fakeSandboxControl
	dir    string
	arch   *archiver
}

func newArchiveHarness(t *testing.T) *archiveHarness {
	t.Helper()
	ctl := &fakeSandboxControl{archiveBody: []byte("sealed disk")}
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	srv := httptest.NewServer(nil)
	t.Cleanup(srv.Close)
	cfg := idleSweepConfig()
	cfg.APIKeys = map[string]struct{}{"admin-key": {}}
	cfg.DefaultMaxSandboxes = 50
	cfg.MaxIdleWindow = 7 * 24 * time.Hour
	cfg.ArchiveDir = filepath.Join(t.TempDir(), "archives")
	cfg.ArchiveURL = srv.URL
	cfg.ArchiveKey, _ = hex.DecodeString("00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	reg := registry.New(time.Minute)
	reg.UpsertRunner(registry.Runner{ID: "runner-1", HTTPBaseURL: "https://127.0.0.1:9", ControlGRPCAddr: startFakeRunnerControl(t, ctl), Healthy: true, CapacityTotal: 10})
	router, err := NewGatewayRouter(s, cfg, reg, metrics.NewAPIRecorder(false))
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = router
	arch, _, err := newArchiver(s, reg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &archiveHarness{router: router, s: s, ctl: ctl, dir: cfg.ArchiveDir, arch: arch}
}

func (h *archiveHarness) objects(t *testing.T) int {
	t.Helper()
	n := 0
	_ = filepath.Walk(h.dir, func(_ string, info os.FileInfo, _ error) error {
		if info != nil && !info.IsDir() {
			n++
		}
		return nil
	})
	return n
}

func (h *archiveHarness) get(t *testing.T, id string) SandboxResponse {
	t.Helper()
	rr := doJSON(t, h.router, http.MethodGet, "/sandboxes/"+id, "admin-key", "")
	var resp SandboxResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	return resp
}

// Archive on request, restore on first use: the sandbox keeps its options,
// the object exists only while it is archived, and the runner copy goes.
func TestArchiveAndRestore(t *testing.T) {
	h := newArchiveHarness(t)
	const id = "c0c0c0c0-c0c0-4c0c-8c0c-c0c0c0c0c0c0"
	if rr := postCreateSandbox(t, h.router, "admin-key", `{"id":"`+id+`","egress":"none","idle_archive_after":3600}`); rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}

	rr := doJSON(t, h.router, http.MethodPost, "/sandboxes/"+id+"/archive", "admin-key", "")
	var resp SandboxResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if rr.Code != http.StatusOK || resp.Status != statusArchived || resp.ArchivedAt == 0 {
		t.Fatalf("archive: %d %s", rr.Code, rr.Body.String())
	}
	if stopped, deleted := h.ctl.calls(); len(stopped) != 1 || len(deleted) != 1 {
		t.Fatalf("runner stops %v deletes %v", stopped, deleted)
	}
	rec, _ := h.s.Get(id)
	if h.objects(t) != 1 || rec.RunnerID != "" || rec.ArchiveKey == "" {
		t.Fatalf("objects %d, record %+v", h.objects(t), rec)
	}

	// The first request starts the restore and is told to come back.
	rr = doJSON(t, h.router, http.MethodPost, "/sandboxes/"+id+"/executions", "admin-key", `{"command":"true"}`)
	var body APIError
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if rr.Code != http.StatusServiceUnavailable || body.Reason != reasonRestoring || rr.Header().Get("Retry-After") == "" {
		t.Fatalf("exec on archived: %d %s", rr.Code, rr.Body.String())
	}
	waitFor(t, "restored", func() bool { return h.get(t, id).Status == "running" })
	rec, _ = h.s.Get(id)
	if rec.RunnerID != "runner-1" || rec.ArchiveObject != "" || h.objects(t) != 0 {
		t.Fatalf("after restore: %+v, objects %d", rec, h.objects(t))
	}
	if len(h.ctl.restored) != 1 || h.ctl.restored[0] != `{"egress":"none"}` {
		t.Fatalf("restore options %v", h.ctl.restored)
	}

	// Archived again, then deleted: the object goes with it.
	if rr := doJSON(t, h.router, http.MethodPost, "/sandboxes/"+id+"/archive", "admin-key", ""); rr.Code != http.StatusOK {
		t.Fatal(rr.Body.String())
	}
	if rr := doJSON(t, h.router, http.MethodDelete, "/sandboxes/"+id, "admin-key", ""); rr.Code != http.StatusNoContent || h.objects(t) != 0 {
		t.Fatalf("delete archived: %d, objects %d", rr.Code, h.objects(t))
	}
}

// An archive that does not open deletes the sandbox; an ephemeral one is
// never archived; a sandbox the sweeper finds idle past its window is.
func TestArchiveLostEphemeralAndSweep(t *testing.T) {
	h := newArchiveHarness(t)
	const lost = "c1c1c1c1-c1c1-4c1c-8c1c-c1c1c1c1c1c1"
	postCreateSandbox(t, h.router, "admin-key", `{"id":"`+lost+`"}`)
	if rr := doJSON(t, h.router, http.MethodPost, "/sandboxes/"+lost+"/archive", "admin-key", ""); rr.Code != http.StatusOK {
		t.Fatal(rr.Body.String())
	}
	// A runner that fails the download leaves the sandbox archived, and the
	// next attempt waits restoreRetryDelay.
	defer func(d time.Duration) { restoreRetryDelay = d }(restoreRetryDelay)
	h.ctl.mu.Lock()
	h.ctl.restoreErr = status.Error(codes.Unavailable, "archive download cut short")
	h.ctl.mu.Unlock()
	doJSON(t, h.router, http.MethodPost, "/sandboxes/"+lost+"/restore", "admin-key", "")
	waitFor(t, "failed restore back to archived", func() bool {
		h.ctl.mu.Lock()
		runs := h.ctl.restoreRuns
		h.ctl.mu.Unlock()
		rec, _ := h.s.Get(lost)
		return runs == 1 && rec != nil && rec.Status == statusArchived
	})
	if rec, _ := h.s.Get(lost); restorable(rec, time.Now()) {
		t.Fatal("a failed restore is retried at once")
	}
	restoreRetryDelay = 0
	h.ctl.mu.Lock()
	h.ctl.restoreErr = status.Error(codes.DataLoss, "archive does not open")
	h.ctl.mu.Unlock()
	doJSON(t, h.router, http.MethodPost, "/sandboxes/"+lost+"/restore", "admin-key", "")
	waitFor(t, "lost sandbox deleted", func() bool { rec, _ := h.s.Get(lost); return rec == nil })

	// An object the store no longer has is lost without asking a runner.
	const gone = "c4c4c4c4-c4c4-4c4c-8c4c-c4c4c4c4c4c4"
	postCreateSandbox(t, h.router, "admin-key", `{"id":"`+gone+`"}`)
	if rr := doJSON(t, h.router, http.MethodPost, "/sandboxes/"+gone+"/archive", "admin-key", ""); rr.Code != http.StatusOK {
		t.Fatal(rr.Body.String())
	}
	h.ctl.mu.Lock()
	h.ctl.restoreErr = nil
	h.ctl.mu.Unlock()
	rec, _ := h.s.Get(gone)
	if err := h.arch.store.Delete(t.Context(), rec.ArchiveObject); err != nil {
		t.Fatal(err)
	}
	doJSON(t, h.router, http.MethodPost, "/sandboxes/"+gone+"/restore", "admin-key", "")
	waitFor(t, "sandbox with a missing archive deleted", func() bool { rec, _ := h.s.Get(gone); return rec == nil })

	const eph = "c2c2c2c2-c2c2-4c2c-8c2c-c2c2c2c2c2c2"
	postCreateSandbox(t, h.router, "admin-key", `{"id":"`+eph+`","ephemeral":true}`)
	rr := doJSON(t, h.router, http.MethodPost, "/sandboxes/"+eph+"/archive", "admin-key", "")
	var body APIError
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if rr.Code != http.StatusConflict || body.Reason != reasonEphemeral {
		t.Fatalf("ephemeral: %d %s", rr.Code, rr.Body.String())
	}
	if rr := postCreateSandbox(t, h.router, "admin-key", `{"ephemeral":true,"idle_archive_after":600}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("ephemeral with archive window: %d", rr.Code)
	}
	if rr := postCreateSandbox(t, h.router, "admin-key", `{"idle_stop_after":3600,"idle_archive_after":600}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("archive before stop: %d", rr.Code)
	}

	const idle = "c3c3c3c3-c3c3-4c3c-8c3c-c3c3c3c3c3c3"
	if err := h.s.Create(&store.SandboxRecord{ID: idle, Status: "stopped", LastActiveAt: time.Now().Add(-2 * time.Hour).Unix(),
		IdleArchiveAfter: 3600, RunnerID: "runner-1", RunnerHTTPBase: "https://127.0.0.1:9", TenantID: store.AdminTenantID,
		RunnerControlGRPCAddr: mustRunner(t, h).ControlGRPCAddr}); err != nil {
		t.Fatal(err)
	}
	cfg := idleSweepConfig()
	cfg.IdleStopAfter, cfg.IdleDeleteAfter = 0, 0
	if err := sweepIdleSandboxes(t.Context(), h.s, h.arch.reg, cfg, runnerControlTLS(cfg), time.Now(), h.arch); err != nil {
		t.Fatal(err)
	}
	if rec, _ := h.s.Get(idle); rec == nil || rec.Status != statusArchived {
		t.Fatalf("idle sandbox = %+v", rec)
	}
}

func mustRunner(t *testing.T, h *archiveHarness) *registry.Runner {
	t.Helper()
	run, ok := h.arch.reg.Get("runner-1")
	if !ok {
		t.Fatal("no runner")
	}
	return run
}
