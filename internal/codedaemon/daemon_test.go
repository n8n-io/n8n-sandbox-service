package codedaemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shellWorker speaks the worker protocol. The "mode" in task.json selects the
// behavior.
const shellWorker = `#!/bin/sh
echo '{"type":"ready"}' >&3
read -r line <&3
dir=$(printf '%s' "$line" | sed -n 's/.*"dir":"\([^"]*\)".*/\1/p')
task=$(cat "$dir/in/task.json")
case "$task" in
  *sleep*) sleep 30 ;;
  *throw*) echo '{"type":"error","error":{"message":"boom"}}' >&3; sleep 5 ;;
  *crash*) exit 3 ;;
  *fastexit*)
    echo "$task" > "$dir/out/result.json"
    echo '{"type":"done"}' >&3
    exit 0 ;;
  *badcallid*)
    echo '{"type":"rpc","callId":"c/1","name":"helpers.httpRequest","params":[]}' >&3
    read -r res <&3
    echo "rpc:$res"
    echo "$task" > "$dir/out/result.json"
    echo '{"type":"done"}' >&3; sleep 5 ;;
  *rpc*)
    echo '{"type":"rpc","callId":"c1","name":"helpers.httpRequest","params":[{"url":"http://x"}]}' >&3
    read -r res <&3
    echo "rpc:$res"
    echo "$task" > "$dir/out/result.json"
    echo '{"type":"done"}' >&3; sleep 5 ;;
  *)
    printf 'hello stdout\n'
    printf 'hello stderr\n' >&2
    cp "$dir/in/bin/0" "$dir/out/bin/copy"
    echo "$task" > "$dir/out/result.json"
    echo '{"type":"done","timings":{"run":1}}' >&3
    sleep 5 ;;
esac
`

type testDaemon struct {
	d    *Daemon
	base string
}

func startTestDaemon(t *testing.T, slots int, spare bool) *testDaemon {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "worker.sh")
	if err := os.WriteFile(script, []byte(shellWorker), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		WorkDir:      filepath.Join(dir, "work"),
		Runtime:      "test",
		Slots:        slots,
		TaskMemoryMB: 64,
		SpareWorkers: spare,
		OutputTTL:    time.Minute,
		WorkerEnv:    []string{"PATH=" + os.Getenv("PATH")},
		// The tests call the daemon on loopback.
		AllowLocalClients: true,
	}
	d, err := New(cfg, RuntimeSpec{Version: "1.2.3", Command: []string{"/bin/sh", script}})
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = d.Serve(ctx, lis)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	td := &testDaemon{d: d, base: "http://" + lis.Addr().String()}
	td.waitHealthy(t)
	return td
}

