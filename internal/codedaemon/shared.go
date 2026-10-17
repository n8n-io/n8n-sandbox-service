package codedaemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Reuse mode: one shared worker process runs all tasks of the sandbox, at the
// same time. The slots limit how many tasks run at once. The worker sends the
// messages of each task with its task id, and the daemon routes them to the
// task. Console output comes as "output" messages, because the stdout and
// stderr pipes are shared by all tasks.
//
// A timeout or a cancel asks the worker to drop the task ("cancel"). If the
// worker does not confirm in time, for example because a task blocks the event
// loop, the daemon kills the process. The other running tasks then fail, and
// the next task starts a new process. The code of a dropped task can still run
// (timers, promises), so the worker is "tainted": the daemon kills it as soon
// as no task runs in it, and the next task starts a new process.

// sharedCancelGrace is how long the daemon waits for the worker to confirm a
// cancel before it kills the process.
const sharedCancelGrace = time.Second

// sharedOutputLogMax and sharedOutputLogChunks bound the raw output of a
// shared worker that the daemon logs. Task output does not use the pipes.
const (
	sharedOutputLogMax    = 1024
	sharedOutputLogChunks = 20
)

// maxOutputText bounds the text of one "output" message. The worker splits
// longer output. A longer text is cut.
const maxOutputText = 64 * 1024

// sharedTaskQueue is the number of messages that wait for one task.
const sharedTaskQueue = 16

// maxOrphanReplies bounds the error replies in flight to RPC calls of tasks
// that have ended. More such calls get no reply.
const maxOrphanReplies = 8

// Restart backoff for a shared worker that exits before it is ready.
const (
	sharedRestartBase = 100 * time.Millisecond
	sharedRestartMax  = 30 * time.Second
)

type sharedWorker struct {
	w *worker
	// dead is closed when the process has exited and all its messages are
	// routed.
	dead chan struct{}

	// orphanReplies limits the replies to RPC calls of ended tasks.
	orphanReplies chan struct{}

	mu     sync.Mutex
	tasks  map[string]*sharedTask
	served int
	// tainted is set when a dropped task can still run code in the process.
	tainted bool
}

// sharedTask receives the messages of one running task.
type sharedTask struct {
	msgs chan workerMsg
	gone chan struct{} // closed when the task no longer reads msgs
}

// startShared starts the shared worker. The caller holds d.mu.
func (d *Daemon) startShared() (*sharedWorker, error) {
	// Files that an earlier worker left in its folder do not carry over.
	emptyDir(d.sharedDir, d.trashDir)
	w, err := startWorker(d.command, d.cfg.WorkerEnv, d.sharedDir)
	if err != nil {
		return nil, err
	}
	d.workersStarted.Add(1)
	sw := &sharedWorker{
		w:             w,
		dead:          make(chan struct{}),
		tasks:         make(map[string]*sharedTask),
		orphanReplies: make(chan struct{}, maxOrphanReplies),
	}
	go sw.logRawOutput()
	go d.routeShared(sw)
	go func() {
		select {
		case <-w.ready:
			d.firstSpareOnce.Do(func() { d.firstSpareAt.Store(time.Now().UnixNano()) })
		case <-w.exited:
		}
	}()
	return sw, nil
}

// startSharedEarly starts the shared worker when the daemon starts, so the
// first task does not wait for the runtime to load.
func (d *Daemon) startSharedEarly() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.shared != nil {
		return
	}
	sw, err := d.startShared()
	if err != nil {
		slog.Error("start shared worker", "err", err)
		return
	}
	d.shared = sw
}

// acquireShared returns the live shared worker. It starts one if there is
// none, and waits until it is ready. hit is true if it was ready already.
func (d *Daemon) acquireShared(cancel <-chan struct{}) (sw *sharedWorker, hit bool, err error) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil, false, errors.New("daemon is shutting down")
	}
	sw = d.shared
	if sw != nil {
		select {
		case <-sw.dead:
			sw = nil
		case <-sw.w.exited:
			sw = nil
		default:
			// A tainted worker with no task is being killed.
			sw.mu.Lock()
			retiring := sw.tainted && len(sw.tasks) == 0
			sw.mu.Unlock()
			if retiring {
				sw.kill()
				sw = nil
			}
		}
	}
	if sw == nil {
		sw, err = d.startShared()
		if err != nil {
			d.mu.Unlock()
			return nil, false, err
		}
		d.shared = sw
	}
	d.mu.Unlock()

	select {
	case <-sw.w.ready:
		hit = true
	default:
	}
	if err := sw.w.waitReady(workerReadyTimeout, cancel); err != nil {
		return nil, false, err
	}
	return sw, hit, nil
}

