package codedaemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	if os.Args[len(os.Args)-1] == "runtime-child" {
		conn, err := net.Dial("tcp4", os.Args[len(os.Args)-2])
		if err != nil {
			os.Exit(2)
		}
		defer conn.Close()
		_, _ = io.Copy(io.Discard, conn)
		return
	}
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
		if m.Type == "cancel" {
			if enc.Encode(workerMessage{Type: "cancelled", TaskID: m.TaskID}) != nil {
				return
			}
			continue
		}
		if m.Type != "start" {
			os.Exit(2)
		}
		if m.TaskID == "largest" || m.TaskID == "oversized" {
			// Pad the error so the message, with its newline, is exactly
			// maxControlMessage bytes, or one byte more.
			msg := workerMessage{Type: "error", TaskID: m.TaskID, Error: json.RawMessage(`""`)}
			b, _ := json.Marshal(msg)
			pad := maxControlMessage - len(b) - 1
			if m.TaskID == "oversized" {
				pad++
			}
			msg.Error = json.RawMessage(`"` + strings.Repeat("x", pad) + `"`)
			if enc.Encode(msg) != nil {
				os.Exit(2)
			}
			continue
		}
		if m.TaskID == "user-error" {
			// A runtime keeps console output in result.json, also on a user error.
			if os.WriteFile(filepath.Join(m.Dir, "out", "result.json"), []byte(`{"console":["before throw"]}`), 0o600) != nil ||
				enc.Encode(workerMessage{Type: "error", TaskID: m.TaskID, Error: json.RawMessage(`{"message":"boom"}`)}) != nil {
				os.Exit(2)
			}
			continue
		}
		if m.TaskID == "quiet" {
			time.Sleep(heartbeatInterval + 250*time.Millisecond)
		}
		if m.TaskID == "hang" {
			time.Sleep(time.Minute)
		}
		if m.TaskID == "pending" || m.TaskID == "cooperative" {
			continue
		}
		if m.TaskID == "crash" || m.TaskID == "oom" {
			if m.TaskID == "oom" {
				_, _ = os.Stderr.WriteString("FATAL ERROR: JavaScript heap out of memory\n")
			}
			os.Exit(2)
		}
		_, _ = os.Stdout.WriteString("raw-runtime-stdout\n")
		_, _ = os.Stderr.WriteString("raw-runtime-stderr\n")
		input, err := os.ReadFile(filepath.Join(m.Dir, "in", "bin", "0"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			os.Exit(2)
		}
		if m.TaskID == "hang-with-child" {
			child := exec.Command(os.Args[0], "-test.run=^TestRuntimeFixture$", "--", string(input), "runtime-child")
			child.ExtraFiles = []*os.File{f}
			_ = child.Run()
			os.Exit(2)
		}
		result, _ := json.Marshal([]string{strconv.Itoa(os.Getpid()), m.TaskID, string(input)})
		var heap int64
		if m.TaskID == "large_heap" {
			heap = 4096
		}
		if os.WriteFile(filepath.Join(m.Dir, "out", "result.json"), result, 0o600) != nil ||
			enc.Encode(workerMessage{Type: "done", TaskID: m.TaskID, HeapUsedBytes: heap}) != nil {
			os.Exit(2)
		}
		if m.TaskID == "exit-after-result" {
			os.Exit(3)
		}
	}
}

func TestWorkerExitPreservesResultAndAllowsRestart(t *testing.T) {
	d := testDaemon(t, Config{})
	req := taskRequest(t, `{"taskId":"exit-after-result"}`, nil)
	spec, mr := d.admit(httptest.NewRecorder(), req)
	if mr == nil {
		t.Fatal("request was not admitted")
	}
	defer d.releaseLane()
	defer d.releaseTask(spec.TaskID)
	dir, err := d.stageInputs(mr)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	x, err := d.dispatch(t.Context(), spec.TaskID, dir, d.taskTimeout(0))
	if err != nil {
		t.Fatal(err)
	}
	defer d.releaseExecution(x)
	select {
	case <-x.worker.done:
	case <-time.After(3 * time.Second):
		t.Fatal("runtime did not exit")
	}
	// The buffered terminal result is still readable after process cleanup.
	outcome := x.watch(t.Context())
	if _, done := d.resultEvent(spec, root, outcome.result).(doneEvent); outcome.code != "" || !done {
		t.Fatalf("terminal outcome = %+v", outcome)
	}
	w, err := d.getWorker(t.Context())
	if err != nil || w == x.worker {
		t.Fatalf("did not start a fresh runtime: %v", err)
	}
}

