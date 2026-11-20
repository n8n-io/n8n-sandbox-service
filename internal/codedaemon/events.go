// This file defines the NDJSON task events and writes them to the stream.
package codedaemon

import (
	"encoding/json"
	"net/http"
	"time"
)

const (
	eventWriteTimeout = 10 * time.Second // Bounds response writes and flushes for one event.
	heartbeatInterval = 5 * time.Second
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
	Result  json.RawMessage `json:"result"`  // Inline result.json, or JSON null when not inlined.
	Outputs []outputEntry   `json:"outputs"` // Manifest of published output files; [] when there are none.
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
