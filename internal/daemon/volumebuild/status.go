package volumebuild

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	// StatusBytes is the size of the status drive and of the record on it.
	StatusBytes = 4096

	statusVersion = 1

	ResultOK     = "ok"
	ResultFailed = "failed"
)

// Status is the record the guest leaves at the start of the status drive: JSON
// padded with zeros to StatusBytes. It holds no text from the tar. A change
// to its JSON needs a new statusVersion: ParseStatus takes only the bytes
// encode writes, and the guest's daemon can be older than the runner.
type Status struct {
	Version int    `json:"version"`
	Result  string `json:"result"`
	// Reason is one of the Reason constants if Result is ResultFailed, and
	// empty otherwise.
	Reason string `json:"reason,omitempty"`
	// Files and Bytes count the regular files unpacked.
	Files int64 `json:"files"`
	Bytes int64 `json:"bytes"`
	// UsedBytes is what the files and directories take in the image.
	UsedBytes int64 `json:"used_bytes"`
	FormatMs  int64 `json:"format_ms"`
	ExtractMs int64 `json:"extract_ms"`
	SyncMs    int64 `json:"sync_ms"`
	FsckMs    int64 `json:"fsck_ms"`
}

func (s Status) encode() ([]byte, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	if len(raw) > StatusBytes {
		return nil, fmt.Errorf("status record takes %d bytes, more than %d", len(raw), StatusBytes)
	}
	record := make([]byte, StatusBytes)
	copy(record, raw)
	return record, nil
}

// ParseStatus reads a status record written by a guest the runner does not
// trust. It accepts only what encode writes; a drive the guest never wrote
// holds zeros and is refused.
func ParseStatus(record []byte) (Status, error) {
	if len(record) != StatusBytes {
		return Status{}, fmt.Errorf("status record has %d bytes, want %d", len(record), StatusBytes)
	}
	raw, padding, _ := bytes.Cut(record, []byte{0})
	if len(raw) == 0 {
		return Status{}, errors.New("status record is empty")
	}
	if len(bytes.TrimLeft(padding, "\x00")) != 0 {
		return Status{}, errors.New("status record has data after its end")
	}

	var status Status
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&status); err != nil {
		return Status{}, errors.New("status record is not the expected JSON object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return Status{}, errors.New("status record holds more than one JSON value")
	}

	if status.Version != statusVersion {
		return Status{}, fmt.Errorf("status record has version %d, want %d", status.Version, statusVersion)
	}
	switch {
	case status.Result == ResultOK && status.Reason == "":
	case status.Result == ResultFailed && knownReason(status.Reason):
	default:
		return Status{}, errors.New("status record has an unknown result or reason")
	}
	for _, count := range []int64{status.Files, status.Bytes, status.UsedBytes, status.FormatMs, status.ExtractMs, status.SyncMs, status.FsckMs} {
		if count < 0 {
			return Status{}, errors.New("status record has a negative count")
		}
	}
	// JSON has many spellings of these values. Only encode's passes.
	if written, err := status.encode(); err != nil || !bytes.Equal(written, record) {
		return Status{}, errors.New("status record is not written as the builder writes it")
	}
	return status, nil
}
