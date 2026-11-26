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
	x, err := d.supervisor.dispatch(r.Context(), spec.TaskID, dir, d.cfg.taskTimeout(spec.TimeoutMs), d.markDispatched)
	if err != nil {
		slog.Warn("dispatch code task", "task_id", spec.TaskID, "error", err)
		_ = stream.emit(newErrorEvent(spec.TaskID, "crashed", "runtime unavailable"))
		return
	}
	if event, ok := d.runTask(r.Context(), spec, stream, x, root); ok {
		_ = stream.emit(event)
	}
}

// runTask streams updates while the execution watcher runs. It releases the
// execution before returning a terminal event, or returns false if the stream breaks.
func (d *Daemon) runTask(ctx context.Context, spec taskSpec, stream taskStream, x *execution, root *os.Root) (taskEvent, bool) {
	defer x.close()
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	// Start monitoring before the first HTTP write. The buffered result lets the
	// watcher finish even when the response is blocked.
	finished := make(chan executionOutcome, 1)
	go func() { finished <- x.watch(ctx) }()
	if stream.emit(newAcceptedEvent(spec.TaskID)) != nil {
		stop()
		<-finished
		return nil, false
	}
	outcome, ok := waitForTask(ctx, spec, stream, stop, finished)
	if !ok {
		return nil, false
	}
	if outcome.code != "" {
		return newErrorEvent(spec.TaskID, outcome.code, outcome.message), true
	}
	return d.resultEvent(spec, root, outcome.result), true
}

// waitForTask sends heartbeats while awaiting the execution outcome. If a
// heartbeat write fails, it stops the task, waits for execution to end, and
// returns no outcome.
func waitForTask(ctx context.Context, spec taskSpec, stream taskStream, stop context.CancelFunc, finished <-chan executionOutcome) (executionOutcome, bool) {
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
			// Wait for the watcher to finish before releasing the lane and task files.
			stop()
			<-finished
			return executionOutcome{}, false
		case <-ctx.Done():
			// The watcher stops the task. Daemon shutdown also cancels the
			// request while the stream is still open, so wait for the task
			// and send its terminal event. After a client disconnect, writing
			// that event fails harmlessly.
			return <-finished, true
		}
	}
}
