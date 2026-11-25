// This file validates task specs and reserves execution lanes before reading input files.

package codedaemon

import (
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"regexp"
	"time"
)

const maxSpecBytes = 1 << 10 // 1 KiB

var taskIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

type taskSpec struct {
	TaskID               string `json:"taskId"`
	InlineResultMaxBytes int64  `json:"inlineResultMaxBytes"` // Requested inline limit; defaults to the daemon cap if unset or out of range.
}

// admit validates the first multipart part and tries to reserve a lane without
// waiting. On success, the caller holds the lane through upload and execution.
// On rejection, do not advance the multipart reader or close a part: either
// can consume data from the upload being refused.
func (d *Daemon) admit(w http.ResponseWriter, r *http.Request) (taskSpec, *multipart.Reader) {
	// For HTTP/1, net/http by default reads any unread request body before it
	// writes the response. A rejection would then wait for the whole upload it
	// refuses. Full duplex lets the error go out while the client is still sending.
	if r.ProtoMajor == 1 {
		_ = http.NewResponseController(w).EnableFullDuplex()
	}
	mr, err := r.MultipartReader()
	if err != nil {
		rejectAdmission(w, r, "expected multipart/form-data", http.StatusBadRequest)
		return taskSpec{}, nil
	}
	part, err := mr.NextPart()
	if err != nil || part.FormName() != "spec" {
		rejectAdmission(w, r, "first part must be spec", http.StatusBadRequest)
		return taskSpec{}, nil
	}
	var spec taskSpec
	// Read one extra byte to distinguish an oversized spec from one at the limit.
	data, err := io.ReadAll(io.LimitReader(part, maxSpecBytes+1))
	if err != nil || len(data) > maxSpecBytes || json.Unmarshal(data, &spec) != nil || !taskIDPattern.MatchString(spec.TaskID) {
		slog.Warn("DEBUG invalid task spec", "error", err, "bytes", len(data), "data", string(data), "unmarshal", json.Unmarshal(data, &spec), "task_id", spec.TaskID,
			"content_type", r.Header.Get("Content-Type"), "content_length", r.ContentLength, "transfer_encoding", r.TransferEncoding, "proto", r.Proto, "part_header", part.Header)
		rejectAdmission(w, r, "invalid task spec", http.StatusBadRequest)
		return taskSpec{}, nil
	}
	if err := d.reserveTask(spec.TaskID); err != nil {
		stopUnreadUpload(w, r)
		writeTaskRejection(w, http.StatusConflict, "duplicate_task", err.Error())
		return taskSpec{}, nil
	}
	// d.lanes is a semaphore: its capacity is the lane count and each token in it
	// is a taken lane. The send succeeds only if a lane is free. With a default
	// case it never blocks, so a full sandbox answers 429 instead of queueing.
	select {
	case d.lanes <- struct{}{}:
		return spec, mr
	default:
		d.releaseTask(spec.TaskID)
		stopUnreadUpload(w, r)
		writeTaskRejection(w, http.StatusTooManyRequests, "sandbox_full", "no free lane")
		return taskSpec{}, nil
	}
}

// rejectAdmission writes the error without waiting for an unfinished HTTP/1 upload.
func rejectAdmission(w http.ResponseWriter, r *http.Request, message string, status int) {
	stopUnreadUpload(w, r)
	http.Error(w, message, status)
}

func stopUnreadUpload(w http.ResponseWriter, r *http.Request) {
	if r.ProtoMajor == 1 {
		// Do not drain an unread upload to reuse the connection. net/http can
		// still read during body cleanup after sending the response, so also
		// give those reads an expired deadline.
		w.Header().Set("Connection", "close")
		rc := http.NewResponseController(w)
		_ = rc.EnableFullDuplex()
		_ = rc.SetReadDeadline(time.Now())
	}
}

// releaseLane frees one slot and must be called once per successful admission.
func (d *Daemon) releaseLane() { <-d.lanes }

// writeTaskRejection sends a JSON error before the stream so the client gets
// an HTTP error status instead of a terminal event in a successful response.
func writeTaskRejection(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(newTaskRejection(code, message))
}
