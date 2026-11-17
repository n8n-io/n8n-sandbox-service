// This file manages the shared runtime process and routes task messages over its control socket.

package codedaemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	workerStartupTimeout = 15 * time.Second
	workerDrainTimeout   = time.Second // Maximum time to read pending control messages after stopping the worker.
	// maxControlMessage bounds every control message, including its newline.
	maxControlMessage = 512 << 10 // 512 KiB
)

var (
	errWorkerUnavailable = errors.New("runtime unavailable")
	errBeforeDispatch    = errors.New("runtime unavailable before dispatch")
)

// workerMessage is any message between the daemon and the runtime, in either
// direction, sent as newline-delimited JSON over FD 3. The runtime's stdout and
// stderr are not part of the protocol. Type selects the message:
//   - "ready" (runtime → daemon): the runtime accepts start commands. Sent once after startup.
//   - "start" (daemon → runtime): run the task staged in Dir, with TaskID set.
//   - "done" or "error" (runtime → daemon): the task with TaskID finished; "error" carries Error.
//
// Every message must fit in maxControlMessage. The runtime cuts error details
// to fit. The daemon ignores unknown types, and stops the runtime on invalid
// JSON or a larger message.
type workerMessage struct {
	Type   string          `json:"type"`
	TaskID string          `json:"taskId,omitempty"`
	Dir    string          `json:"dir,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
	// Deadline is the execution deadline in a start message, in Unix milliseconds.
	Deadline int64 `json:"deadline,omitempty"`

	HeapUsedBytes int64  `json:"heapUsedBytes,omitempty"`
	Outcome       string `json:"outcome,omitempty"`
}

type worker struct {
	cmd       *exec.Cmd
	conn      net.Conn
	ready     chan struct{} // closed when the runtime reports readiness
	stopped   chan struct{} // closed when shutdown begins; no new executions can register
	done      chan struct{} // closed after the worker process is reaped
	stopOnce  sync.Once
	writeGate chan struct{} // holds one token while a control command is being written

	mu                 sync.Mutex // guards execution registration, delivery, and cleanup
	executionsByTaskID map[string]*execution
	startedAt          time.Time
	heapLimit          int64
	served             int // protected by mu
	killed             atomic.Bool
	exitCode           string // published by closing done
	retiring           bool   // rejects new tasks while existing ones drain; protected by mu
}

// startWorker launches the runtime in its own process group and gives it the
// control socket on fd 3. It stops the runtime if it is not ready within
// startupTimeout, whether or not a caller is still waiting.
func startWorker(cfg Config, startupTimeout time.Duration) (*worker, error) {
	if len(cfg.RuntimeCommand) == 0 {
		return nil, errWorkerUnavailable
	}
	conn, child, err := newControlSocket()
	if err != nil {
		return nil, err
	}
	defer child.Close()
	cmd := exec.Command(cfg.RuntimeCommand[0], cfg.RuntimeCommand[1:]...)
	cmd.Dir = cfg.WorkDir
	cmd.Env = []string{"PATH=" + envOr("PATH", "/usr/bin:/bin"), "HOME=" + envOr("HOME", "/home/node"), "NODE_ENV=production"}
	cmd.ExtraFiles = []*os.File{child}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Don't wait indefinitely for inherited log pipes to close after exit.
	cmd.WaitDelay = time.Second
	logs := newRuntimeLogger(cfg.RuntimeLogWrites)
	cmd.Stdout = logs.writer("stdout")
	stderr := logs.writer("stderr")
	cmd.Stderr = stderr
	initialOOM := oomKillCount(cfg.CgroupDir)
	if err := cmd.Start(); err != nil {
		logs.close()
		_ = conn.Close()
		return nil, err
	}
	w := &worker{
		cmd: cmd, conn: conn, ready: make(chan struct{}), stopped: make(chan struct{}),
		done: make(chan struct{}), startedAt: time.Now(),
		writeGate: make(chan struct{}, 1), executionsByTaskID: make(map[string]*execution),
		heapLimit: int64(cfg.RecycleHeapBytes),
	}
	controlDone := make(chan struct{})
	go func() {
		defer close(controlDone)
		w.readControl()
	}()
	go func() {
		timer := time.NewTimer(startupTimeout)
		defer timer.Stop()
		select {
		case <-w.ready:
		case <-w.stopped:
		case <-timer.C:
			w.kill()
		}
	}()
	go func() {
		waitWorkerExit(cmd.Process.Pid)
		// The unreaped leader reserves the group ID. Any other caller of kill
		// either signals it first or waits for this call via stopOnce.
		w.stop(false)
		_ = cmd.Wait()
		w.exitCode = "crashed"
		if state := cmd.ProcessState; state != nil && !state.Success() {
			status, _ := state.Sys().(syscall.WaitStatus)
			if stderr.oom || (status.Signal() == syscall.SIGKILL && !w.killed.Load() && initialOOM >= 0 && oomKillCount(cfg.CgroupDir) > initialOOM) {
				w.exitCode = "oom"
			}
		}
		logs.close()
		<-controlDone
		_ = conn.Close()
		close(w.done)
		// Close result channels after the control reader has queued any final
		// messages so the handler can receive them before detecting the exit.
		w.mu.Lock()
		for _, task := range w.executionsByTaskID {
			close(task.result)
		}
		clear(w.executionsByTaskID)
		w.mu.Unlock()
	}()
	return w, nil
}

// newControlSocket creates a connected Unix socket pair for the control
// protocol. It returns the daemon's end as a connection and the runtime's end
// as a file to pass to the child process. The caller closes the file once the
// child has started.
func newControlSocket() (net.Conn, *os.File, error) {
	// Holding ForkLock until both ends are close-on-exec keeps them from
	// leaking into processes forked concurrently.
	syscall.ForkLock.RLock()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fds[0])
		syscall.CloseOnExec(fds[1])
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, nil, err
	}
	parent := os.NewFile(uintptr(fds[0]), "daemon-control")
	child := os.NewFile(uintptr(fds[1]), "runtime-control")
	conn, err := net.FileConn(parent)
	_ = parent.Close()
	if err != nil {
		_ = child.Close()
		return nil, nil, err
	}
	return conn, child, nil
}

// readControl receives readiness and task results until the control connection fails or closes.
func (w *worker) readControl() {
	scanner := bufio.NewScanner(w.conn)
	scanner.Buffer(make([]byte, 4096), maxControlMessage)
	ready := false
	var stopErr error
	for scanner.Scan() {
		var msg workerMessage
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			stopErr = fmt.Errorf("invalid control message: %w", err)
			break
		}
		switch msg.Type {
		case "ready":
			if !ready {
				ready = true
				close(w.ready)
			}
		case "cancelled":
			w.mu.Lock()
			if task := w.executionsByTaskID[msg.TaskID]; task != nil {
				select {
				case task.cancelAck <- struct{}{}:
				default:
				}
			}
			w.mu.Unlock()
		case "heap", "done", "error", "failed":
			w.mu.Lock()
			if msg.HeapUsedBytes > w.heapLimit {
				w.retiring = true
			}
			if task := w.executionsByTaskID[msg.TaskID]; task != nil && msg.Type != "heap" {
				select {
				case task.result <- msg:
				default:
				}
			}
			retireIdle := w.retiring && len(w.executionsByTaskID) == 0
			w.mu.Unlock()
			if retireIdle {
				w.kill()
			}
		}
	}
	if err := scanner.Err(); errors.Is(err, bufio.ErrTooLong) {
		stopErr = fmt.Errorf("control message over %d bytes", maxControlMessage)
	} else if err != nil {
		stopErr = err
	}
	// A read error after kill comes from closing the connection ourselves.
	if stopErr != nil && !w.isStopped() {
		slog.Warn("stopping code runtime", "error", stopErr)
	}
	w.stop(false)
}

// kill releases pending result waiters and signals the runtime's process group once.
func (w *worker) kill() {
	w.stop(true)
}

func (w *worker) stop(intentional bool) {
	w.stopOnce.Do(func() {
		w.killed.Store(intentional)
		w.mu.Lock()
		close(w.stopped)
		w.mu.Unlock()

		_ = syscall.Kill(-w.cmd.Process.Pid, syscall.SIGKILL)
		if conn, ok := w.conn.(*net.UnixConn); ok {
			_ = conn.CloseWrite()
		}
		// Preserve buffered terminal messages, but bound the drain even if a
		// descendant outside the group still holds a copy of the socket.
		_ = w.conn.SetReadDeadline(time.Now().Add(workerDrainTimeout))
	})
}

func (w *worker) isStopped() bool {
	select {
	case <-w.stopped:
		return true
	default:
		return false
	}
}

// send serializes control writes. The deadline covers both waiting and writing.
func (w *worker) send(msg workerMessage, deadline time.Time) error {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case w.writeGate <- struct{}{}:
		defer func() { <-w.writeGate }()
	case <-timer.C:
		return context.DeadlineExceeded
	case <-w.done:
		return errWorkerUnavailable
	}
	if err := w.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	return json.NewEncoder(w.conn).Encode(msg)
}

// getWorker starts the shared runtime on demand and waits until it is ready to accept commands.
func (d *Daemon) getWorker(ctx context.Context) (*worker, error) {
	d.workerMu.Lock()
	for d.worker != nil && d.worker.isStopped() && !d.closed {
		select {
		case <-d.worker.done:
			// Replace the exited worker on this dispatch, not during shutdown.
			if !d.worker.wasReady() && time.Since(d.worker.startedAt) < time.Second {
				d.workerMu.Unlock()
				return nil, errWorkerUnavailable
			}
			d.worker = nil
			continue
		default:
		}
		// A stopped worker is reaped soon. Wait for it, then start a new one.
		stopped := d.worker
		d.workerMu.Unlock()
		select {
		case <-stopped.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		d.workerMu.Lock()
	}
	if d.closed || d.startFailed {
		d.workerMu.Unlock()
		return nil, errWorkerUnavailable
	}
	if d.worker == nil {
		w, err := startWorker(d.cfg, d.startupTimeout)
		if err != nil {
			// A failed start is not retried.
			d.startFailed = true
			d.workerMu.Unlock()
			return nil, fmt.Errorf("start runtime: %w", err)
		}
		d.worker = w
	}
	w := d.worker
	d.workerMu.Unlock()
	select {
	case <-w.stopped:
		return nil, errWorkerUnavailable
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-w.ready:
		if w.isStopped() {
			return nil, errWorkerUnavailable
		}
		return w, nil
	}
}

func (w *worker) wasReady() bool {
	select {
	case <-w.ready:
		return true
	default:
		return false
	}
}

// Close prevents worker startup and waits for any existing worker to exit and be reaped.
func (d *Daemon) Close() {
	d.workerMu.Lock()
	d.closed = true
	w := d.worker
	d.workerMu.Unlock()
	if w != nil {
		w.kill()
		<-w.done
	}
}

// execution is one task dispatched to the shared runtime. It receives the
// task's "done" or "error" message until close unregisters it.
type execution struct {
	worker          *worker
	id              string
	result          chan workerMessage // closed on worker failure; any buffered result is still readable
	deadline        time.Time          // Set at dispatch, after the worker is ready.
	cancelRequested chan struct{}      // closed when an explicit cancel is requested
	cancelAck       chan struct{}      // buffers the runtime's acknowledgement without blocking the reader
}

// dispatch registers a result receiver before sending the start command to the shared runtime.
func (d *Daemon) dispatch(ctx context.Context, id, dir string, timeout time.Duration) (*execution, error) {
	w, err := d.getWorker(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errBeforeDispatch, err)
	}
	x := &execution{
		worker: w, id: id, result: make(chan workerMessage, 1), deadline: time.Now().Add(timeout),
		cancelRequested: make(chan struct{}), cancelAck: make(chan struct{}, 1),
	}
	w.mu.Lock()
	if err := ctx.Err(); err != nil {
		w.mu.Unlock()
		return nil, fmt.Errorf("%w: %w", errBeforeDispatch, err)
	}
	if w.isStopped() || w.retiring {
		w.mu.Unlock()
		return nil, errBeforeDispatch
	}
	if _, exists := w.executionsByTaskID[id]; exists {
		w.mu.Unlock()
		return nil, errDuplicateTask
	}
	w.executionsByTaskID[id] = x
	w.served++
	w.retiring = w.served >= d.cfg.RecycleAfterTasks
	w.mu.Unlock()
	d.markDispatched(id)
	if err := w.send(workerMessage{Type: "start", TaskID: id, Dir: dir, Deadline: x.deadline.UnixMilli()}, time.Now().Add(time.Second)); err != nil {
		w.kill() // a partial command leaves the shared protocol unusable
		d.releaseExecution(x)
		return nil, err
	}
	return x, nil
}

// releaseExecution removes a task from the worker and reaps a retired runtime
// after its last task leaves, so a later dispatch can start a replacement.
func (d *Daemon) releaseExecution(x *execution) {
	w := x.worker
	w.mu.Lock()
	delete(w.executionsByTaskID, x.id)
	drained := w.retiring && len(w.executionsByTaskID) == 0
	w.mu.Unlock()
	if !drained {
		return
	}

	w.kill()
	<-w.done
	d.workerMu.Lock()
	if d.worker == w {
		d.worker = nil
	}
	d.workerMu.Unlock()
}
