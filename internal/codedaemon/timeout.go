// This file loads execution limits and enforces task deadlines.
package codedaemon

import (
	"time"

	"github.com/n8n-io/sandbox-service/internal/envvar"
)

const (
	defaultMaxTaskTimeout = time.Minute
	// Hard cap on the configured maximum. It matches the runner's default exec
	// limit and stays below the API's default response activity window (30m),
	// so a running task keeps its sandbox from going idle.
	maxTaskTimeoutCap = 15 * time.Minute
)

// loadMaxTaskTimeout returns the service cap on the execution duration.
func loadMaxTaskTimeout() (time.Duration, error) {
	value := defaultMaxTaskTimeout
	if err := envvar.DurationBetween("CODE_DAEMON_MAX_TASK_TIMEOUT", &value, time.Millisecond, maxTaskTimeoutCap); err != nil {
		return 0, err
	}
	return value, nil
}

// taskTimeout uses the requested duration, but never more than the service cap.
func (d *Daemon) taskTimeout(requestMillis int64) time.Duration {
	limit := d.cfg.MaxTimeout
	// Compare before converting, since the request value may overflow a duration.
	if requestMillis > 0 && requestMillis <= limit.Milliseconds() {
		return time.Duration(requestMillis) * time.Millisecond
	}
	return limit
}

type executionOutcome struct {
	result        workerMessage // Terminal runtime reply, including a possible user error.
	code, message string        // Daemon error details; when code is set, result is unused.
}

// watch waits for the runtime reply or stops the shared worker at the deadline.
// It runs independently of HTTP writes, so a blocked client cannot delay enforcement.
func (x *execution) watch() executionOutcome {
	timer := time.NewTimer(time.Until(x.deadline))
	defer timer.Stop()
	select {
	case msg, ok := <-x.result:
		if !ok {
			return executionOutcome{code: "crashed", message: "runtime exited"}
		}
		return executionOutcome{result: msg}
	case <-timer.C:
		x.worker.kill()
		<-x.worker.done
		return executionOutcome{code: "timeout", message: "task execution deadline exceeded"}
	}
}