// routeShared sends each message of the worker to its task. When the process
// exits, it marks the worker dead and starts a new one if spares are on.
func (d *Daemon) routeShared(sw *sharedWorker) {
	msgs := sw.w.msgs
	exited := sw.w.exited
	var grace <-chan time.Time
loop:
	for {
		select {
		case m, ok := <-msgs:
			if !ok {
				break loop
			}
			sw.route(m)
		case <-exited:
			// A child process that left the process group can keep fd 3
			// open, so the exit, not the end of fd 3, ends the worker. Read
			// what is left for a short time.
			exited = nil
			grace = time.After(exitGraceTimeout)
		case <-grace:
			break loop
		}
	}
	// fd 3 is closed, or the process exited. A process that closed fd 3 but
	// still runs cannot take tasks, so kill it.
	select {
	case <-sw.w.exited:
	case <-time.After(exitGraceTimeout):
		sw.w.kill()
		<-sw.w.exited
	}
	sw.w.release()
	close(sw.dead)
	slog.Warn("shared worker exited", "state", sw.w.exitDescription(), "tasks", sw.servedCount())

	wasReady := sw.w.readyAt.Load() > 0
	d.mu.Lock()
	if d.shared == sw {
		d.shared = nil
	}
	if wasReady {
		d.sharedFailures = 0
	} else {
		d.sharedFailures++
	}
	failures := d.sharedFailures
	d.mu.Unlock()
	if !d.cfg.SpareWorkers {
		return
	}
	if failures == 0 {
		d.startSharedEarly()
		return
	}
	// The worker did not even get ready: do not restart it in a tight loop.
	delay := sharedRestartBase << min(failures-1, 16)
	if delay > sharedRestartMax {
		delay = sharedRestartMax
	}
	time.AfterFunc(delay, d.startSharedEarly)
}

// route gives a message to its task. It waits while the queue of the task is
// full. Output and results of an ended task are dropped. An RPC call of an
// ended task gets an error, if few such replies are in flight.
func (sw *sharedWorker) route(m workerMsg) {
	if m.Type == "output" && len(m.Text) > maxOutputText {
		m.Text = m.Text[:maxOutputText]
	}
	sw.mu.Lock()
	t := sw.tasks[m.TaskID]
	sw.mu.Unlock()
	if t != nil {
		select {
		case t.msgs <- m:
			return
		case <-t.gone:
		}
	}
	if m.Type != "rpc" {
		return
	}
	select {
	case sw.orphanReplies <- struct{}{}:
		go func() {
			defer func() { <-sw.orphanReplies }()
			ok := false
			_ = sw.w.send(workerMsg{Type: "rpc_result", CallID: m.CallID, OK: &ok, Error: errorJSON("the task of this call has ended")})
		}()
	default:
	}
}

// logRawOutput reads the stdout and stderr pipes of the worker. Task output
// goes over the control socket, so this is output of the runtime itself.
func (sw *sharedWorker) logRawOutput() {
	n := 0
	for c := range sw.w.out {
		n++
		if n > sharedOutputLogChunks {
			if n == sharedOutputLogChunks+1 {
				slog.Info("shared worker output: more output is not logged")
			}
			continue
		}
		text := string(c.data)
		if len(text) > sharedOutputLogMax {
			text = text[:sharedOutputLogMax] + "…"
		}
		slog.Info("shared worker output", "stream", c.stream, "text", text)
	}
}

func (sw *sharedWorker) register(taskID string) (*sharedTask, int) {
	t := &sharedTask{msgs: make(chan workerMsg, sharedTaskQueue), gone: make(chan struct{})}
	sw.mu.Lock()
	defer sw.mu.Unlock()
	sw.tasks[taskID] = t
	sw.served++
	return t, sw.served
}

// unregister removes a task. A tainted worker is killed when its last task is
// gone.
func (sw *sharedWorker) unregister(taskID string, t *sharedTask) {
	sw.mu.Lock()
	if sw.tasks[taskID] == t {
		delete(sw.tasks, taskID)
	}
	retire := sw.tainted && len(sw.tasks) == 0
	sw.mu.Unlock()
	close(t.gone)
	if retire {
		slog.Info("retiring shared worker after a dropped task")
		sw.kill()
	}
}

// kill kills the process unless it has exited already, so a new process
// with the same pid is never hit.
func (sw *sharedWorker) kill() {
	select {
	case <-sw.w.exited:
	default:
		sw.w.kill()
	}
}

