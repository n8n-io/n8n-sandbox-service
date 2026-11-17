// This file handles task uploads and streams execution events as NDJSON.
package codedaemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	uploadTimeout     = 2 * time.Minute  // Total time for reading the request body.
	eventWriteTimeout = 10 * time.Second // Bounds response writes and flushes for one event.
	heartbeatInterval = 5 * time.Second
	inlineResultMax   = 1 << 20 // 1 MiB; default and maximum inline result size.
)

// Each event type below is one NDJSON line in the task stream.

// acceptedEvent tells the client that the runtime received the task.
type acceptedEvent struct {
	Type   string `json:"type"`
	TaskID string `json:"taskId"`
}

func newAcceptedEvent(taskID string) acceptedEvent {
	return acceptedEvent{Type: "accepted", TaskID: taskID}
}

// heartbeatEvent tells the client that the task still runs.
type heartbeatEvent struct {
	Type   string `json:"type"`
	TaskID string `json:"taskId"`
}

func newHeartbeatEvent(taskID string) heartbeatEvent {
	return heartbeatEvent{Type: "heartbeat", TaskID: taskID}
}

// doneEvent is the terminal event of a task that completed.
type doneEvent struct {
	Type    string          `json:"type"`
	TaskID  string          `json:"taskId"`
	Result  json.RawMessage `json:"result"`            // Inline result.json, or JSON null when not inlined.
	Outputs []outputEntry   `json:"outputs,omitempty"` // Manifest of published output files.
}

func newDoneEvent(taskID string, result json.RawMessage, outputs []outputEntry) doneEvent {
	return doneEvent{Type: "done", TaskID: taskID, Result: result, Outputs: outputs}
}

// errorEvent is the terminal event of a task that failed.
type errorEvent struct {
	Type      string          `json:"type"`
	TaskID    string          `json:"taskId"`
	Code      string          `json:"code"`
	Message   string          `json:"message"`
	UserError json.RawMessage `json:"userError,omitempty"`
	// Result is the inline result.json of a user_error task, when it has a
	// valid one within the inline limit.
	Result json.RawMessage `json:"result,omitempty"`
}

func newErrorEvent(taskID, code, message string) errorEvent {
	return errorEvent{Type: "error", TaskID: taskID, Code: code, Message: message}
}

// taskRejection is the plain JSON body of a rejection before the stream starts.
type taskRejection struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func newTaskRejection(code, message string) taskRejection {
	return taskRejection{Code: code, Message: message}
}

// taskStream encodes events and controls HTTP flushes and write deadlines.
type taskStream struct {
	enc *json.Encoder
	rc  *http.ResponseController
}

// newTaskStream sets streaming headers without committing the HTTP response.
func newTaskStream(w http.ResponseWriter) taskStream {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	return taskStream{enc: json.NewEncoder(w), rc: http.NewResponseController(w)}
}

// emit writes and flushes one event. Only the request goroutine writes events,
// so heartbeats and completion cannot race on the stream.
func (s taskStream) emit(e any) error {
	_ = s.rc.SetWriteDeadline(time.Now().Add(eventWriteTimeout))
	defer s.rc.SetWriteDeadline(time.Time{})
	if err := s.enc.Encode(e); err != nil {
		return err
	}
	return s.rc.Flush()
}

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

// waitForTask sends heartbeats while awaiting a terminal event. If the stream
// closes first, it waits for execution to end and returns no event to write.
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
		case <-ctx.Done():
		}
		// Until the runtime supports per-task cancellation, retain the lane
		// and files until the task ends or its deadline stops the process.
		<-finished
		return executionOutcome{}, false
	}
}

// resultEvent translates the worker's terminal message and validates successful outputs.
// The daemon does not look inside result.json. The runtime puts what the
// client needs there, such as the task's console output.
func (d *Daemon) resultEvent(spec taskSpec, root *os.Root, msg workerMessage) any {
	limit := spec.InlineResultMaxBytes
	if limit <= 0 || limit > inlineResultMax {
		limit = inlineResultMax
	}
	if msg.Type == "error" {
		e := newErrorEvent(spec.TaskID, "user_error", "code execution failed")
		e.UserError = msg.Error
		// The error is the outcome, so a missing, large, or invalid
		// result.json only leaves the result out.
		if info, err := root.Lstat("result.json"); err == nil && info.Mode().IsRegular() && info.Size() <= limit {
			if result, err := readResult(root, outputEntry{Path: "result.json", Size: info.Size()}); err == nil {
				e.Result = result
			}
		}
		return e
	}
	outputs, err := collectOutputs(root, d.cfg.Limits)
	if err != nil {
		return newErrorEvent(spec.TaskID, "invalid", "invalid or oversized outputs")
	}
	result := json.RawMessage("null")
	for _, output := range outputs {
		if output.Path != "result.json" || output.Size > limit {
			continue
		}
		if result, err = readResult(root, output); err != nil {
			return newErrorEvent(spec.TaskID, "invalid", "invalid result file")
		}
	}
	return newDoneEvent(spec.TaskID, result, outputs)
}

// readResult reads a result.json that must still match its manifest entry and be valid JSON.
func readResult(root *os.Root, entry outputEntry) (json.RawMessage, error) {
	f, err := openOutput(root, entry)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, entry.Size+1))
	if err != nil || int64(len(data)) != entry.Size || !json.Valid(data) {
		return nil, errors.New("invalid result file")
	}
	return data, nil
}

// openOutput rechecks the file against the manifest without following a final
// symlink or blocking if the file was replaced by a FIFO.
func openOutput(root *os.Root, expected outputEntry) (*os.File, error) {
	f, err := root.OpenFile(expected.Path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != expected.Size {
		_ = f.Close()
		return nil, errors.New("output changed after publication")
	}
	return f, nil
}
