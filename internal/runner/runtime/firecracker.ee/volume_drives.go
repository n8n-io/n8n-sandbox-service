package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// volumeDrive is a read-only drive the guest mounts; the guest sees drive N as
// /dev/vd followed by the letter b+N.
type volumeDrive struct {
	Drive   int    `json:"drive"`
	Sectors uint64 `json:"sectors"`
	Path    string `json:"path"`
}

// The guest waits up to 2s per drive for its new size, for at most eight.
const mountVolumeDrivesTimeout = 30 * time.Second

// mountVolumeDrives sends the guest daemon's one volume mount request, after
// which the daemon drops root. Never retried: later requests get 409. Without
// drives, 404 (older daemon) and 501 (daemon not root) count as success too.
func mountVolumeDrives(ctx context.Context, baseURL string, drives []volumeDrive) error {
	if drives == nil {
		drives = []volumeDrive{}
	}
	body, err := json.Marshal(struct {
		Volumes []volumeDrive `json:"volumes"`
	}{drives})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/volumes/mount", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// A connection of its own: a POST is not retried on a stale pooled one.
	client := http.Client{
		Timeout:       mountVolumeDrivesTimeout,
		Transport:     &http.Transport{DisableKeepAlives: true},
		CheckRedirect: runnerruntime.RefuseRedirect,
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNoContent:
		return nil
	case len(drives) == 0 && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNotImplemented):
		return nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	var answer struct {
		Error  string `json:"error"`
		Reason string `json:"reason"`
	}
	if json.Unmarshal(raw, &answer) != nil || answer.Error == "" {
		answer.Error = strings.TrimSpace(string(raw))
	}
	return &volumeMountError{status: resp.StatusCode, reason: answer.Reason, msg: answer.Error}
}

// volumeMountError is a daemon's answer other than success.
type volumeMountError struct {
	status int
	reason string
	msg    string
}

func (e *volumeMountError) Error() string {
	return fmt.Sprintf("daemon answered %d: %s", e.status, e.msg)
}

// volumeMountReasons are the daemon's reasons for a failed mount. Only these
// become metric labels, never other text from the guest.
var volumeMountReasons = []string{"drive", "mount_point", "mount"}

// volumeMountResult is the result label of a mountVolumeDrives call.
func volumeMountResult(err error) string {
	var answer *volumeMountError
	switch {
	case err == nil:
		return "success"
	case !errors.As(err, &answer):
		return "no_response"
	case answer.status == http.StatusBadRequest:
		return "invalid_request"
	case answer.status == http.StatusConflict:
		return "already_mounted"
	case answer.status == http.StatusNotFound, answer.status == http.StatusNotImplemented:
		return "unsupported"
	case answer.status == http.StatusInternalServerError && slices.Contains(volumeMountReasons, answer.reason):
		return answer.reason
	default:
		return "error"
	}
}
