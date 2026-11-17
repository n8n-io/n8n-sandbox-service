// This file handles cancellation requests and the runtime's cancellation handshake.

package codedaemon

import (
	"context"
	"net/http"
	"time"
)

// cancelGrace covers sending the cancel command and waiting for the runtime's reply.
const cancelGrace = time.Second

// handleCancel reports whether a cancellation was requested, not whether it succeeded.
func (d *Daemon) handleCancel(w http.ResponseWriter, r *http.Request) {
	if !d.requestCancel(r.PathValue("taskId")) {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// requestCancel signals a dispatched task once and retires its worker before
// another task can join it.
func (d *Daemon) requestCancel(id string) bool {
	d.workerMu.Lock()
	w := d.worker
	d.workerMu.Unlock()
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	x := w.executionsByTaskID[id]
	if x == nil {
		return false
	}
	select {
	case <-x.cancelRequested:
	default:
		w.retiring = true
		close(x.cancelRequested)
	}
	return true
}

// stop sends a cancel command to the retiring worker. If the runtime neither
// acknowledges it nor finishes the task in time, stop kills the shared worker.
func (x *execution) stop() {
	w := x.worker
	w.mu.Lock()
	w.retiring = true
	w.mu.Unlock()
	deadline := time.Now().Add(cancelGrace)
	if w.send(workerMessage{Type: "cancel", TaskID: x.id}, deadline) == nil {
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		select {
		case <-x.cancelAck:
			return
		case <-x.result:
			return // already finished, but the worker is still retired
		case <-w.done:
			return
		case <-timer.C:
		}
	}
	w.kill()
	<-w.done
}

type executionOutcome struct {
	result        workerMessage // Terminal runtime reply, including a possible user error.
	code, message string        // Daemon error details; when code is set, result is unused.
}

// Watch the runtime separately from the HTTP writer so slow clients can't
// postpone a timeout or an explicit cancellation.
func (x *execution) watch(ctx context.Context) executionOutcome {
	timer := time.NewTimer(time.Until(x.deadline))
	defer timer.Stop()
	select {
	case msg, ok := <-x.result:
		if !ok {
			// The worker classifies the process exit once for all unfinished tasks.
			return executionOutcome{code: x.worker.exitCode, message: "runtime exited"}
		}
		return executionOutcome{result: msg}
	case <-timer.C:
		x.stop()
		return executionOutcome{code: "timeout", message: "task execution deadline exceeded"}
	case <-x.cancelRequested:
		x.stop()
		return executionOutcome{code: "cancelled", message: "task cancelled"}
	case <-ctx.Done():
		x.stop()
		return executionOutcome{code: "cancelled", message: "task stream closed"}
	}
}
