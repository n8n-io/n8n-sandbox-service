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

	mu                 sync.Mutex // guards execution registration, delivery, and cleanup
	executionsByTaskID map[string]*execution
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
	w := &worker{
		cmd: cmd, conn: conn, ready: make(chan struct{}), stopped: make(chan struct{}),
		done: make(chan struct{}), executionsByTaskID: make(map[string]*execution),
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
		w.kill()
		_ = cmd.Wait()
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

// send serializes writes so concurrent tasks cannot interleave control messages.
func (w *worker) send(msg workerMessage) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	if err := w.conn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	return json.NewEncoder(w.conn).Encode(msg)
}