func (td *testDaemon) waitHealthy(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(td.base + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("daemon not healthy")
}

type inputFile struct {
	name string
	data string
}

func taskBody(t *testing.T, spec map[string]any, inputs ...inputFile) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="spec"`)
	h.Set("Content-Type", "application/json")
	pw, _ := mw.CreatePart(h)
	_ = json.NewEncoder(pw).Encode(spec)
	for _, in := range inputs {
		fw, err := mw.CreateFormFile("input", in.name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fw.Write([]byte(in.data))
	}
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

func (td *testDaemon) runTask(t *testing.T, spec map[string]any, inputs ...inputFile) (int, []taskEvent) {
	t.Helper()
	body, ct := taskBody(t, spec, inputs...)
	resp, err := http.Post(td.base+"/tasks", ct, body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	var events []taskEvent
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var ev taskEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("bad event %q: %v", sc.Text(), err)
		}
		events = append(events, ev)
	}
	return resp.StatusCode, events
}

func terminal(t *testing.T, events []taskEvent) taskEvent {
	t.Helper()
	n := 0
	var last taskEvent
	for _, ev := range events {
		if ev.Type == "done" || ev.Type == "error" {
			n++
			last = ev
		}
	}
	if n != 1 {
		t.Fatalf("want exactly one terminal event, got %d: %+v", n, events)
	}
	return last
}

func collect(events []taskEvent, typ string) string {
	var sb strings.Builder
	for _, ev := range events {
		if ev.Type == typ {
			sb.WriteString(ev.Data)
		}
	}
	return sb.String()
}

func TestTaskDoneWithInputsAndOutputs(t *testing.T) {
	td := startTestDaemon(t, 1, true)
	status, events := td.runTask(t,
		map[string]any{"taskId": "t1", "timeoutMs": 5000},
		inputFile{"task.json", `{"mode":"ok"}`},
		inputFile{"bin/0", "binary-bytes"},
	)
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	if events[0].Type != "accepted" || events[0].TaskID != "t1" {
		t.Fatalf("first event: %+v", events[0])
	}
	if got := collect(events, "stdout"); got != "hello stdout\n" {
		t.Fatalf("stdout %q", got)
	}
	if got := collect(events, "stderr"); got != "hello stderr\n" {
		t.Fatalf("stderr %q", got)
	}
	done := terminal(t, events)
	if done.Type != "done" {
		t.Fatalf("terminal: %+v", done)
	}
	if !bytes.Contains(done.Result, []byte(`"mode":"ok"`)) {
		t.Fatalf("result %s", done.Result)
	}
	if done.Timings == nil || !done.Timings.SpareHit || string(done.Timings.Worker) != `{"run":1}` {
		t.Fatalf("timings %+v", done.Timings)
	}
	paths := map[string]int64{}
	for _, o := range done.Outputs {
		paths[o.Path] = o.Size
	}
	if paths["bin/copy"] != int64(len("binary-bytes")) {
		t.Fatalf("outputs %+v", done.Outputs)
	}

	resp, err := http.Get(td.base + "/tasks/t1/outputs/bin/copy")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "binary-bytes" || resp.ContentLength != int64(len(b)) {
		t.Fatalf("output %d %q %d", resp.StatusCode, b, resp.ContentLength)
	}

	req, _ := http.NewRequest(http.MethodDelete, td.base+"/tasks/t1", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	resp, _ = http.Get(td.base + "/tasks/t1/outputs/bin/copy")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("after delete: %d", resp.StatusCode)
	}

	// The slot is clean for the next task.
	status, events = td.runTask(t, map[string]any{"taskId": "t2"},
		inputFile{"task.json", `{"mode":"ok2"}`}, inputFile{"bin/0", "x"})
	if status != 200 || terminal(t, events).Type != "done" {
		t.Fatalf("second task: %d %+v", status, events)
	}
	// The task id cannot be reused while its outputs exist.
	status, _ = td.runTask(t, map[string]any{"taskId": "t2"}, inputFile{"task.json", `{}`})
	if status != http.StatusConflict {
		t.Fatalf("reused id: %d", status)
	}
}

func TestTaskErrors(t *testing.T) {
	td := startTestDaemon(t, 1, true)

	_, events := td.runTask(t, map[string]any{"taskId": "e1"}, inputFile{"task.json", `{"mode":"throw"}`})
	ev := terminal(t, events)
	if ev.Code != "user_error" || !bytes.Contains(ev.UserError, []byte("boom")) {
		t.Fatalf("throw: %+v", ev)
	}

	_, events = td.runTask(t, map[string]any{"taskId": "e2"}, inputFile{"task.json", `{"mode":"crash"}`})
	if ev := terminal(t, events); ev.Code != "crashed" {
		t.Fatalf("crash: %+v", ev)
	}

	start := time.Now()
	_, events = td.runTask(t, map[string]any{"taskId": "e3", "timeoutMs": 300}, inputFile{"task.json", `{"mode":"sleep"}`})
	if ev := terminal(t, events); ev.Code != "timeout" {
		t.Fatalf("timeout: %+v", ev)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("timeout took %s", time.Since(start))
	}
}

// A worker exits right after "done". The exit can arrive before the message.
func TestDoneThenExitIsDone(t *testing.T) {
	td := startTestDaemon(t, 2, true)
	for i := 0; i < 30; i++ {
		_, events := td.runTask(t, map[string]any{"taskId": fmt.Sprintf("fx%d", i)}, inputFile{"task.json", `{"mode":"fastexit"}`})
		if ev := terminal(t, events); ev.Type != "done" {
			t.Fatalf("run %d: %+v", i, ev)
		}
	}
}

func TestInvalidInputs(t *testing.T) {
	td := startTestDaemon(t, 1, false)
	_, events := td.runTask(t, map[string]any{"taskId": "p1"}, inputFile{"../escape", "x"})
	if ev := terminal(t, events); ev.Code != "invalid" {
		t.Fatalf("traversal: %+v", ev)
	}
	status, _ := td.runTask(t, map[string]any{"taskId": "bad id"})
	if status != http.StatusBadRequest {
		t.Fatalf("bad id: %d", status)
	}
	// Spare workers off: the task starts a worker itself.
	_, events = td.runTask(t, map[string]any{"taskId": "p2"}, inputFile{"task.json", `{}`}, inputFile{"bin/0", "x"})
	ev := terminal(t, events)
	if ev.Type != "done" || ev.Timings.SpareHit {
		t.Fatalf("no spare: %+v", ev)
	}
}

func TestNoFreeSlotAndCancel(t *testing.T) {
	td := startTestDaemon(t, 1, true)
	body, ct := taskBody(t, map[string]any{"taskId": "busy", "timeoutMs": 20000}, inputFile{"task.json", `{"mode":"sleep"}`})
	resp, err := http.Post(td.base+"/tasks", ct, body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	line, _ := br.ReadBytes('\n')
	if !bytes.Contains(line, []byte(`"accepted"`)) {
		t.Fatalf("first line %s", line)
	}

	status, _ := td.runTask(t, map[string]any{"taskId": "other"}, inputFile{"task.json", `{}`})
	if status != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d", status)
	}

	req, _ := http.NewRequest(http.MethodDelete, td.base+"/tasks/busy", nil)
	dresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	dresp.Body.Close()
	rest, _ := io.ReadAll(br)
	if !bytes.Contains(rest, []byte(`"code":"cancelled"`)) {
		t.Fatalf("after cancel: %s", rest)
	}
}

// postRPCResult posts an RPC result and returns the status code.
func (td *testDaemon) postRPCResult(t *testing.T, taskID, callID, body string) int {
	t.Helper()
	resp, err := http.Post(td.base+"/tasks/"+taskID+"/rpc/"+callID, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// runTaskRPC runs a task and calls onRPC for each "rpc" event while the stream
// is open.
func (td *testDaemon) runTaskRPC(t *testing.T, spec map[string]any, onRPC func(ev taskEvent), inputs ...inputFile) []taskEvent {
	t.Helper()
	body, ct := taskBody(t, spec, inputs...)
	resp, err := http.Post(td.base+"/tasks", ct, body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("task status %d", resp.StatusCode)
	}
	var events []taskEvent
	dec := json.NewDecoder(resp.Body)
	for {
		var ev taskEvent
		if err := dec.Decode(&ev); err != nil {
			break
		}
		events = append(events, ev)
		if ev.Type == "rpc" {
			onRPC(ev)
		}
	}
	return events
}

func TestRPCOverTaskStream(t *testing.T) {
	td := startTestDaemon(t, 1, true)
	calls := 0
	events := td.runTaskRPC(t, map[string]any{"taskId": "r1"}, func(ev taskEvent) {
		calls++
		if ev.CallID != "c1" || ev.Name != "helpers.httpRequest" || ev.TaskID != "" {
			t.Errorf("rpc event %+v", ev)
		}
		if string(ev.Params) != `[{"url":"http://x"}]` {
			t.Errorf("params %s", ev.Params)
		}
		if code := td.postRPCResult(t, "r1", ev.CallID, `{"ok":true,"data":{"status":200}}`); code != http.StatusNoContent {
			t.Errorf("result status %d", code)
		}
		// The same result again: the call is not open any more.
		if code := td.postRPCResult(t, "r1", ev.CallID, `{"ok":true,"data":{}}`); code != http.StatusNotFound {
			t.Errorf("second result status %d", code)
		}
	}, inputFile{"task.json", `{"mode":"rpc"}`})
	if ev := terminal(t, events); ev.Type != "done" {
		t.Fatalf("rpc task: %+v", events)
	}
	if calls != 1 {
		t.Fatalf("want 1 rpc event, got %d", calls)
	}
	if events[0].Type != "accepted" {
		t.Fatalf("first event %+v", events[0])
	}
	out := collect(events, "stdout")
	if !strings.Contains(out, `"ok":true`) || !strings.Contains(out, `"status":200`) {
		t.Fatalf("rpc result in stdout: %q", out)
	}
}

func TestRPCResultErrors(t *testing.T) {
	td := startTestDaemon(t, 1, true)
	if code := td.postRPCResult(t, "nope", "c1", `{"ok":true}`); code != http.StatusNotFound {
		t.Fatalf("unknown task: %d", code)
	}
	if code := td.postRPCResult(t, "r1", "c.1", `{"ok":true}`); code != http.StatusBadRequest {
		t.Fatalf("invalid call id: %d", code)
	}
	if code := td.postRPCResult(t, "r.1", "c1", `{"ok":true}`); code != http.StatusBadRequest {
		t.Fatalf("invalid task id: %d", code)
	}

	events := td.runTaskRPC(t, map[string]any{"taskId": "r2"}, func(ev taskEvent) {
		if code := td.postRPCResult(t, "r2", "other", `{"ok":true}`); code != http.StatusNotFound {
			t.Errorf("unknown call: %d", code)
		}
		if code := td.postRPCResult(t, "r2", ev.CallID, `not json`); code != http.StatusBadRequest {
			t.Errorf("bad body: %d", code)
		}
		if code := td.postRPCResult(t, "r2", ev.CallID, `{"data":1}`); code != http.StatusBadRequest {
			t.Errorf("missing ok: %d", code)
		}
		// The call is still open after the bad requests.
		if code := td.postRPCResult(t, "r2", ev.CallID, `{"ok":false,"error":{"message":"no"}}`); code != http.StatusNoContent {
			t.Errorf("result: %d", code)
		}
	}, inputFile{"task.json", `{"mode":"rpc"}`})
	if ev := terminal(t, events); ev.Type != "done" {
		t.Fatalf("task: %+v", events)
	}
	if out := collect(events, "stdout"); !strings.Contains(out, `"ok":false`) || !strings.Contains(out, `"no"`) {
		t.Fatalf("stdout %q", out)
	}
	if code := td.postRPCResult(t, "r2", "c1", `{"ok":true}`); code != http.StatusNotFound {
		t.Fatalf("after the task: %d", code)
	}
}

func TestRPCInvalidCallID(t *testing.T) {
	td := startTestDaemon(t, 1, true)
	events := td.runTaskRPC(t, map[string]any{"taskId": "b1"}, func(ev taskEvent) {
		t.Errorf("unexpected rpc event %+v", ev)
	}, inputFile{"task.json", `{"mode":"badcallid"}`})
	if ev := terminal(t, events); ev.Type != "done" {
		t.Fatalf("task: %+v", events)
	}
	if out := collect(events, "stdout"); !strings.Contains(out, "invalid rpc callId") {
		t.Fatalf("stdout %q", out)
	}
}

func TestRPCClientGone(t *testing.T) {
	td := startTestDaemon(t, 1, true)
	body, ct := taskBody(t, map[string]any{"taskId": "g1"}, inputFile{"task.json", `{"mode":"rpc"}`})
	resp, err := http.Post(td.base+"/tasks", ct, body)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(resp.Body)
	var callID string
	for callID == "" {
		var ev taskEvent
		if err := dec.Decode(&ev); err != nil {
			t.Fatalf("stream: %v", err)
		}
		if ev.Type == "rpc" {
			callID = ev.CallID
		}
	}
	resp.Body.Close()

	// The slot becomes free, and the call is not open any more.
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, _ := td.d.state()
		if st.BusySlots == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("slot not freed after the client left")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if code := td.postRPCResult(t, "g1", callID, `{"ok":true}`); code != http.StatusNotFound {
		t.Fatalf("result after the client left: %d", code)
	}
}

func TestUTF8Carry(t *testing.T) {
	var c utf8Carry
	s := "aé€😀"
	b := []byte(s)
	var got strings.Builder
	for i := range b {
		got.WriteString(c.push(b[i : i+1]))
	}
	got.WriteString(c.flush())
	if got.String() != s {
		t.Fatalf("got %q", got.String())
	}
}

func TestSafeRelPath(t *testing.T) {
	for _, ok := range []string{"task.json", "bin/0", "a/b/c"} {
		if _, err := safeRelPath(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "/abs", "../x", "a/../b", "a//b", "./a", "a/", "a\\b"} {
		if _, err := safeRelPath(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestRefusesLocalClientsByDefault(t *testing.T) {
	d, err := New(Config{WorkDir: t.TempDir(), Slots: 1}, RuntimeSpec{Command: []string{"/bin/true"}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(d.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/state")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d, want 403", resp.StatusCode)
	}
}

func TestReadLimitedLine(t *testing.T) {
	br := bufio.NewReaderSize(strings.NewReader(strings.Repeat("x", 100)+"\nok\n"), 16)
	if _, err := readLimitedLine(br, 50); err != errLineTooLong {
		t.Fatalf("want errLineTooLong, got %v", err)
	}
	br = bufio.NewReaderSize(strings.NewReader(strings.Repeat("x", 40)+"\n"), 16)
	line, err := readLimitedLine(br, 50)
	if err != nil || len(line) != 41 {
		t.Fatalf("line %d %v", len(line), err)
	}
}

// sharedWorkerScript is a shared worker (reuse mode). It runs each task in the
// background, so tasks run at the same time in one process. The result holds
// the task number and the pid of the process.
//   - "hang": the task does not end, but the worker confirms a cancel.
//   - "block": the worker blocks and cannot confirm a cancel.
//   - "slow": the task takes 1 s.
//   - "rpc": the task makes one RPC call with the task number as params, and
//     writes "rpc ok" if the result data holds the same number.
const sharedWorkerScript = `#!/bin/sh
echo '{"type":"ready"}' >&3
rdir=$(dirname "$0")
n=0
while read -r line <&3; do
  id=$(printf '%s' "$line" | sed -n 's/.*"taskId":"\([^"]*\)".*/\1/p')
  case "$line" in
    *'"type":"cancel"'*)
      printf '{"type":"cancelled","taskId":"%s"}\n' "$id" >&3
      continue ;;
    *'"type":"rpc_result"'*)
      cid=$(printf '%s' "$line" | sed -n 's/.*"callId":"\([^"]*\)".*/\1/p')
      printf '%s' "$line" > "$rdir/rpc-$cid"
      continue ;;
    *'"type":"start"'*) ;;
    *) continue ;;
  esac
  dir=$(printf '%s' "$line" | sed -n 's/.*"dir":"\([^"]*\)".*/\1/p')
  n=$((n+1))
  if grep -q block "$dir/in/task.json"; then sleep 30; fi
  (
    if grep -q hang "$dir/in/task.json"; then sleep 30; fi
    if grep -q slow "$dir/in/task.json"; then sleep 1; fi
    if grep -q rpc "$dir/in/task.json"; then
      printf '{"type":"rpc","taskId":"%s","callId":"r%s","name":"helpers.httpRequest","params":[%s]}\n' "$id" "$n" "$n" >&3
      while [ ! -f "$rdir/rpc-r$n" ]; do sleep 0.05; done
      if grep -q "\"tag\":$n}" "$rdir/rpc-r$n"; then res=ok; else res=bad; fi
      printf '{"type":"output","taskId":"%s","stream":"stdout","text":"rpc %s\\n"}\n' "$id" "$res" >&3
    fi
    printf '{"type":"output","taskId":"%s","stream":"stdout","text":"task %s\\n"}\n' "$id" "$n" >&3
    printf '{"n":%s,"pid":%s}' "$n" "$$" > "$dir/out/result.json"
    printf '{"type":"done","taskId":"%s"}\n' "$id" >&3
  ) &
