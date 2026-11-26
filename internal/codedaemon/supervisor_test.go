package codedaemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDispatchRejectsActiveTaskID(t *testing.T) {
	d := testDaemon(t, Config{})
	dir := d.cfg.WorkDir
	if err := os.MkdirAll(filepath.Join(dir, "out"), 0o700); err != nil {
		t.Fatal(err)
	}
	x, err := d.supervisor.dispatch(t.Context(), "same", dir, d.cfg.taskTimeout(0), d.markDispatched)
	if err != nil {
		t.Fatal(err)
	}
	defer x.close()
	if _, err := d.supervisor.dispatch(t.Context(), "same", dir, d.cfg.taskTimeout(0), d.markDispatched); !errors.Is(err, errDuplicateTask) {
		t.Fatalf("duplicate dispatch: %v", err)
	}
	x.worker.mu.Lock()
	registered := x.worker.executionsByTaskID["same"]
	x.worker.mu.Unlock()
	if registered != x {
		t.Fatal("duplicate dispatch replaced the active receiver")
	}
}

func TestFailedWorkerStartIsUnhealthyAndNotRetried(t *testing.T) {
	d := testDaemon(t, Config{RuntimeCommand: []string{filepath.Join(t.TempDir(), "missing")}})
	if _, err := d.supervisor.dispatch(t.Context(), "first", d.cfg.WorkDir, d.cfg.taskTimeout(0), d.markDispatched); err == nil || errors.Is(err, errWorkerUnavailable) {
		t.Fatalf("want the start error, got %v", err)
	}
	probe := httptest.NewRecorder()
	d.Handler().ServeHTTP(probe, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if probe.Code != http.StatusServiceUnavailable {
		t.Fatalf("worker failed to start but health probe returned %d", probe.Code)
	}
	if _, err := d.supervisor.dispatch(t.Context(), "second", d.cfg.WorkDir, d.cfg.taskTimeout(0), d.markDispatched); !errors.Is(err, errWorkerUnavailable) {
		t.Fatalf("unexpected dispatch error: %v", err)
	}
}

func TestUnreadyWorkerIsStoppedAfterCallerGivesUp(t *testing.T) {
	d := testDaemon(t, Config{RuntimeCommand: []string{"/bin/sleep", "60"}})
	d.supervisor.startupTimeout = 200 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := d.supervisor.dispatch(ctx, "first", d.cfg.WorkDir, d.cfg.taskTimeout(0), d.markDispatched); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unexpected dispatch error: %v", err)
	}
	d.supervisor.workerMu.Lock()
	w := d.supervisor.worker
	d.supervisor.workerMu.Unlock()
	select {
	case <-w.done:
	case <-time.After(5 * time.Second):
		t.Fatal("unready runtime was not stopped")
	}
	probe := httptest.NewRecorder()
	d.Handler().ServeHTTP(probe, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if probe.Code != http.StatusServiceUnavailable {
		t.Fatalf("runtime never became ready but health probe returned %d", probe.Code)
	}
}
