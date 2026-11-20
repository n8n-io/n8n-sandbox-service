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
	"syscall"
	"time"
)

const (
	workerStartupTimeout = 15 * time.Second
	// maxControlMessage bounds every control message, including its newline.
	maxControlMessage = 512 << 10 // 512 KiB
)

var errWorkerUnavailable = errors.New("runtime unavailable")

// workerMessage is any message between the daemon and the runtime, in either
// direction, sent as newline-delimited JSON over FD 3. The runtime's stdout and
// stderr are not part of the protocol. Type selects the message:
//   - "ready" (runtime → daemon): the runtime accepts start commands. Sent once after startup.
//   - "start" (daemon → runtime): run the task staged in Dir, with TaskID and Shared set.
//   - "done" or "error" (runtime → daemon): the task with TaskID finished; "error" carries Error.
//
// Every message must fit in maxControlMessage. The runtime cuts error details
// to fit. The daemon ignores unknown types, and stops the runtime on invalid
// JSON or a larger message.
type workerMessage struct {
	Type   string          `json:"type"`
	TaskID string          `json:"taskId,omitempty"`
	Dir    string          `json:"dir,omitempty"`
	Shared bool            `json:"shared,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

type worker struct {
	cmd      *exec.Cmd
	conn     net.Conn
	ready    chan struct{} // closed when the runtime reports readiness
	stopped  chan struct{} // closed when shutdown begins; no new executions can register
	done     chan struct{} // closed after the worker process is reaped
	stopOnce sync.Once
	writeMu  sync.Mutex

	mu                 sync.Mutex // guards execution registration, delivery, and cleanup
	executionsByTaskID map[string]chan workerMessage
}

// startWorker launches the runtime in its own process group and gives it the control socket on fd 3.
func startWorker(cfg Config) (*worker, error) {
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
	if err := cmd.Start(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	w := &worker{
		cmd: cmd, conn: conn, ready: make(chan struct{}), stopped: make(chan struct{}),
		done: make(chan struct{}), executionsByTaskID: make(map[string]chan workerMessage),
	}
	go w.readControl()
	go func() {
		waitWorkerExit(cmd.Process.Pid)
		// The unreaped leader reserves the group ID. Any other caller of kill
		// either signals it first or waits for this call via stopOnce.
		w.kill()
		_ = cmd.Wait()
		close(w.done)
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
		case "done", "error":
			w.mu.Lock()
			if ch := w.executionsByTaskID[msg.TaskID]; ch != nil {
				select {
				case ch <- msg:
				default:
				}
			}
			w.mu.Unlock()
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
	w.kill()
}

// kill releases pending result waiters and signals the runtime's process group once.
func (w *worker) kill() {
	w.stopOnce.Do(func() {
		w.mu.Lock()
		close(w.stopped)
		for _, ch := range w.executionsByTaskID {
			close(ch)
		}
		clear(w.executionsByTaskID)
		w.mu.Unlock()

		_ = syscall.Kill(-w.cmd.Process.Pid, syscall.SIGKILL)
		_ = w.conn.Close()
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

// getWorker starts the shared runtime on demand and waits until it is ready to accept commands.
func (d *Daemon) getWorker(ctx context.Context) (*worker, error) {
	d.workerMu.Lock()
	if d.closed || d.startFailed {
		d.workerMu.Unlock()
		return nil, errWorkerUnavailable
	}
	if d.worker == nil {
		w, err := startWorker(d.cfg)
		if err != nil {
			// Like a runtime that stops, a failed start is not retried.
			d.startFailed = true
			d.workerMu.Unlock()
			return nil, fmt.Errorf("start runtime: %w", err)
		}
		d.worker = w
	}
	w := d.worker
	d.workerMu.Unlock()
	startup, cancel := context.WithTimeout(ctx, workerStartupTimeout)
	defer cancel()
	select {
	case <-w.stopped:
		return nil, errWorkerUnavailable
	case <-startup.Done():
		if ctx.Err() == nil {
			w.kill()
		}
		return nil, startup.Err()
	case <-w.ready:
		if w.isStopped() {
			return nil, errWorkerUnavailable
		}
		return w, nil
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
	worker *worker
	id     string
	result chan workerMessage // closed on worker failure; any buffered result is still readable
}

// dispatch registers a result receiver before sending the start command to the shared runtime.
func (d *Daemon) dispatch(ctx context.Context, id, dir string) (*execution, error) {
	w, err := d.getWorker(ctx)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	x := &execution{worker: w, id: id, result: make(chan workerMessage, 1)}
	w.mu.Lock()
	if w.isStopped() {
		w.mu.Unlock()
		return nil, errWorkerUnavailable
	}
	if _, exists := w.executionsByTaskID[id]; exists {
		w.mu.Unlock()
		return nil, errDuplicateTask
	}
	w.executionsByTaskID[id] = x.result
	w.mu.Unlock()
	d.markDispatched(id)
	if err := w.send(workerMessage{Type: "start", TaskID: id, Dir: dir, Shared: true}); err != nil {
		w.kill() // a partial command leaves the shared protocol unusable
		x.close()
		return nil, err
	}
	return x, nil
}

// close unregisters the result receiver without stopping the shared runtime.
func (x *execution) close() {
	x.worker.mu.Lock()
	defer x.worker.mu.Unlock()
	delete(x.worker.executionsByTaskID, x.id)
}
