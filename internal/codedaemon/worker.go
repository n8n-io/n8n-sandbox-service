package codedaemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// workerMsg is one JSON line on the control socket (fd 3), in either direction.
type workerMsg struct {
	Type     string          `json:"type"`
	TaskID   string          `json:"taskId,omitempty"`
	Dir      string          `json:"dir,omitempty"`
	Deadline int64           `json:"deadline,omitempty"`
	CallID   string          `json:"callId,omitempty"`
	Name     string          `json:"name,omitempty"`
	Params   json.RawMessage `json:"params,omitempty"`
	OK       *bool           `json:"ok,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
	Error    json.RawMessage `json:"error,omitempty"`
	Timings  json.RawMessage `json:"timings,omitempty"`
	// Shared in "start": the worker is the shared process of the sandbox
	// (reuse mode). It runs tasks at the same time and stays alive.
	Shared bool `json:"shared,omitempty"`
	// Stream and Text in "output": console output of a task in a shared
	// worker. The stdout and stderr pipes are shared by all its tasks, so
	// the worker sends task output on the control socket instead.
	Stream string `json:"stream,omitempty"`
	Text   string `json:"text,omitempty"`
}

type outChunk struct {
	stream string // "stdout" or "stderr"
	data   []byte
}

// stderrTailSize is how much stderr a worker keeps to detect a V8 heap OOM.
const stderrTailSize = 4096

// maxControlLine bounds one control message from a worker (an RPC call with
// its params). A longer line kills the worker.
const maxControlLine = 64 << 20

// controlWriteTimeout bounds a write to the control socket.
const controlWriteTimeout = 10 * time.Second

// worker is one runtime process. It serves one task and is then killed, or it
// is the shared worker of the sandbox in reuse mode (shared.go).
type worker struct {
	cmd       *exec.Cmd
	pgid      int
	ctrl      *os.File
	writeMu   sync.Mutex
	msgs      chan workerMsg // closed when the control socket reaches EOF
	out       chan outChunk  // closed when stdout and stderr both reach EOF
	ready     chan struct{}  // closed when the worker sends "ready"
	exited    chan struct{}  // closed when the process exits
	abandon   chan struct{}  // closed when the daemon stops reading output
	readyOnce sync.Once
	abandonMu sync.Once
	killed    atomic.Bool
	startedAt time.Time
	readyAt   atomic.Int64 // unix nanos

	stdoutR, stderrR *os.File

	tailMu     sync.Mutex
	stderrTail []byte

	state *os.ProcessState

	// tasks is the number of tasks the worker has served. Set by the task
	// handler only.
	tasks int
}

// startWorker starts a worker process in dir with a new process group. The
// control socket is fd 3.
func startWorker(command []string, env []string, dir string) (*worker, error) {
	if len(command) == 0 {
		return nil, errors.New("empty worker command")
	}

	// ForkLock keeps the new descriptors out of children that other goroutines
	// fork before CloseOnExec is set.
	syscall.ForkLock.RLock()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fds[0])
		syscall.CloseOnExec(fds[1])
		// Non-blocking, so os.NewFile uses the poller and write deadlines work:
		// a worker that stops reading must not block the daemon.
		err = syscall.SetNonblock(fds[0], true)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, fmt.Errorf("socketpair: %w", err)
	}
	parentCtrl := os.NewFile(uintptr(fds[0]), "ctrl-parent")
	childCtrl := os.NewFile(uintptr(fds[1]), "ctrl-child")

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		parentCtrl.Close()
		childCtrl.Close()
		return nil, err
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		parentCtrl.Close()
		childCtrl.Close()
		stdoutR.Close()
		stdoutW.Close()
		return nil, err
	}

	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = nil // /dev/null
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	cmd.ExtraFiles = []*os.File{childCtrl}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	startErr := cmd.Start()
	// The child has its own copies now.
	childCtrl.Close()
	stdoutW.Close()
	stderrW.Close()
	if startErr != nil {
		parentCtrl.Close()
		stdoutR.Close()
		stderrR.Close()
		return nil, fmt.Errorf("start worker: %w", startErr)
	}

	w := &worker{
		cmd:       cmd,
		pgid:      cmd.Process.Pid,
		ctrl:      parentCtrl,
		msgs:      make(chan workerMsg, 64),
		out:       make(chan outChunk, 256),
		ready:     make(chan struct{}),
		exited:    make(chan struct{}),
		abandon:   make(chan struct{}),
		startedAt: time.Now(),
		stdoutR:   stdoutR,
		stderrR:   stderrR,
	}

	var streams sync.WaitGroup
	streams.Add(2)
	go w.readStream("stdout", stdoutR, &streams)
	go w.readStream("stderr", stderrR, &streams)
	go func() {
		streams.Wait()
		close(w.out)
	}()
	go w.readControl()
	go func() {
		_ = cmd.Wait()
		w.state = cmd.ProcessState
		close(w.exited)
	}()
	return w, nil
}

// readStream reads output in chunks. It does not split on lines, so a long line
// has no size limit.
func (w *worker) readStream(name string, r *os.File, wg *sync.WaitGroup) {
	defer wg.Done()
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			c := make([]byte, n)
			copy(c, buf[:n])
			if name == "stderr" {
				w.keepStderrTail(c)
			}
			select {
			case w.out <- outChunk{stream: name, data: c}:
			case <-w.abandon:
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (w *worker) keepStderrTail(c []byte) {
	w.tailMu.Lock()
	defer w.tailMu.Unlock()
	w.stderrTail = append(w.stderrTail, c...)
	if len(w.stderrTail) > stderrTailSize {
		w.stderrTail = w.stderrTail[len(w.stderrTail)-stderrTailSize:]
	}
}

func (w *worker) readControl() {
	defer close(w.msgs)
	br := bufio.NewReaderSize(w.ctrl, 64*1024)
	for {
		line, err := readLimitedLine(br, maxControlLine)
		if errors.Is(err, errLineTooLong) {
			w.kill()
			return
		}
		if len(bytes.TrimSpace(line)) > 0 {
			var m workerMsg
			if jerr := json.Unmarshal(line, &m); jerr == nil {
				if m.Type == "ready" {
					w.readyOnce.Do(func() {
						w.readyAt.Store(time.Now().UnixNano())
						close(w.ready)
					})
					continue
				}
				select {
				case w.msgs <- m:
				case <-w.abandon:
					return
				}
			}
		}
		if err != nil {
			return
		}
	}
}

var errLineTooLong = errors.New("control line too long")

// readLimitedLine reads up to and including '\n', with at most max bytes.
func readLimitedLine(br *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > max {
			return nil, errLineTooLong
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line, err
	}
}

// send writes one JSON line to the control socket.
func (w *worker) send(m any) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	_ = w.ctrl.SetWriteDeadline(time.Now().Add(controlWriteTimeout))
	_, err = w.ctrl.Write(b)
	return err
}

// kill sends SIGKILL to the process group of the worker.
func (w *worker) kill() {
	w.killed.Store(true)
	_ = syscall.Kill(-w.pgid, syscall.SIGKILL)
}

// release stops all reader goroutines and closes the descriptors. Call it after
// kill, when the daemon needs no more output.
func (w *worker) release() {
	w.abandonMu.Do(func() { close(w.abandon) })
	_ = w.ctrl.Close()
	_ = w.stdoutR.Close()
	_ = w.stderrR.Close()
}

// waitReady waits until the worker has loaded its runtime.
func (w *worker) waitReady(timeout time.Duration, cancel <-chan struct{}) error {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-w.ready:
		return nil
	case <-w.exited:
		return fmt.Errorf("worker exited before it was ready: %s", w.exitDescription())
	case <-t.C:
		return errors.New("worker was not ready in time")
	case <-cancel:
		return errors.New("cancelled")
	}
}

// exitCode classifies an unexpected exit as "oom" or "crashed". Best effort.
func (w *worker) exitCode() string {
	w.tailMu.Lock()
	tail := string(w.stderrTail)
	w.tailMu.Unlock()
	if bytes.Contains([]byte(tail), []byte("heap out of memory")) ||
		bytes.Contains([]byte(tail), []byte("Allocation failed")) {
		return "oom"
	}
	if w.state != nil {
		if ws, ok := w.state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			switch ws.Signal() {
			case syscall.SIGKILL:
				// The daemon did not kill it, so the kernel OOM killer is the
				// likely cause.
				if !w.killed.Load() {
					return "oom"
				}
			case syscall.SIGABRT:
				return "oom"
			}
		}
	}
	return "crashed"
}

func (w *worker) exitDescription() string {
	if w.state == nil {
		return "unknown"
	}
	return w.state.String()
}

func (w *worker) stderrTailString() string {
	w.tailMu.Lock()
	defer w.tailMu.Unlock()
	return string(w.stderrTail)
}

// drainOutput reads the remaining output after the worker was killed. It stops
// at EOF or after the timeout, because a process that escaped the process group
// can keep the pipes open.
func (w *worker) drainOutput(timeout time.Duration, emit func(outChunk)) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	for {
		select {
		case c, ok := <-w.out:
			if !ok {
				return
			}
			emit(c)
		case <-t.C:
			return
		}
	}
}
