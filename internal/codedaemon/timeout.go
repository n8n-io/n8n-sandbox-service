// This file loads execution limits and enforces task deadlines.
package codedaemon

import (
	"errors"
	"time"

	"github.com/n8n-io/sandbox-service/internal/envvar"
)

const (
	defaultTaskTimeout = time.Minute
	maxTaskTimeout     = 24 * time.Hour // Hard cap on configured execution limits.
)

// loadTaskTimeouts returns the default and maximum execution durations. A lower
// configured maximum also lowers the default unless the default is set explicitly.
func loadTaskTimeouts() (time.Duration, time.Duration, error) {
	upper := maxTaskTimeout
	if err := envvar.DurationBetween("CODE_DAEMON_MAX_TASK_TIMEOUT", &upper, time.Millisecond, maxTaskTimeout); err != nil {
		return 0, 0, err
	}
	value := min(defaultTaskTimeout, upper)
	if err := envvar.DurationBetween("CODE_DAEMON_DEFAULT_TASK_TIMEOUT", &value, time.Millisecond, maxTaskTimeout); err != nil {
		return 0, 0, err
	}
	if value > upper {
		return 0, 0, errors.New("default task timeout exceeds maximum task timeout")
	}
	return value, upper, nil
}

// taskTimeout lets a request shorten, but never extend, the service limit.
func (d *Daemon) taskTimeout(requestMillis int64) time.Duration {
	limit := min(d.cfg.DefaultTimeout, d.cfg.MaxTimeout)
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
