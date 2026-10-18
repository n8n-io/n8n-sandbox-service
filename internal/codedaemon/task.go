package codedaemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// taskSpec is the first part of a task request.
type taskSpec struct {
	TaskID               string `json:"taskId"`
	TimeoutMs            int64  `json:"timeoutMs"`
	InlineResultMaxBytes int64  `json:"inlineResultMaxBytes"`
}

const (
	maxTimeout          = 24 * time.Hour
	defaultTimeout      = 5 * time.Minute
	defaultInlineResult = 1 << 20
	maxSpecBytes        = 64 * 1024
	drainTimeout        = 200 * time.Millisecond
	exitGraceTimeout    = time.Second
	resultFile          = "result.json"
)

type outputEntry struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// taskTimings are the daemon steps of one task, in milliseconds.
type taskTimings struct {
	SlotWait      int64 `json:"slotWait"`
	InputWrite    int64 `json:"inputWrite"`
	Handoff       int64 `json:"handoff"`
	Run           int64 `json:"run"`
	OutputMove    int64 `json:"outputMove"`
	SpareHit      bool  `json:"spareHit"`
	InputBytes    int64 `json:"inputBytes"`
	OutputBytes   int64 `json:"outputBytes"`
	LeftoverFiles int   `json:"leftoverFiles"`
	// WorkerTasks is the number of tasks the worker has served, this one
	// included. More than 1 only in reuse mode (the shared worker).
	WorkerTasks int             `json:"workerTasks"`
	Worker      json.RawMessage `json:"worker,omitempty"`
}

type taskEvent struct {
	Type      string          `json:"type"`
	TaskID    string          `json:"taskId,omitempty"`
	Slot      *int            `json:"slot,omitempty"`
	Data      string          `json:"data,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Outputs   []outputEntry   `json:"outputs,omitempty"`
	Timings   *taskTimings    `json:"timings,omitempty"`
	Code      string          `json:"code,omitempty"`
	Message   string          `json:"message,omitempty"`
	UserError json.RawMessage `json:"userError,omitempty"`
	// RPC call fields (type "rpc").
	CallID string          `json:"callId,omitempty"`
	Name   string          `json:"name,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

// eventWriter writes NDJSON events and flushes after each one.
type eventWriter struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	rc      *http.ResponseController
	started bool
	broken  bool
}

func (e *eventWriter) start() {
	if e.started {
		return
	}
	e.started = true
	e.w.Header().Set("Content-Type", "application/x-ndjson")
	e.w.Header().Set("Cache-Control", "no-store")
	e.w.WriteHeader(http.StatusOK)
}

// isBroken reports whether a write to the stream failed.
func (e *eventWriter) isBroken() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.broken
}

// emit writes one event. It returns false if the stream is broken.
func (e *eventWriter) emit(ev taskEvent) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.broken {
		return false
	}
	e.start()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// Output text with many "<" or "&" must not grow 6 times.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(ev); err != nil {
		return false
	}
	b := buf.Bytes()
	if _, err := e.w.Write(b); err != nil {
		e.broken = true
		return false
	}
	if err := e.rc.Flush(); err != nil {
		e.broken = true
		return false
	}
	return true
}

// utf8Carry holds the incomplete UTF-8 sequence at the end of a chunk, so a
// multi-byte character is never split over two events.
type utf8Carry struct{ rest []byte }

func (c *utf8Carry) push(b []byte) string {
	buf := append(c.rest, b...)
	cut := len(buf)
	// Look back at most 3 bytes for the start of an incomplete rune.
	for i := len(buf) - 1; i >= 0 && i >= len(buf)-3; i-- {
		if utf8.RuneStart(buf[i]) {
			if !utf8.FullRune(buf[i:]) {
				cut = i
			}
			break
		}
	}
	c.rest = append([]byte(nil), buf[cut:]...)
	return string(buf[:cut])
}

func (c *utf8Carry) flush() string {
	s := string(c.rest)
	c.rest = nil
	return s
}