done
`

func startSharedDaemon(t *testing.T, slots int) *testDaemon {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "worker.sh")
	if err := os.WriteFile(script, []byte(sharedWorkerScript), 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := New(Config{
		WorkDir: filepath.Join(dir, "work"), Runtime: "test", Slots: slots, TaskMemoryMB: 64,
		SpareWorkers: true, ReuseWorkers: true, OutputTTL: time.Minute,
		WorkerEnv: []string{"PATH=" + os.Getenv("PATH")}, AllowLocalClients: true,
	}, RuntimeSpec{Version: "1", Command: []string{"/bin/sh", script}})
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() {
		_ = d.Serve(ctx, lis)
		close(served)
	}()
	t.Cleanup(func() {
		cancel()
		<-served
	})
	td := &testDaemon{d: d, base: "http://" + lis.Addr().String()}
	td.waitHealthy(t)
	return td
}

type sharedResult struct {
	N   int `json:"n"`
	Pid int `json:"pid"`
}

func sharedDone(t *testing.T, events []taskEvent) sharedResult {
	t.Helper()
	ev := terminal(t, events)
	if ev.Type != "done" {
		t.Fatalf("not done: %+v", ev)
	}
	var r sharedResult
	if err := json.Unmarshal(ev.Result, &r); err != nil {
		t.Fatalf("result %s: %v", ev.Result, err)
	}
	return r
}

func TestSharedWorker(t *testing.T) {
	td := startSharedDaemon(t, 2)

	// Tasks one after the other: one process, task numbers 1, 2, 3.
	var pid int
	for i := 1; i <= 3; i++ {
		_, events := td.runTask(t, map[string]any{"taskId": fmt.Sprintf("s%d", i)}, inputFile{"task.json", `{}`})
		r := sharedDone(t, events)
		if r.N != i || terminal(t, events).Timings.WorkerTasks != i {
			t.Fatalf("task %d: %+v", i, r)
		}
		if pid == 0 {
			pid = r.Pid
		} else if r.Pid != pid {
			t.Fatalf("task %d ran in pid %d, want %d", i, r.Pid, pid)
		}
		if got := collect(events, "stdout"); got != fmt.Sprintf("task %d\n", i) {
			t.Fatalf("task %d: stdout %q", i, got)
		}
		// The slot folders are clean again.
		for _, slot := range []string{"0", "1"} {
			entries, err := os.ReadDir(filepath.Join(td.d.slotsDir, slot))
			if err != nil || len(entries) != 0 {
				t.Fatalf("slot %s folder: %v %v", slot, entries, err)
			}
		}
	}

	// Two tasks at the same time run in the same process, each with its own
	// output.
	type out struct {
		r      sharedResult
		stdout string
	}
	results := make(chan out, 2)
	start := time.Now()
	for _, id := range []string{"c1", "c2"} {
		go func() {
			_, events := td.runTask(t, map[string]any{"taskId": id}, inputFile{"task.json", `{"slow":1}`})
			results <- out{sharedDone(t, events), collect(events, "stdout")}
		}()
	}
	a, b := <-results, <-results
	if elapsed := time.Since(start); elapsed > 1800*time.Millisecond {
		t.Fatalf("the tasks did not run at the same time: %s", elapsed)
	}
	if a.r.Pid != pid || b.r.Pid != pid || a.r.N == b.r.N {
		t.Fatalf("concurrent tasks: %+v %+v", a, b)
	}
	if a.stdout != fmt.Sprintf("task %d\n", a.r.N) || b.stdout != fmt.Sprintf("task %d\n", b.r.N) {
		t.Fatalf("output mixed: %q %q", a.stdout, b.stdout)
	}
	if got := td.workersStarted(t); got != 1 {
		t.Fatalf("workersStarted %d, want 1", got)
	}
}

func TestSharedWorkerCancel(t *testing.T) {
	td := startSharedDaemon(t, 2)

	// A timeout of a task that the worker can drop. A task that runs at the
	// same time finishes in the same process. Then the daemon retires the
	// process, because code of the dropped task can still run.
	slow := make(chan sharedResult, 1)
	go func() {
		_, events := td.runTask(t, map[string]any{"taskId": "s1"}, inputFile{"task.json", `{"slow":1}`})
		slow <- sharedDone(t, events)
	}()
	time.Sleep(100 * time.Millisecond)
	_, events := td.runTask(t, map[string]any{"taskId": "h1", "timeoutMs": 200}, inputFile{"task.json", `{"hang":1}`})
	if ev := terminal(t, events); ev.Code != "timeout" {
		t.Fatalf("timeout: %+v", ev)
	}
	sibling := <-slow
	_, events = td.runTask(t, map[string]any{"taskId": "h2"}, inputFile{"task.json", `{}`})
	first := sharedDone(t, events)
	if first.Pid == sibling.Pid {
		t.Fatalf("the tainted process was not retired: pid %d", first.Pid)
	}
	if got := td.workersStarted(t); got != 2 {
		t.Fatalf("after a dropped task: workersStarted %d, want 2", got)
	}

	// A task that blocks the worker: the daemon kills the process. The next
	// task gets a new one.
	_, events = td.runTask(t, map[string]any{"taskId": "b1", "timeoutMs": 200}, inputFile{"task.json", `{"block":1}`})
	if ev := terminal(t, events); ev.Code != "timeout" {
		t.Fatalf("blocked timeout: %+v", ev)
	}
	td.waitHealthy(t)
	_, events = td.runTask(t, map[string]any{"taskId": "b2"}, inputFile{"task.json", `{}`})
	r := sharedDone(t, events)
	if r.Pid == first.Pid || r.N != 1 {
		t.Fatalf("after kill: %+v, old pid %d", r, first.Pid)
	}
	if got := td.workersStarted(t); got != 3 {
		t.Fatalf("after kill: workersStarted %d, want 3", got)
	}
}

func TestSharedWorkerRPC(t *testing.T) {
	td := startSharedDaemon(t, 2)
	type out struct {
		id     string
		calls  []taskEvent
		stdout string
	}
	results := make(chan out, 2)
	// Each task sees only its own call. A result posted under the other
	// task's id does not reach the call.
	ids := []string{"p1", "p2"}
	for i, id := range ids {
		other := ids[1-i]
		go func() {
			var calls []taskEvent
			events := td.runTaskRPC(t, map[string]any{"taskId": id}, func(ev taskEvent) {
				calls = append(calls, ev)
				if code := td.postRPCResult(t, other, ev.CallID, `{"ok":true,"data":{"tag":0}}`); code != http.StatusNotFound {
					t.Errorf("result under another task: %d", code)
				}
				body := fmt.Sprintf(`{"ok":true,"data":{"tag":%s}}`, strings.Trim(string(ev.Params), "[]"))
				if code := td.postRPCResult(t, id, ev.CallID, body); code != http.StatusNoContent {
					t.Errorf("result: %d", code)
				}
			}, inputFile{"task.json", `{"rpc":1,"slow":1}`})
			if ev := terminal(t, events); ev.Type != "done" {
				t.Errorf("task %s: %+v", id, ev)
			}
			results <- out{id, calls, collect(events, "stdout")}
		}()
	}
	for range ids {
		r := <-results
		if len(r.calls) != 1 {
			t.Fatalf("task %s: %d rpc events", r.id, len(r.calls))
		}
		if !strings.Contains(r.stdout, "rpc ok") {
			t.Fatalf("task %s: stdout %q", r.id, r.stdout)
		}
	}
}

func TestSharedWorkerHeapLimit(t *testing.T) {
	d, err := New(Config{WorkDir: t.TempDir(), Slots: 4, TaskMemoryMB: 128, ReuseWorkers: true},
		RuntimeSpec{Command: []string{"node", "--max-old-space-size={taskMemoryMb}"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := d.command[1]; got != "--max-old-space-size=512" {
		t.Fatalf("command %q", got)
	}
}

func (td *testDaemon) workersStarted(t *testing.T) int64 {
	t.Helper()
	resp, err := http.Get(td.base + "/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st stateResponse
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	return st.WorkersStarted
}

// Process churn: a new process per task starts one spare per slot plus one
// replacement per task. Reuse starts one shared process.
func TestWorkersStarted(t *testing.T) {
	td := startTestDaemon(t, 2, true)
	// Wait for the spare of the second slot.
	deadline := time.Now().Add(5 * time.Second)
	for td.workersStarted(t) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	for i := 0; i < 3; i++ {
		_, events := td.runTask(t, map[string]any{"taskId": fmt.Sprintf("w%d", i)}, inputFile{"task.json", `{"mode":"fastexit"}`})
		if ev := terminal(t, events); ev.Type != "done" {
			t.Fatalf("task %d: %+v", i, ev)
		}
	}
	if got := td.workersStarted(t); got != 2+3 {
		t.Errorf("new process per task: workersStarted %d, want 5", got)
	}

	shared := startSharedDaemon(t, 2)
	for i := 0; i < 3; i++ {
		_, events := shared.runTask(t, map[string]any{"taskId": fmt.Sprintf("w%d", i)}, inputFile{"task.json", `{}`})
		sharedDone(t, events)
	}
	if got := shared.workersStarted(t); got != 1 {
		t.Errorf("reuse: workersStarted %d, want 1", got)
	}
}

func TestPendingCallsLimits(t *testing.T) {
	p := newPendingCalls()
	noop := func(rpcResult) {}
	for i := 0; i < maxOpenCalls; i++ {
		if err := p.add(fmt.Sprintf("c%d", i), noop); err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	if err := p.add("one-more", noop); err != errTooMany {
		t.Fatalf("over the limit: %v", err)
	}
	if err := p.add("c0", noop); err != errCallExists {
		t.Fatalf("duplicate: %v", err)
	}
	if p.take("c0") == nil || p.take("c0") != nil {
		t.Fatal("take must return the callback once")
	}
	p.close()
	if p.has("c1") {
		t.Fatal("open call after close")
	}
	if err := p.add("new", noop); err != errCallsDone {
		t.Fatalf("add after close: %v", err)
	}
}

func TestReadCgroupUsage(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "cpu.stat"), []byte("usage_usec 12345\nuser_usec 10000\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "memory.peak"), []byte("2048\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "memory.current"), []byte("1024\n"), 0o600)
	cpu, peak, cur := readCgroupUsage(dir)
	if cpu != 12345 || peak != 2048 || cur != 1024 {
		t.Fatalf("got %d %d %d", cpu, peak, cur)
	}
	if cpu, peak, cur := readCgroupUsage(filepath.Join(dir, "missing")); cpu != 0 || peak != 0 || cur != 0 {
		t.Fatalf("missing dir: %d %d %d", cpu, peak, cur)
	}
}
