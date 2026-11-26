// This file tracks one task dispatched to the shared runtime and enforces
// its deadline.

package codedaemon

import "time"

// execution is one task dispatched to the shared runtime. It receives the
// task's "done" or "error" message until close unregisters it.
type execution struct {
	worker   *worker
	id       string
	result   chan workerMessage // closed on worker failure; any buffered result is still readable
	deadline time.Time          // Set at dispatch, after the worker is ready.
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

// close unregisters the result receiver without stopping the shared runtime.
func (x *execution) close() {
	x.worker.mu.Lock()
	defer x.worker.mu.Unlock()
	delete(x.worker.executionsByTaskID, x.id)
}
