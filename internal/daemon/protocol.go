package daemon

import "time"

// RequestType enumerates the supported request types.
type RequestType string

const (
	RequestTypeExec       RequestType = "exec"
	RequestTypeFileRead   RequestType = "file_read"
	RequestTypeFileWrite  RequestType = "file_write"
	RequestTypeFileList   RequestType = "file_list"
	RequestTypeFileDelete RequestType = "file_delete"
	RequestTypeFileAppend RequestType = "file_append"
	RequestTypeFileCopy   RequestType = "file_copy"
	RequestTypeFileMove   RequestType = "file_move"
	RequestTypeFileMkdir  RequestType = "file_mkdir"
	RequestTypeFileStat   RequestType = "file_stat"
)

// ResponseType enumerates the supported response types.
type ResponseType string

const (
	ResponseTypeSession ResponseType = "session"
	ResponseTypeStdout  ResponseType = "stdout"
	ResponseTypeStderr  ResponseType = "stderr"
	ResponseTypeExit    ResponseType = "exit"
	ResponseTypeResult  ResponseType = "result"
	ResponseTypeError   ResponseType = "error"
)

// Request is the JSON envelope sent from client to daemon.
// The Type field determines which embedded fields are relevant.
type Request struct {
	// Type indicates which operation to perform.
	Type RequestType `json:"type"`

	// ExecRequest fields (used when Type == "exec")
	Command   string   `json:"command,omitempty"`
	Env       []string `json:"env,omitempty"`
	WorkDir   string   `json:"work_dir,omitempty"`
	TimeoutMs int64    `json:"timeout_ms,omitempty"`

	// FileRequest fields (used for file operations)
	Path      string `json:"path,omitempty"`
	Data      []byte `json:"data,omitempty"`      // for file_write / file_append
	MaxBytes  int64  `json:"max_bytes,omitempty"` // for file_read / file_write
	SrcPath   string `json:"src_path,omitempty"`  // for file_copy / file_move
	DestPath  string `json:"dest_path,omitempty"` // for file_copy / file_move
	Recursive bool   `json:"recursive,omitempty"` // for file_delete / file_mkdir / file_copy / file_list
	Force     bool   `json:"force,omitempty"`     // for file_delete
	Overwrite bool   `json:"overwrite,omitempty"` // for file_copy / file_move / file_write
	Extension string `json:"extension,omitempty"` // for file_list
}

// Response is the JSON envelope sent from daemon to client.
// For exec, multiple Response messages are streamed (stdout/stderr/exit).
// For file ops, a single Response is sent (result or error).
type Response struct {
	// Seq is a monotonically increasing sequence number for exec session events.
	Seq *uint64 `json:"seq,omitempty"`

	// Type indicates the kind of response.
	Type ResponseType `json:"type"`

	// ExecID identifies the exec session (set when Type == "session").
	ExecID string `json:"exec_id,omitempty"`

	// Data carries string output for stdout/stderr/result responses.
	Data string `json:"data,omitempty"`

	// ExitCode is set when Type == "exit".
	ExitCode int `json:"exit_code"`

	// Exec metadata fields (set when Type == "exit").
	Success         *bool `json:"success,omitempty"`
	ExecutionTimeMs int64 `json:"execution_time_ms"`
	TimedOut        *bool `json:"timed_out,omitempty"`
	Killed          *bool `json:"killed,omitempty"`

	// Files is set on a successful file_list result.
	Files []FileInfo `json:"files,omitempty"`

	// FileStat is set on a successful file_stat result.
	FileStat *FileStatInfo `json:"file_stat,omitempty"`

	// Error holds a human-readable error message when Type == "error".
	Error string `json:"error,omitempty"`
}

func newSessionResponse(execID string) Response {
	seq := uint64(0)
	return Response{Seq: &seq, Type: ResponseTypeSession, ExecID: execID}
}

func newErrorResponse(msg string) Response {
	return Response{Type: ResponseTypeError, Error: msg}
}

func (r Response) isTerminal() bool {
	return r.Type == ResponseTypeExit || r.Type == ResponseTypeError
}

// FileInfo describes a single directory entry returned by file_list.
type FileInfo struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	IsDir   bool      `json:"is_dir"`
	Type    string    `json:"type"` // "file" or "directory"
	ModTime time.Time `json:"mod_time"`
}

// FileStatInfo describes detailed file metadata returned by file_stat.
type FileStatInfo struct {
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	Type       string    `json:"type"` // "file" or "directory"
	Size       int64     `json:"size"`
	CreatedAt  time.Time `json:"created_at"`
	ModifiedAt time.Time `json:"modified_at"`
}
