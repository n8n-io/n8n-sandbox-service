// This file defines the control protocol on FD 3 and reads the runtime's
// control messages.

package codedaemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"syscall"
)

// maxControlMessage bounds every control message, including its newline.
const maxControlMessage = 512 << 10 // 512 KiB

var errControlMessageTooLong = fmt.Errorf("control message over %d bytes", maxControlMessage)

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
			if task := w.executionsByTaskID[msg.TaskID]; task != nil {
				select {
				case task.result <- msg:
				default:
				}
			}
			w.mu.Unlock()
		}
	}
	if err := scanner.Err(); errors.Is(err, bufio.ErrTooLong) {
		stopErr = errControlMessageTooLong
	} else if err != nil {
		stopErr = err
	}
	// A read error after kill comes from closing the connection ourselves.
	if stopErr != nil && !w.isStopped() {
		slog.Warn("stopping code runtime", "error", stopErr)
	}
	w.kill()
}
