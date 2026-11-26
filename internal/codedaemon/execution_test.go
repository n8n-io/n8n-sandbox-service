package codedaemon

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHungTaskTimesOut(t *testing.T) {
	d := testDaemon(t, Config{})
	w := httptest.NewRecorder()
	d.Handler().ServeHTTP(w, taskRequest(t, `{"taskId":"hang","timeoutMs":50}`, nil))
	dec := json.NewDecoder(w.Body)
	var accepted, terminal decodedEvent
	if err := dec.Decode(&accepted); err != nil || accepted.Type != "accepted" {
		t.Fatalf("accepted = %+v, %v", accepted, err)
	}
	if err := decodeSkippingHeartbeats(dec, &terminal); err != nil || terminal.Type != "error" || terminal.Code != "timeout" {
		t.Fatalf("terminal = %+v, %v", terminal, err)
	}
}

type blockedFlush struct {
	http.ResponseWriter
	resume <-chan struct{}
}

func (w blockedFlush) Flush() {
	<-w.resume
}

func TestTimeoutKillsRuntimeGroupWhileAcceptedFlushIsBlocked(t *testing.T) {
	testRuntimeGroupCleanup(t, false)
}

func TestRuntimeExitKillsChildHoldingControlSocket(t *testing.T) {
	testRuntimeGroupCleanup(t, true)
}

func testRuntimeGroupCleanup(t *testing.T, killLeader bool) {
	t.Helper()
	d := testDaemon(t, Config{})
	worker, err := d.supervisor.getWorker(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := listener.SetDeadline(time.Now().Add(4 * time.Second)); err != nil {
		t.Fatal(err)
	}
	spec := `{"taskId":"hang-with-child","timeoutMs":2000}`
	if killLeader {
		spec = `{"taskId":"hang-with-child","timeoutMs":60000}`
	}
	r := taskRequest(t, spec, map[string]string{"bin/0": listener.Addr().String()})
	w := httptest.NewRecorder()
	resume := make(chan struct{})
	handled := make(chan struct{})
	go func() {
		defer close(handled)
		d.Handler().ServeHTTP(blockedFlush{w, resume}, r)
	}()
	defer func() {
		close(resume)
		<-handled
	}()

	child, err := listener.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	if killLeader {
		if err := worker.cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-worker.done:
	case <-time.After(cancelGrace + 4*time.Second):
		t.Fatal("runtime did not exit while the response flush was blocked")
	}
	if err := child.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := child.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("expected the runtime child to exit too, got %v", err)
	}
}

// The cancel arrives after the runtime reported the result, but before the
// watcher has read it.
func TestCancelAfterTaskFinishedKeepsRuntime(t *testing.T) {
	d := testDaemon(t, Config{})
	x, err := d.supervisor.dispatch(t.Context(), "no-outputs", d.cfg.WorkDir, time.Minute, d.markDispatched)
	if err != nil {
		t.Fatal(err)
	}
	for start := time.Now(); !isFinished(x); time.Sleep(time.Millisecond) {
		if time.Since(start) > 5*time.Second {
			t.Fatal("the runtime did not report the result")
		}
	}
	if d.supervisor.requestCancel("no-outputs") {
		t.Fatal("cancel of a finished task was accepted")
	}
	if outcome := x.watch(t.Context()); outcome.code != "" {
		t.Fatalf("outcome = %+v", outcome)
	}
	x.close()
	if !d.isHealthy() || x.worker.isStopped() {
		t.Fatal("late cancel stopped the runtime")
	}
}

func isFinished(x *execution) bool {
	x.worker.mu.Lock()
	defer x.worker.mu.Unlock()
	return x.finished
}

func TestTaskThatFinishesDuringCancelReportsResult(t *testing.T) {
	d := testDaemon(t, Config{})
	x, err := d.supervisor.dispatch(t.Context(), "finish-on-cancel", d.cfg.WorkDir, time.Minute, d.markDispatched)
	if err != nil {
		t.Fatal(err)
	}
	if !d.supervisor.requestCancel("finish-on-cancel") {
		t.Fatal("cancel was not accepted")
	}
	outcome := x.watch(t.Context())
	if outcome.code != "" || outcome.result.Type != "done" {
		t.Fatalf("outcome = %+v, want the task's result", outcome)
	}
	x.close()
}
