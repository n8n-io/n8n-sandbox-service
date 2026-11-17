// This file handles POST /tasks: it admits and stages the upload, dispatches
// the task, and streams its events until the task ends.
package codedaemon

import (
	"context"
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
	// Start monitoring before the first HTTP write. The buffered result lets the
	// watcher finish even when the response is blocked.
	finished := make(chan executionOutcome, 1)
	go func() { finished <- x.watch() }()
	if stream.emit(newAcceptedEvent(spec.TaskID)) != nil {
		<-finished
		return
	}
	outcome, ok := waitForTask(r.Context(), spec, stream, finished)
	if !ok {
		return
	}
	if outcome.code != "" {
		_ = stream.emit(newErrorEvent(spec.TaskID, outcome.code, outcome.message))
		return
	}
	_ = stream.emit(d.resultEvent(spec, root, outcome.result))
}

// waitForTask sends heartbeats while awaiting the execution outcome. If a
// heartbeat write fails, it waits for execution to end and returns no outcome.
func waitForTask(ctx context.Context, spec taskSpec, stream taskStream, finished <-chan executionOutcome) (executionOutcome, bool) {
	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()
	for {
		select {
		case outcome := <-finished:
			return outcome, true
		case <-heartbeat.C:
			if stream.emit(newHeartbeatEvent(spec.TaskID)) == nil {
				continue
			}
			// Until the runtime supports per-task cancellation, retain the lane
			// and files until the task ends or its deadline stops the process.
			<-finished
			return executionOutcome{}, false
		case <-ctx.Done():
			// Daemon shutdown also cancels the request while the stream is
			// still open. Wait for the task so the stream gets a terminal event.
			// After a client disconnect, writing that event fails harmlessly.
			return <-finished, true
		}
	}
}
