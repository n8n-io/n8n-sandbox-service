// This file manages the shared runtime process and sends it task commands.

package codedaemon

import (
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
	cmd      *exec.Cmd
	conn     net.Conn
	ready    chan struct{} // closed when the runtime reports readiness
	stopped  chan struct{} // closed when shutdown begins; no new executions can register
	done     chan struct{} // closed after the worker process is reaped
	stopOnce sync.Once
	writeMu  sync.Mutex

	mu                 sync.Mutex // guards execution registration, delivery, and cleanup; only worker methods lock it
	executionsByTaskID map[string]*execution
}

func newWorker(cmd *exec.Cmd, conn net.Conn) *worker {
	return &worker{
		cmd: cmd, conn: conn, ready: make(chan struct{}), stopped: make(chan struct{}),
		done: make(chan struct{}), executionsByTaskID: make(map[string]*execution),
	}
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

// register adds a result receiver for the task. It fails if the worker is
// stopping or the task ID is already active.
func (w *worker) register(id string, deadline time.Time) (*execution, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.isStopped() {
		return nil, errWorkerUnavailable
	}
	if _, exists := w.executionsByTaskID[id]; exists {
		return nil, errDuplicateTask
	}
	x := newExecution(w, id, deadline)
	w.executionsByTaskID[id] = x
	return x, nil
}

// unregister removes the task's result receiver without stopping the worker.
func (w *worker) unregister(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.executionsByTaskID, id)
}

// deliver passes a terminal message to the task's receiver, if it is still registered.
func (w *worker) deliver(msg workerMessage) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if x := w.executionsByTaskID[msg.TaskID]; x != nil {
		x.deliver(msg)
	}
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

// send serializes writes so concurrent tasks cannot interleave control messages.
func (w *worker) send(msg workerMessage) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	if err := w.conn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	return json.NewEncoder(w.conn).Encode(msg)
}