// handleTask runs one task. Request: multipart with a spec part and input parts.
// Response: an NDJSON event stream with exactly one terminal event.
func (d *Daemon) handleTask(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "expected multipart/form-data: "+err.Error())
		return
	}
	part, err := mr.NextPart()
	if err != nil || part.FormName() != "spec" {
		writeError(w, http.StatusBadRequest, "the first part must be \"spec\"")
		return
	}
	var spec taskSpec
	if err := json.NewDecoder(io.LimitReader(part, maxSpecBytes)).Decode(&spec); err != nil {
		writeError(w, http.StatusBadRequest, "invalid spec: "+err.Error())
		return
	}
	if !taskIDRegex.MatchString(spec.TaskID) {
		writeError(w, http.StatusBadRequest, "invalid taskId")
		return
	}
	timeout := time.Duration(spec.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if timeout > maxTimeout {
		timeout = maxTimeout
	}
	if spec.InlineResultMaxBytes <= 0 {
		spec.InlineResultMaxBytes = defaultInlineResult
	}

	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)

	// 429 before the inputs are read: the caller owns slot accounting, so this
	// only happens when it is wrong.
	rt, err := d.acquireSlot(spec.TaskID, cancel)
	if err != nil {
		code := http.StatusServiceUnavailable
		if errors.Is(err, errTaskExists) {
			code = http.StatusConflict
		}
		writeError(w, code, err.Error())
		return
	}
	if rt == nil {
		writeError(w, http.StatusTooManyRequests, "no free slot")
		return
	}
	s := rt.slot
	// No RPC result is accepted after the task ends.
	defer rt.rpc.close()
	tm := &taskTimings{SlotWait: 0}
	outputsStored := false
	defer func() {
		if s != nil {
			d.releaseSlot(s, spec.TaskID, outputsStored)
		}
	}()

	ew := &eventWriter{w: w, rc: http.NewResponseController(w)}
	fail := func(code, msg string) {
		ew.emit(taskEvent{Type: "error", Code: code, Message: msg})
	}

	// Inputs.
	inDir := filepath.Join(s.dir, "in")
	outDir := filepath.Join(s.dir, "out")
	for _, dir := range []string{inDir, filepath.Join(outDir, "bin")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			fail("crashed", "prepare slot: "+err.Error())
			return
		}
	}
	tIn := time.Now()
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			fail("invalid", "read inputs: "+err.Error())
			return
		}
		if part.FormName() != "input" {
			fail("invalid", fmt.Sprintf("unexpected part %q", part.FormName()))
			return
		}
		rel, err := safeRelPath(rawFileName(part.Header.Get("Content-Disposition")))
		if err != nil {
			fail("invalid", "input filename: "+err.Error())
			return
		}
		n, err := writeInput(inDir, rel, part)
		tm.InputBytes += n
		if err != nil {
			fail("invalid", "write input "+rel+": "+err.Error())
			return
		}
	}
	tm.InputWrite = time.Since(tIn).Milliseconds()

	// Run.
	var res runResult
	if d.cfg.ReuseWorkers {
		res = d.runShared(ctx, ew, rt, spec, tm, timeout)
	} else {
		res = d.runOwnWorker(ctx, ew, rt, spec, tm, timeout)
	}
	if res.failedEarly {
		return
	}
	outcome, message, userError := res.outcome, res.message, res.userError
	tm.LeftoverFiles = countLeftovers(t0)

	if outcome != "done" {
		releasedSlot := s
		s = nil
		d.releaseSlot(releasedSlot, spec.TaskID, false)
		ev := taskEvent{Type: "error", Code: outcome, Message: message, UserError: userError, Timings: tm}
		if outcome == "user_error" {
			ev.Message = "user code threw an error"
		}
		ew.emit(ev)
		logTask(spec.TaskID, outcome, t0)
		return
	}

	// Outputs.
	tOut := time.Now()
	dest := filepath.Join(d.outputsDir, spec.TaskID)
	if err := os.Rename(outDir, dest); err != nil {
		releasedSlot := s
		s = nil
		d.releaseSlot(releasedSlot, spec.TaskID, false)
		fail("crashed", "move outputs: "+err.Error())
		return
	}
	outputs, total := listOutputs(dest)
	tm.OutputBytes = total
	var inline json.RawMessage
	for _, o := range outputs {
		if o.Path == resultFile && o.Size <= spec.InlineResultMaxBytes {
			if b, err := readRegularFile(dest, resultFile, spec.InlineResultMaxBytes); err == nil && json.Valid(b) {
				inline = b
			}
		}
	}
	tm.OutputMove = time.Since(tOut).Milliseconds()

	// Free the slot before the terminal event, so the next task that the
	// caller sends on receipt finds it free.
	outputsStored = true
	releasedSlot := s
	s = nil
	d.releaseSlot(releasedSlot, spec.TaskID, true)

	ev := taskEvent{Type: "done", Outputs: outputs, Timings: tm}
	if inline != nil {
		ev.Result = inline
	} else {
		ev.Result = json.RawMessage("null")
	}
	ew.emit(ev)
	logTask(spec.TaskID, outcome, t0)
}

