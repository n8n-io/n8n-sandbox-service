package codedaemon

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func testDaemon(t *testing.T, cfg Config) *Daemon {
	t.Helper()
	if cfg.WorkDir == "" {
		cfg.WorkDir = t.TempDir()
	}
	if cfg.RuntimeCommand == nil {
		cfg.RuntimeCommand = []string{os.Args[0], "-test.run=^TestRuntimeFixture$", "--", "runtime-fixture"}
	}
	cfg.AllowLocalClients = true
	d := New(cfg)
	t.Cleanup(d.Close)
	return d
}

func TestRuntimeFixture(t *testing.T) {
	if os.Args[len(os.Args)-1] != "runtime-fixture" {
		return
	}
	f := os.NewFile(3, "control")
	defer f.Close()
	enc, dec := json.NewEncoder(f), json.NewDecoder(f)
	if enc.Encode(workerMessage{Type: "ready"}) != nil {
		return
	}
	for {
		var m workerMessage
		if dec.Decode(&m) != nil {
			return
		}
		if m.Type != "start" || !m.Shared {
			os.Exit(2)
		}
		input, err := os.ReadFile(filepath.Join(m.Dir, "in", "bin", "0"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			os.Exit(2)
		}
		result, _ := json.Marshal([]string{strconv.Itoa(os.Getpid()), m.TaskID, string(input)})
		if os.WriteFile(filepath.Join(m.Dir, "out", "result.json"), result, 0o600) != nil ||
			enc.Encode(workerMessage{Type: "done", TaskID: m.TaskID}) != nil {
			os.Exit(2)
		}
	}
}

func TestStageAndDispatchSharedRuntime(t *testing.T) {
	d := testDaemon(t, Config{MaxConcurrency: 2})
	ctx := t.Context()
	type pending struct {
		x   *execution
		dir string
	}
	var tasks []pending
	for _, id := range []string{"first", "second"} {
		r := taskRequest(t, `{"taskId":"`+id+`"}`, map[string]string{"bin/0": id})
		spec, mr := d.admit(httptest.NewRecorder(), r)
		if mr == nil {
			t.Fatal("request was not admitted")
		}
		defer d.releaseLane()
		if err := d.reserveTask(spec.TaskID); err != nil {
			t.Fatal(err)
		}
		dir, err := d.stageInputs(mr)
		if err != nil {
			t.Fatal(err)
		}
		x, err := d.dispatch(ctx, id, dir)
		if err != nil {
			t.Fatal(err)
		}
		defer x.close()
		tasks = append(tasks, pending{x, dir})
	}
	if tasks[0].x.worker != tasks[1].x.worker {
		t.Fatal("tasks used different workers")
	}
	for _, task := range tasks {
		select {
		case msg := <-task.x.result:
			if msg.Type != "done" || msg.TaskID != task.x.id {
				t.Fatalf("unexpected result: %+v", msg)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("missing runtime result")
		}
		b, err := os.ReadFile(filepath.Join(task.dir, "out", "result.json"))
		var result []string
		if err != nil || json.Unmarshal(b, &result) != nil || len(result) != 3 || result[1] != task.x.id || result[2] != task.x.id {
			t.Fatalf("wrong result: %q (%v)", b, err)
		}
		if err := os.RemoveAll(task.dir); err != nil {
			t.Fatal(err)
		}
		d.releaseTask(task.x.id)
		if err := d.reserveTask(task.x.id); !errors.Is(err, errDuplicateTask) {
			t.Fatalf("duplicate accepted after cleanup: %v", err)
		}
	}
}

func TestDispatchRejectsActiveTaskID(t *testing.T) {
	d := testDaemon(t, Config{})
	dir := d.cfg.WorkDir
	if err := os.MkdirAll(filepath.Join(dir, "out"), 0o700); err != nil {
		t.Fatal(err)
	}
	x, err := d.dispatch(t.Context(), "same", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer x.close()
	if _, err := d.dispatch(t.Context(), "same", dir); !errors.Is(err, errDuplicateTask) {
		t.Fatalf("duplicate dispatch: %v", err)
	}
	x.worker.mu.Lock()
	registered := x.worker.executionsByTaskID["same"]
	x.worker.mu.Unlock()
	if registered != x.result {
		t.Fatal("duplicate dispatch replaced the active receiver")
	}
}

func TestFailedWorkerStartIsUnhealthyAndNotRetried(t *testing.T) {
	d := testDaemon(t, Config{RuntimeCommand: []string{filepath.Join(t.TempDir(), "missing")}})
	if _, err := d.dispatch(t.Context(), "first", d.cfg.WorkDir); err == nil || errors.Is(err, errWorkerUnavailable) {
		t.Fatalf("want the start error, got %v", err)
	}
	probe := httptest.NewRecorder()
	d.Handler().ServeHTTP(probe, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if probe.Code != http.StatusServiceUnavailable {
		t.Fatalf("worker failed to start but health probe returned %d", probe.Code)
	}
	if _, err := d.dispatch(t.Context(), "second", d.cfg.WorkDir); !errors.Is(err, errWorkerUnavailable) {
		t.Fatalf("unexpected dispatch error: %v", err)
	}
}

func TestRuntimeLifecycleFixture(t *testing.T) {
	if len(os.Args) < 5 || os.Args[len(os.Args)-3] != "lifecycle-fixture" {
		return
	}
	mode, addr := os.Args[len(os.Args)-2], os.Args[len(os.Args)-1]
	if mode == "descendant" {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			os.Exit(2)
		}
		defer conn.Close()
		_, _ = io.Copy(io.Discard, conn)
		return
	}
	f := os.NewFile(3, "control")
	defer f.Close()
	child := exec.Command(os.Args[0], "-test.run=^TestRuntimeLifecycleFixture$", "--", "lifecycle-fixture", "descendant", addr)
	child.ExtraFiles = []*os.File{f} // keep FD 3 open even if the leader crashes
	if child.Start() != nil || json.NewEncoder(f).Encode(workerMessage{Type: "ready"}) != nil {
		os.Exit(2)
	}
	dec := json.NewDecoder(f)
	for {
		var m workerMessage
		if dec.Decode(&m) != nil {
			return
		}
		if m.TaskID == "crash" {
			os.Exit(7)
		}
	}
}

func TestStoppedWorkerFailsPendingTasksAndCleansUpDescendants(t *testing.T) {
	for _, mode := range []string{"shutdown", "crash"} {
		t.Run(mode, func(t *testing.T) {
			listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			const timeout = 5 * time.Second
			d := testDaemon(t, Config{RuntimeCommand: []string{
				os.Args[0], "-test.run=^TestRuntimeLifecycleFixture$", "--", "lifecycle-fixture", "leader", listener.Addr().String(),
			}})
			pending, err := d.dispatch(t.Context(), "pending", d.cfg.WorkDir)
			if err != nil {
				t.Fatal(err)
			}
			defer pending.close()
			if err := listener.SetDeadline(time.Now().Add(timeout)); err != nil {
				t.Fatal(err)
			}
			descendant, err := listener.AcceptTCP()
			if err != nil {
				t.Fatal(err)
			}
			defer descendant.Close()

			if mode == "crash" {
				crash, err := d.dispatch(t.Context(), "crash", d.cfg.WorkDir)
				if err != nil {
					t.Fatal(err)
				}
				defer crash.close()
			} else {
				go d.Close()
			}
			select {
			case _, ok := <-pending.result:
				if ok {
					t.Fatal("unexpected result from stopped worker")
				}
			case <-time.After(timeout):
				t.Fatal("pending task was not unblocked")
			}
			if _, err := d.dispatch(t.Context(), "next", d.cfg.WorkDir); !errors.Is(err, errWorkerUnavailable) {
				t.Fatalf("unexpected dispatch error: %v", err)
			}
			probe := httptest.NewRecorder()
			d.Handler().ServeHTTP(probe, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			if probe.Code != http.StatusServiceUnavailable {
				t.Fatalf("worker stopped but health probe returned %d", probe.Code)
			}
			if err := descendant.SetReadDeadline(time.Now().Add(timeout)); err != nil {
				t.Fatal(err)
			}
			if _, err := descendant.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
				t.Fatalf("descendant connection remained open: %v", err)
			}
		})
	}
}