func (sw *sharedWorker) servedCount() int {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.served
}

// runShared runs the task in the shared worker.
func (d *Daemon) runShared(ctx context.Context, ew *eventWriter, s *slot, spec taskSpec, tm *taskTimings, timeout time.Duration) runResult {
	fail := func(code, msg string) runResult {
		ew.emit(taskEvent{Type: "error", Code: code, Message: msg})
		return runResult{failedEarly: true}
	}
	tHand := time.Now()
	sw, hit, err := d.acquireShared(ctx.Done())
	if err != nil {
		return fail("crashed", "worker: "+err.Error())
	}
	tm.SpareHit = hit
	t, served := sw.register(spec.TaskID)
	defer sw.unregister(spec.TaskID, t)
	tm.WorkerTasks = served

	deadline := time.Now().Add(timeout)
	if err := sw.w.send(workerMsg{Type: "start", TaskID: spec.TaskID, Dir: s.dir, Deadline: deadline.UnixMilli(), Shared: true}); err != nil {
		return fail("crashed", "send start: "+err.Error())
	}
	tm.Handoff = time.Since(tHand).Milliseconds()
	slotIdx := s.idx
	ew.emit(taskEvent{Type: "accepted", TaskID: spec.TaskID, Slot: &slotIdx})

	tRun := time.Now()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var res runResult
	// handle returns true when m ends the task.
	handle := func(m workerMsg) bool {
		switch m.Type {
		case "output":
			if (m.Stream == "stdout" || m.Stream == "stderr") && m.Text != "" {
				ew.emit(taskEvent{Type: m.Stream, Data: m.Text})
			}
		case "rpc":
			d.relayRPC(spec.TaskID, sw.w, m)
		case "done":
			res.outcome = "done"
			tm.Worker = m.Timings
			return true
		case "error":
			res.outcome = "user_error"
			res.userError = m.Error
			tm.Worker = m.Timings
			return true
		}
		return false
	}
loop:
	for {
		select {
		case m := <-t.msgs:
			if handle(m) {
				break loop
			}
		case <-sw.dead:
			// Messages sent before the exit are routed before dead closes.
			for {
				select {
				case m := <-t.msgs:
					if handle(m) {
						break loop
					}
					continue
				default:
				}
				break
			}
			res.outcome = sw.w.exitCode()
			res.message = "shared worker exited: " + sw.w.exitDescription()
			break loop
		case <-timer.C:
			res.outcome = "timeout"
			res.message = fmt.Sprintf("task timed out after %s", timeout)
			d.stopSharedTask(sw, t, spec.TaskID)
			break loop
		case <-ctx.Done():
			res.outcome = "cancelled"
			res.message = context.Cause(ctx).Error()
			d.stopSharedTask(sw, t, spec.TaskID)
			break loop
		}
	}
	tm.Run = time.Since(tRun).Milliseconds()
	if ch := d.currentChannel(); ch != nil {
		ch.dropTask(spec.TaskID)
	}
	return res
}

// stopSharedTask asks the worker to drop a task. If the worker does not
// confirm within sharedCancelGrace, it kills the process.
func (d *Daemon) stopSharedTask(sw *sharedWorker, t *sharedTask, taskID string) {
	if err := sw.w.send(workerMsg{Type: "cancel", TaskID: taskID}); err == nil {
		grace := time.NewTimer(sharedCancelGrace)
		defer grace.Stop()
		for {
			select {
			case m := <-t.msgs:
				switch m.Type {
				case "cancelled":
					// The code of the task can still run. Retire the
					// process when no task runs in it.
					sw.mu.Lock()
					sw.tainted = true
					sw.mu.Unlock()
					return
				case "done", "error":
					return
				}
				continue
			case <-sw.dead:
				return
			case <-grace.C:
			}
			break
		}
	}
	slog.Warn("shared worker did not stop a task; killing it", "task_id", taskID, "running_tasks", sw.runningCount())
	sw.kill()
}

func (sw *sharedWorker) runningCount() int {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return len(sw.tasks)
}

// emptyDir moves the entries of dir to the trash and deletes them in the
// background.
func emptyDir(dir, trashDir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for i, e := range entries {
		src := filepath.Join(dir, e.Name())
		trash := filepath.Join(trashDir, fmt.Sprintf("shared-%d-%d", time.Now().UnixNano(), i))
		if err := os.Rename(src, trash); err == nil {
			go func() { _ = os.RemoveAll(trash) }()
		} else {
			_ = os.RemoveAll(src)
		}
	}
}
