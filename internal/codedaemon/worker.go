// This file manages the shared runtime process and sends it task commands.

package codedaemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const (
	workerStartupTimeout = 15 * time.Second
	workerDrainTimeout   = time.Second // Maximum time to read pending control messages after stopping the worker.
)

var errWorkerUnavailable = errors.New("runtime unavailable")

type worker struct {
	cmd       *exec.Cmd
	conn      net.Conn
	ready     chan struct{} // closed when the runtime reports readiness
	stopped   chan struct{} // closed when shutdown begins; no new executions can register
	done      chan struct{} // closed after the worker process is reaped
	stopOnce  sync.Once
	writeGate chan struct{} // holds one token while a control command is being written

	mu                 sync.Mutex // guards execution registration, delivery, and cleanup; only worker methods lock it
	executionsByTaskID map[string]*execution
	retiring           bool // rejects new tasks while existing ones drain; protected by mu
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
	cmd := exec.Command(cfg.RuntimeCommand[0], cfg.RuntimeCommand[1:]...) //nolint:gosec // The image's runtime registry sets the command, not the task.
	cmd.Dir = cfg.WorkDir
	cmd.Env = []string{"PATH=" + envOr("PATH", "/usr/bin:/bin"), "HOME=" + envOr("HOME", "/home/node"), "NODE_ENV=production"}
	cmd.ExtraFiles = []*os.File{child}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Don't wait indefinitely for inherited log pipes to close after exit.
	cmd.WaitDelay = time.Second
	logs := newRuntimeLogger(cfg.RuntimeLogWrites)
	cmd.Stdout = logs.writer("stdout")
	cmd.Stderr = logs.writer("stderr")
	if err := cmd.Start(); err != nil {
		logs.close()
		_ = conn.Close()
		return nil, err
	}
	w := newWorker(cmd, conn)
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
		w.kill()
		_ = cmd.Wait()
		logs.close()
		<-controlDone
		_ = conn.Close()
		close(w.done)
		// Close result channels after the control reader has queued any final
		// messages so the handler can receive them before detecting the exit.
		w.releaseExecutions()
	}()
	return w, nil
}

func newWorker(cmd *exec.Cmd, conn net.Conn) *worker {
	return &worker{
		cmd:                cmd,
		conn:               conn,
		ready:              make(chan struct{}),
		stopped:            make(chan struct{}),
		done:               make(chan struct{}),
		writeGate:          make(chan struct{}, 1),
		executionsByTaskID: make(map[string]*execution),
	}
}

// kill releases pending result waiters and signals the runtime's process group once.
func (w *worker) kill() {
	w.stopOnce.Do(func() {
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

// acceptsTasks reports whether new tasks can still join the worker.
func (w *worker) acceptsTasks() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return !w.retiring && !w.isStopped()
}

// retire stops new tasks from joining. The worker stops when its last task
// unregisters.
func (w *worker) retire() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.retiring = true
}

// register adds a result receiver for the task. It fails if the worker is
// stopping or retiring, or the task ID is already active.
func (w *worker) register(id string, deadline time.Time) (*execution, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.isStopped() || w.retiring {
		return nil, errWorkerUnavailable
	}
	if _, exists := w.executionsByTaskID[id]; exists {
		return nil, errDuplicateTask
	}
	x := newExecution(w, id, deadline)
	w.executionsByTaskID[id] = x
	return x, nil
}

// unregister removes the task's result receiver. It stops a retired worker
// after its last task leaves.
func (w *worker) unregister(id string) {
	w.mu.Lock()
	delete(w.executionsByTaskID, id)
	drained := w.retiring && len(w.executionsByTaskID) == 0
	w.mu.Unlock()
	if drained {
		w.kill()
	}
}

// deliver passes a terminal message to the task's receiver, if it is still registered.
func (w *worker) deliver(msg workerMessage) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if x := w.executionsByTaskID[msg.TaskID]; x != nil {
		x.deliver(msg)
	}
}

// acknowledgeCancel passes the runtime's cancel reply to the task's receiver,
// if it is still registered.
func (w *worker) acknowledgeCancel(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if x := w.executionsByTaskID[id]; x != nil {
		x.acknowledgeCancel()
	}
}

// requestCancel signals a running task once and retires the worker before
// another task can join it. It returns false if the task is not registered or
// has already finished. A finished task needs no cancel, and retiring its
// worker would only stop a healthy runtime.
func (w *worker) requestCancel(id string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	x := w.executionsByTaskID[id]
	if x == nil || x.finished {
		return false
	}
	if x.requestCancel() {
		w.retiring = true
	}
	return true
}

// releaseExecutions ends all registered executions after the worker exits.
func (w *worker) releaseExecutions() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, x := range w.executionsByTaskID {
		x.workerExited()
	}
	clear(w.executionsByTaskID)
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
