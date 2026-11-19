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
	x, err := d.dispatch(r.Context(), spec.TaskID, dir)
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

// waitForTask sends heartbeats while awaiting a terminal event. If the stream
// closes first, it waits for execution to end and returns no event to write.
func (d *Daemon) waitForTask(r *http.Request, spec taskSpec, stream taskStream, x *execution, root *os.Root) (any, bool) {
	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()
	for {
		select {
		case msg, ok := <-x.result:
			if !ok {
				return newErrorEvent(spec.TaskID, "crashed", "runtime exited"), true
			}
			return d.resultEvent(spec, root, msg), true
		case <-heartbeat.C:
			if stream.emit(newHeartbeatEvent(spec.TaskID)) == nil {
				continue
			}
		case <-r.Context().Done():
		}
		x.waitDetached()
		return nil, false
	}
}
