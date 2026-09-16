package daemon

import "time"

// ResponseType enumerates the supported response types.
type ResponseType string

const (
	ResponseTypeStarted ResponseType = "started"
	ResponseTypeStdout  ResponseType = "stdout"
	ResponseTypeStderr  ResponseType = "stderr"
	ResponseTypeExit    ResponseType = "exit"
	ResponseTypeError   ResponseType = "error"
)

// Response is one NDJSON event of an exec stream (started/stdout/stderr/exit/error).
// File routes return plain JSON ([]FileInfo, FileStatInfo) instead.
type Response struct {
	// Seq is a monotonically increasing sequence number for exec session events.
	// Keep Seq and Type as the first marshaled fields: the runner exec proxy
	// depends on this wire prefix to resume streams without JSON-decoding events.
	Seq *uint64 `json:"seq,omitempty"`

	// Type indicates the kind of response.
	Type ResponseType `json:"type"`

	// ExecID identifies the execution (set on the "started" event).
	ExecID string `json:"exec_id,omitempty"`

	// Data carries string output for stdout/stderr events.
	Data string `json:"data,omitempty"`

	// ExitCode is set when Type == "exit".
	ExitCode int `json:"exit_code"`

	// Exec metadata fields (set when Type == "exit").
	Success         *bool `json:"success,omitempty"`
	ExecutionTimeMs int64 `json:"execution_time_ms"`
	TimedOut        *bool `json:"timed_out,omitempty"`
	Killed          *bool `json:"killed,omitempty"`

	// Error holds a human-readable error message when Type == "error".
	Error string `json:"error,omitempty"`
}

func newStartedResponse(execID string) Response {
	seq := uint64(0)
	return Response{Seq: &seq, Type: ResponseTypeStarted, ExecID: execID}
}

func newErrorResponse(msg string) Response {
	return Response{Type: ResponseTypeError, Error: msg}
}

func (r Response) isTerminal() bool {
	return r.Type == ResponseTypeExit || r.Type == ResponseTypeError
}

// FileInfo describes a single directory entry returned by the file list route.
type FileInfo struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	IsDir   bool      `json:"is_dir"`
	Type    string    `json:"type"` // "file" or "directory"
	ModTime time.Time `json:"mod_time"`
}

// FileStatInfo describes file metadata returned by the file stat route.
type FileStatInfo struct {
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	Type       string    `json:"type"` // "file" or "directory"
	Size       int64     `json:"size"`
	CreatedAt  time.Time `json:"created_at"`
	ModifiedAt time.Time `json:"modified_at"`
}
