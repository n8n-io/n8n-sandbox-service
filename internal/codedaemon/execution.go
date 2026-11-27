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

func newExecution(w *worker, id string, deadline time.Time) *execution {
	return &execution{worker: w, id: id, result: make(chan workerMessage, 1), deadline: deadline}
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

// deliver keeps the first terminal message and drops any later one. The
// worker calls it with its lock held, so it cannot race with workerExited.
func (x *execution) deliver(msg workerMessage) {
	select {
	case x.result <- msg:
	default:
	}
}

// workerExited tells watch that no result will come. The worker calls it once,
// with its lock held, after the control reader has delivered any final message.
func (x *execution) workerExited() {
	close(x.result)
}

// close unregisters the result receiver without stopping the shared runtime.
func (x *execution) close() {
	x.worker.unregister(x.id)
}
