package codedaemon

import (
	"context"
	"net/http"
	"time"
)

const cancelGrace = time.Second

func (d *Daemon) handleCancel(w http.ResponseWriter, r *http.Request) {
	if !d.requestCancel(r.PathValue("taskId")) {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

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
			return executionOutcome{code: "crashed", message: "runtime exited"}
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
