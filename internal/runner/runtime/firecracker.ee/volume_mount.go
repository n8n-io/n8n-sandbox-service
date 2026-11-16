package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/n8n-io/sandbox-service/internal/daemon/volumedrives"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
	"github.com/n8n-io/sandbox-service/internal/volume"
)

// The guest waits up to 2s per drive for its new size.
const mountVolumeDrivesTimeout = volume.MaxDrives*2*time.Second + 15*time.Second

// mountVolumeDrives sends the guest daemon's one volume mount request, after
// which the daemon drops root. Never retried: later requests get 409. It
// returns the drives the guest did not mount, with the daemon's reasons.
//
// The runner takes nothing from the answer but its shape: a status, an
// allowlisted reason, and drives it asked for. Daemons that mount all or none
// answer a request with drives 204 for all and 500 with a reason for none.
// Without drives, 404 (older daemon) and 501 (daemon not root) count as
// success too.
func mountVolumeDrives(ctx context.Context, baseURL string, drives []volumedrives.Volume) (map[int]string, error) {
	if drives == nil {
		drives = []volumedrives.Volume{}
	}
	body, err := json.Marshal(struct {
		Volumes []volumedrives.Volume `json:"volumes"`
	}{drives})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/volumes/mount", bytes.NewReader(body))
	if err != nil {
		return nil, err
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
		// Not wrapped: net/http quotes a malformed answer in its error.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("daemon gave no answer: %w", ctxErr)
		}
		return nil, errors.New("daemon gave no answer")
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNoContent:
		return nil, nil
	case len(drives) == 0 && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNotImplemented):
		return nil, nil
	case len(drives) > 0 && resp.StatusCode == http.StatusOK:
		raw, err := io.ReadAll(io.LimitReader(resp.Body, volumedrives.MaxAnswerBytes+1))
		if err != nil {
			return nil, &volumeMountError{status: resp.StatusCode, invalidAnswer: errors.New("answer could not be read")}
		}
		outcomes, err := volumedrives.ParseAnswer(raw, drives)
		if err != nil {
			return nil, &volumeMountError{status: resp.StatusCode, invalidAnswer: err}
		}
		failed := map[int]string{}
		for _, outcome := range outcomes {
			if !outcome.Mounted {
				failed[outcome.Drive] = outcome.Reason
			}
		}
		return failed, nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	var answer struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal(raw, &answer) != nil || !volumedrives.KnownReason(answer.Reason) {
		answer.Reason = ""
	}
	if len(drives) > 0 && resp.StatusCode == http.StatusInternalServerError && answer.Reason != "" {
		failed := make(map[int]string, len(drives))
		for _, drive := range drives {
			failed[drive.Drive] = answer.Reason
		}
		return failed, nil
	}
	return nil, &volumeMountError{status: resp.StatusCode, reason: answer.Reason}
}

// volumeMountError is a daemon's answer that says nothing about the drives.
// It carries no text from the guest.
type volumeMountError struct {
	status int
	// reason is allowlisted.
	reason        string
	invalidAnswer error
}

func (e *volumeMountError) Error() string {
	switch {
	case e.invalidAnswer != nil:
		return fmt.Sprintf("daemon answered %d with an invalid answer: %v", e.status, e.invalidAnswer)
	case e.reason != "":
		return fmt.Sprintf("daemon answered %d (%s)", e.status, e.reason)
	default:
		return fmt.Sprintf("daemon answered %d", e.status)
	}
}

// volumeMountResult is the result label of a mountVolumeDrives call: success
// when every drive mounted, and for a request where some did not, the reason
// of the first in request order. Only allowlisted values, never other text
// from the guest.
func volumeMountResult(drives []volumedrives.Volume, failed map[int]string, err error) string {
	var answer *volumeMountError
	switch {
	case err == nil:
		for _, drive := range drives {
			if reason, ok := failed[drive.Drive]; ok {
				return reason
			}
		}
		return "success"
	case !errors.As(err, &answer):
		return "no_response"
	case answer.invalidAnswer != nil:
		return "invalid_answer"
	case answer.status == http.StatusBadRequest:
		return "invalid_request"
	case answer.status == http.StatusConflict:
		return "already_mounted"
	case answer.status == http.StatusNotFound, answer.status == http.StatusNotImplemented:
		return "unsupported"
	case answer.status == http.StatusInternalServerError && answer.reason != "":
		return answer.reason
	default:
		return "error"
	}
}
