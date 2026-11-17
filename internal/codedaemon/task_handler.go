// This file handles POST /tasks: it admits and stages the upload, dispatches
// the task, and streams its events until the task ends.
package codedaemon

import (
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const uploadTimeout = 2 * time.Minute // Total time for reading the request body.

// handleTask admits and stages the upload before dispatching and streaming events.
func (d *Daemon) handleTask(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	// Multipart EOF does not imply HTTP body EOF. Keep the deadline in place
	// for any unread body that net/http drains after the handler returns.
	_ = rc.SetReadDeadline(time.Now().Add(uploadTimeout))
	spec, mr := d.admit(w, r)
	if mr == nil {
		return
	}
	defer d.releaseLane()
	defer d.releaseTask(spec.TaskID)
	dir, err := d.stageInputs(mr)
	if err != nil {
		stopUnreadUpload(w, r)
		if errors.Is(err, errInputLimit) {
			writeTaskRejection(w, http.StatusRequestEntityTooLarge, "input_limit", "input limit exceeded")
		} else {
			writeTaskRejection(w, http.StatusBadRequest, "invalid", "could not stage inputs")
		}
		return
	}
	defer os.RemoveAll(dir)
	root, err := os.OpenRoot(filepath.Join(dir, "out"))
	if err != nil {
		writeTaskRejection(w, http.StatusServiceUnavailable, "crashed", "could not open output directory")
		return
	}
	defer root.Close()
	stream := newTaskStream(w)
	x, err := d.dispatch(r.Context(), spec.TaskID, dir, d.taskTimeout(spec.TimeoutMs))
	if err != nil {
		slog.Warn("dispatch code task", "task_id", spec.TaskID, "error", err)
		_ = stream.emit(newErrorEvent(spec.TaskID, "crashed", "runtime unavailable"))
		return
	}
	defer x.close()
	if stream.emit(newAcceptedEvent(spec.TaskID)) != nil {
		x.waitDetached()
		return
	}
	if event, ok := d.waitForTask(r, spec, stream, x, root); ok {
		_ = stream.emit(event)
	}
}

// waitForTask sends heartbeats while awaiting a terminal event. If a heartbeat
// write fails, it waits for execution to end and returns no event to write.
func (d *Daemon) waitForTask(r *http.Request, spec taskSpec, stream taskStream, x *execution, root *os.Root) (taskEvent, bool) {
	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()
	timer := time.NewTimer(time.Until(x.deadline))
	defer timer.Stop()
	for {
		select {
		case msg, ok := <-x.result:
			return d.terminalEvent(spec, root, msg, ok), true
		case <-timer.C:
			return x.timeoutEvent(spec), true
		case <-heartbeat.C:
			if stream.emit(newHeartbeatEvent(spec.TaskID)) == nil {
				continue
			}
			x.waitDetached()
			return nil, false
		case <-r.Context().Done():
			// Daemon shutdown also cancels the request while the stream is
			// still open. Wait for the task so the stream gets a terminal event.
			// After a client disconnect, writing that event fails harmlessly.
			select {
			case msg, ok := <-x.result:
				return d.terminalEvent(spec, root, msg, ok), true
			case <-timer.C:
				return x.timeoutEvent(spec), true
			}
		}
	}
}

// terminalEvent maps a receive from execution.result to the last stream event.
func (d *Daemon) terminalEvent(spec taskSpec, root *os.Root, msg workerMessage, ok bool) taskEvent {
	if !ok {
		return newErrorEvent(spec.TaskID, "crashed", "runtime exited")
	}
	return d.resultEvent(spec, root, msg)
}

// timeoutEvent stops the worker after the task deadline passes.
func (x *execution) timeoutEvent(spec taskSpec) any {
	x.worker.kill()
	<-x.worker.done
	return newErrorEvent(spec.TaskID, "timeout", "task execution deadline exceeded")
}