func TestDispatchAfterKillWaitsForFreshRuntime(t *testing.T) {
	d := testDaemon(t, Config{})
	old, err := d.getWorker(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	old.kill()
	w, err := d.getWorker(t.Context())
	if err != nil || w == old {
		t.Fatalf("did not start a fresh runtime: %v", err)
	}
	select {
	case <-old.done:
	default:
		t.Fatal("started a fresh runtime before the old one was reaped")
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
		dir, err := d.stageInputs(mr)
		if err != nil {
			t.Fatal(err)
		}
		x, err := d.dispatch(ctx, spec.TaskID, dir, d.taskTimeout(0))
		if err != nil {
			t.Fatal(err)
		}
		defer d.releaseExecution(x)
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
	x, err := d.dispatch(t.Context(), "same", dir, d.taskTimeout(0))
	if err != nil {
		t.Fatal(err)
	}
	defer d.releaseExecution(x)
	if _, err := d.dispatch(t.Context(), "same", dir, d.taskTimeout(0)); !errors.Is(err, errDuplicateTask) {
		t.Fatalf("duplicate dispatch: %v", err)
	}
	x.worker.mu.Lock()
	registered := x.worker.executionsByTaskID["same"]
	x.worker.mu.Unlock()
	if registered != x {
		t.Fatal("duplicate dispatch replaced the active receiver")
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestControlMessageLimit(t *testing.T) {
	var logs syncBuffer
	defaultLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(defaultLogger) })
	d := testDaemon(t, Config{})
	result := func(id string) (workerMessage, bool) {
		t.Helper()
		x, err := d.dispatch(t.Context(), id, d.cfg.WorkDir, d.taskTimeout(0))
		if err != nil {
			t.Fatal(err)
		}
		defer d.releaseExecution(x)
		select {
		case msg, ok := <-x.result:
			return msg, ok
		case <-time.After(5 * time.Second):
			t.Fatal("missing runtime result")
			return workerMessage{}, false
		}
	}
	if msg, ok := result("largest"); !ok || msg.Type != "error" {
		t.Fatalf("runtime did not survive the largest control message: %q", msg.Type)
	}
	if _, ok := result("oversized"); ok {
		t.Fatal("runtime survived a message over the limit")
	}
	if want := fmt.Sprintf("control message over %d bytes", maxControlMessage); !strings.Contains(logs.String(), want) {
		t.Fatalf("log does not say why the runtime stopped: %q", logs.String())
	}
	probe := httptest.NewRecorder()
	d.Handler().ServeHTTP(probe, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if probe.Code != http.StatusServiceUnavailable {
		t.Fatalf("runtime stopped but health probe returned %d", probe.Code)
	}
}

func TestFailedWorkerStartIsUnhealthyAndNotRetried(t *testing.T) {
	d := testDaemon(t, Config{RuntimeCommand: []string{filepath.Join(t.TempDir(), "missing")}})
	if _, err := d.dispatch(t.Context(), "first", d.cfg.WorkDir, d.taskTimeout(0)); err == nil || errors.Is(err, errWorkerUnavailable) {
		t.Fatalf("want the start error, got %v", err)
	}
	probe := httptest.NewRecorder()
	d.Handler().ServeHTTP(probe, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if probe.Code != http.StatusServiceUnavailable {
		t.Fatalf("worker failed to start but health probe returned %d", probe.Code)
	}
	if _, err := d.dispatch(t.Context(), "second", d.cfg.WorkDir, d.taskTimeout(0)); !errors.Is(err, errWorkerUnavailable) {
		t.Fatalf("unexpected dispatch error: %v", err)
	}
}

func TestUnreadyWorkerIsStoppedAfterCallerGivesUp(t *testing.T) {
	d := testDaemon(t, Config{RuntimeCommand: []string{"/bin/sleep", "60"}})
	d.startupTimeout = 200 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := d.dispatch(ctx, "first", d.cfg.WorkDir, d.taskTimeout(0)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unexpected dispatch error: %v", err)
	}
	d.workerMu.Lock()
	w := d.worker
	d.workerMu.Unlock()
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
			pending, err := d.dispatch(t.Context(), "pending", d.cfg.WorkDir, d.taskTimeout(0))
			if err != nil {
				t.Fatal(err)
			}
			defer d.releaseExecution(pending)
			if err := listener.SetDeadline(time.Now().Add(timeout)); err != nil {
				t.Fatal(err)
			}
			descendant, err := listener.AcceptTCP()
			if err != nil {
				t.Fatal(err)
			}
			defer descendant.Close()

			if mode == "crash" {
				crash, err := d.dispatch(t.Context(), "crash", d.cfg.WorkDir, d.taskTimeout(0))
				if err != nil {
					t.Fatal(err)
				}
				defer d.releaseExecution(crash)
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
			if mode == "shutdown" {
				if _, err := d.dispatch(t.Context(), "next", d.cfg.WorkDir, d.taskTimeout(0)); !errors.Is(err, errWorkerUnavailable) {
					t.Fatalf("unexpected dispatch error: %v", err)
				}
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
