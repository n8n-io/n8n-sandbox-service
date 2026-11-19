package runner

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/n8n-io/sandbox-service/internal/runner/activity"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// activityPollInterval is how often executions nobody is streaming are checked
// against the daemon. Shorter than the heartbeat, so an ended run stops
// counting as activity by the next one.
const activityPollInterval = 5 * time.Second

// PollUnwatchedExecutions asks each sandbox's daemon whether the executions the
// tracker holds without an open stream have ended, until ctx is done. It never
// wakes a sandbox: one that is not running has no executions left.
func PollUnwatchedExecutions(ctx context.Context, rt runnerruntime.Runtime, tr *activity.Tracker) {
	client := &http.Client{CheckRedirect: runnerruntime.RefuseRedirect, Timeout: 10 * time.Second}
	t := time.NewTicker(activityPollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pollUnwatchedOnce(ctx, rt, tr, client)
		}
	}
}

func pollUnwatchedOnce(ctx context.Context, rt runnerruntime.Runtime, tr *activity.Tracker, client *http.Client) {
	for sandboxID, execIDs := range tr.Unwatched() {
		base, err := rt.DaemonURL(ctx, sandboxID)
		if errors.Is(err, runnerruntime.ErrSandboxNotRunning) || errors.Is(err, runnerruntime.ErrSandboxNotFound) {
			tr.Forget(sandboxID)
			continue
		}
		if err != nil {
			continue // next poll, or the execution's deadline
		}
		for _, p := range execIDs {
			ended, seq, err := executionEnded(ctx, client, base, p)
			switch {
			case err != nil:
				slog.Debug("activity poll failed", "sandbox_id", sandboxID, "exec_id", p.ExecID, "err", err)
			case ended:
				tr.ExecEnded(sandboxID, p.ExecID)
			case seq != nil:
				tr.ExecProgress(sandboxID, p.ExecID, *seq)
			}
		}
	}
}

// executionEnded reads the execution's history after the last event seen: it
// has ended once a terminal event is in it, or once the daemon no longer has
// it. It also returns the last event read. A 410 means history was trimmed
// past what was seen; that is undecided, and the execution's deadline settles it.
func executionEnded(ctx context.Context, client *http.Client, base string, p activity.Pending) (ended bool, last *uint64, err error) {
	u := base + "/executions/" + url.PathEscape(p.ExecID)
	if p.Seq != nil {
		u += "?after=" + strconv.FormatUint(*p.Seq, 10)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return true, nil, nil
	default:
		return false, nil, errors.New("daemon answered " + resp.Status)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		seq, typ, err := execEventPrefix(append(sc.Bytes(), '\n'))
		if err != nil {
			continue
		}
		last = &seq
		if isTerminalEvent(typ) {
			return true, last, nil
		}
	}
	// A line longer than the buffer ends the scan: undecided, ask again.
	return false, last, sc.Err()
}