func logTask(taskID, outcome string, t0 time.Time) {
	slog.Info("task finished", "task_id", taskID, "outcome", outcome, "duration_ms", time.Since(t0).Milliseconds())
}

// runResult is how the worker part of a task ended.
type runResult struct {
	outcome   string // done, user_error, timeout, cancelled, oom or crashed
	message   string
	userError json.RawMessage
	// failedEarly: the task failed before it ran, and the error event is
	// already sent.
	failedEarly bool
}

// runOwnWorker runs the task in a worker process of its own: the spare of the
// slot, or a new one. The process is killed after the task.
func (d *Daemon) runOwnWorker(ctx context.Context, ew *eventWriter, rt *runningTask, spec taskSpec, tm *taskTimings, timeout time.Duration) runResult {
	s := rt.slot
	fail := func(code, msg string) runResult {
		ew.emit(taskEvent{Type: "error", Code: code, Message: msg})
		return runResult{failedEarly: true}
	}
	tHand := time.Now()
	wk, spareHit, err := d.takeWorker(s)
	if err != nil {
		return fail("crashed", "start worker: "+err.Error())
	}
	tm.SpareHit = spareHit
	wk.tasks++
	tm.WorkerTasks = wk.tasks
	defer func() {
		wk.kill()
		wk.release()
	}()
	if err := wk.waitReady(workerReadyTimeout, ctx.Done()); err != nil {
		return fail("crashed", "worker: "+err.Error())
	}
	deadline := time.Now().Add(timeout)
	if err := wk.send(workerMsg{Type: "start", TaskID: spec.TaskID, Dir: s.dir, Deadline: deadline.UnixMilli()}); err != nil {
		return fail("crashed", "send start: "+err.Error())
	}
	tm.Handoff = time.Since(tHand).Milliseconds()
	slotIdx := s.idx
	ew.emit(taskEvent{Type: "accepted", TaskID: spec.TaskID, Slot: &slotIdx})

	tRun := time.Now()
	carry := map[string]*utf8Carry{"stdout": {}, "stderr": {}}
	emitChunk := func(c outChunk) {
		if txt := carry[c.stream].push(c.data); txt != "" {
			ew.emit(taskEvent{Type: c.stream, Data: txt})
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var res runResult
	msgs := wk.msgs
	out := wk.out
	exited := wk.exited
	// After the exit, the control socket can still hold the last message
	// ("done" or "error"). Read it before the exit counts as a crash.
	var exitGrace <-chan time.Time
	crashed := func() {
		res.outcome = wk.exitCode()
		res.message = "worker exited: " + wk.exitDescription()
	}
loop:
	for {
		select {
		case c, ok := <-out:
			if !ok {
				out = nil
				continue
			}
			emitChunk(c)
		case m, ok := <-msgs:
			if !ok {
				msgs = nil
				if exited == nil {
					// Exited, and no more control messages.
					crashed()
					break loop
				}
				continue
			}
			switch m.Type {
			case "rpc":
				d.relayRPC(rt, ew, spec.TaskID, wk, m)
			case "done":
				res.outcome = "done"
				tm.Worker = m.Timings
				break loop
			case "error":
				res.outcome = "user_error"
				res.userError = m.Error
				tm.Worker = m.Timings
				break loop
			}
		case <-exited:
			exited = nil
			if msgs == nil {
				crashed()
				break loop
			}
			exitGrace = time.After(exitGraceTimeout)
		case <-exitGrace:
			crashed()
			break loop
		case <-timer.C:
			res.outcome = "timeout"
			res.message = fmt.Sprintf("task timed out after %s", timeout)
			break loop
		case <-ctx.Done():
			res.outcome = "cancelled"
			res.message = context.Cause(ctx).Error()
			break loop
		}
	}
	tm.Run = time.Since(tRun).Milliseconds()

	rt.rpc.close()
	// Kill the worker and its process group, then read what is left in the
	// pipes.
	wk.kill()
	wk.drainOutput(drainTimeout, emitChunk)
	for _, stream := range []string{"stdout", "stderr"} {
		if txt := carry[stream].flush(); txt != "" {
			ew.emit(taskEvent{Type: stream, Data: txt})
		}
	}
	return res
}

// relayRPC sends an RPC call of the worker to the client as an "rpc" event in
// the task stream. The client answers with POST /tasks/{taskId}/rpc/{callId}.
// The task of the stream is the scope of the call; the worker cannot select
// another task.
func (d *Daemon) relayRPC(rt *runningTask, ew *eventWriter, taskID string, wk *worker, m workerMsg) {
	reply := func(res rpcResult) {
		ok := res.OK
		_ = wk.send(workerMsg{Type: "rpc_result", CallID: m.CallID, OK: &ok, Data: res.Data, Error: res.Error})
	}
	if m.CallID == "" || m.Name == "" {
		reply(rpcResult{OK: false, Error: errorJSON("invalid rpc message")})
		return
	}
	if !callIDRegex.MatchString(m.CallID) {
		reply(rpcResult{OK: false, Error: errorJSON("invalid rpc callId")})
		return
	}
	// A worker can still run code of an earlier task (a timer, for example).
	// Its helpers send that task's id. Do not run such a call in the name of
	// another task.
	if m.TaskID != "" && m.TaskID != taskID {
		reply(rpcResult{OK: false, Error: errorJSON("the task of this call has ended")})
		return
	}
	if ew.isBroken() {
		reply(rpcResult{OK: false, Error: errorJSON("the client of the task is gone")})
		return
	}
	params := m.Params
	if len(params) == 0 {
		params = json.RawMessage("[]")
	}
	// Register before the event: the client can answer before emit returns.
	if err := rt.rpc.add(m.CallID, reply); err != nil {
		reply(rpcResult{OK: false, Error: errorJSON(err.Error())})
		return
	}
	if !ew.emit(taskEvent{Type: "rpc", CallID: m.CallID, Name: m.Name, Params: params}) {
		if cb := rt.rpc.take(m.CallID); cb != nil {
			cb(rpcResult{OK: false, Error: errorJSON("the client of the task is gone")})
		}
	}
}

// rawFileName returns the filename parameter of a Content-Disposition header.
// multipart.Part.FileName applies filepath.Base, which drops folders.
func rawFileName(cd string) string {
	_, params, err := mime.ParseMediaType(cd)
	if err != nil {
		return ""
	}
	return params["filename"]
}

// safeRelPath accepts a relative slash path with no "..", "." or empty
// elements.
func safeRelPath(p string) (string, error) {
	if p == "" {
		return "", errors.New("empty path")
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.ContainsRune(p, 0) {
		return "", errors.New("path must be relative")
	}
	if path.Clean(p) != p {
		return "", errors.New("path must be clean")
	}
	for _, el := range strings.Split(p, "/") {
		if el == "" || el == "." || el == ".." {
			return "", errors.New("invalid path element")
		}
	}
	return p, nil
}

func writeInput(inDir, rel string, r io.Reader) (int64, error) {
	root, err := os.OpenRoot(inDir)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	if dir := path.Dir(rel); dir != "." {
		if err := root.MkdirAll(dir, 0o700); err != nil {
			return 0, err
		}
	}
	f, err := root.OpenFile(rel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	buf := make([]byte, 256*1024)
	n, err := copyBuffer(f, r, buf)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return n, err
}

func copyBuffer(dst io.Writer, src io.Reader, buf []byte) (int64, error) {
	// Hide ReaderFrom/WriterTo so the given buffer size is used.
	return io.CopyBuffer(struct{ io.Writer }{dst}, struct{ io.Reader }{src}, buf)
}

func listOutputs(dir string) ([]outputEntry, int64) {
	var out []outputEntry
	var total int64
	_ = filepath.WalkDir(dir, func(p string, de fs.DirEntry, err error) error {
		if err != nil || de.IsDir() {
			return nil
		}
		if !de.Type().IsRegular() {
			return nil
		}
		fi, err := de.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return nil
		}
		out = append(out, outputEntry{Path: filepath.ToSlash(rel), Size: fi.Size()})
		total += fi.Size()
		return nil
	})
	return out, total
}

// countLeftovers counts entries in /tmp that changed after the task started.
// It shows state that a task left for later tasks in the same sandbox.
func countLeftovers(since time.Time) int {
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && fi.ModTime().After(since) {
			n++
		}
	}
	return n
}

// openRegular opens a regular file under dir. It does not follow a symlink at
// the last element and does not block on a FIFO, because a process that a task
// left behind can swap files in its outputs.
func openRegular(dir, rel string) (*os.File, os.FileInfo, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		f.Close()
		return nil, nil, errors.New("not a regular file")
	}
	return f, fi, nil
}

func readRegularFile(dir, rel string, max int64) ([]byte, error) {
	f, _, err := openRegular(dir, rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, max))
}
