// This file tracks one task dispatched to the shared runtime. It enforces the
// task's deadline and stops the task on cancellation.

package codedaemon

import (
	"context"
	"time"
)

// cancelGrace covers sending the cancel command and waiting for the runtime's reply.
const cancelGrace = 5 * time.Second

// execution is one task dispatched to the shared runtime. It receives the
// task's "done" or "error" message until close unregisters it.
type execution struct {
	worker          *worker
	id              string
	result          chan workerMessage // closed on worker failure; any buffered result is still readable
	deadline        time.Time          // Set at dispatch, after the worker is ready.
	cancelRequested chan struct{}      // closed when an explicit cancel is requested; protected by worker.mu
	cancelAck       chan struct{}      // buffers the runtime's acknowledgement without blocking the reader
	finished        bool               // set when the runtime reports the task's end; protected by worker.mu
}

func newExecution(w *worker, id string, deadline time.Time) *execution {
	return &execution{
		worker:          w,
		id:              id,
		result:          make(chan workerMessage, 1),
		deadline:        deadline,
		cancelRequested: make(chan struct{}),
		cancelAck:       make(chan struct{}, 1),
	}
}

type executionOutcome struct {
	result        workerMessage // Terminal runtime reply, including a possible user error.
	code, message string        // Daemon error details; when code is set, result is unused.
}

// watch waits for the runtime reply, or stops the task at the deadline, on an
// explicit cancel, or when ctx ends. It runs independently of HTTP writes, so
// a slow client cannot postpone a timeout or a cancellation.
func (x *execution) watch(ctx context.Context) executionOutcome {
	timer := time.NewTimer(time.Until(x.deadline))
	defer timer.Stop()
	var stopped executionOutcome
	select {
	case msg, ok := <-x.result:
		return resultOutcome(msg, ok)
	case <-timer.C:
		stopped = executionOutcome{code: "timeout", message: "task execution deadline exceeded"}
	case <-x.cancelRequested:
		stopped = executionOutcome{code: "cancelled", message: "task cancelled"}
	case <-ctx.Done():
		stopped = executionOutcome{code: "cancelled", message: "task stream closed"}
	}
	// Select picks at random among ready cases. Keep a result that arrived
	// together with the stop signal.
	select {
	case msg, ok := <-x.result:
		return resultOutcome(msg, ok)
	default:
	}
	// A task that finished during the grace period reports its result.
	if msg, ok := x.stop(); ok {
		return executionOutcome{result: msg}
	}
	return stopped
}

func resultOutcome(msg workerMessage, ok bool) executionOutcome {
	if !ok {
		return executionOutcome{code: "crashed", message: "runtime exited"}
	}
	return executionOutcome{result: msg}
}

// stop retires the worker and sends it a cancel command. If the runtime
// neither acknowledges it nor finishes the task in time, stop kills the shared
// worker. It returns the task's terminal message when the task finished during
// the grace period.
func (x *execution) stop() (workerMessage, bool) {
	w := x.worker
	w.retire()
	deadline := time.Now().Add(cancelGrace)
	if w.send(workerMessage{Type: messageTypeCancel, TaskID: x.id}, deadline) == nil {
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		select {
		case <-x.cancelAck:
			return workerMessage{}, false
		case msg, ok := <-x.result:
			return msg, ok // the worker stays retired
		case <-w.done:
			return workerMessage{}, false
		case <-timer.C:
		}
	}
	w.kill()
	<-w.done
	return workerMessage{}, false
}

// deliver keeps the first terminal message and drops any later one. The
// worker calls it with its lock held, so it cannot race with workerExited.
func (x *execution) deliver(msg workerMessage) {
	x.finished = true
	select {
	case x.result <- msg:
	default:
	}
}

// acknowledgeCancel records the runtime's cancel reply. The worker calls it
// with its lock held.
func (x *execution) acknowledgeCancel() {
	select {
	case x.cancelAck <- struct{}{}:
	default:
	}
}

// requestCancel signals watch to stop the task. The worker calls it with its
// lock held. It returns true only for the first request.
func (x *execution) requestCancel() bool {
	select {
	case <-x.cancelRequested:
		return false
	default:
		close(x.cancelRequested)
		return true
	}
}

// workerExited tells watch that no result will come. The worker calls it once,
// with its lock held, after the control reader has delivered any final message.
func (x *execution) workerExited() {
	close(x.result)
}

// close unregisters the result receiver. It stops a retired worker after its
// last task leaves.
func (x *execution) close() {
	x.worker.unregister(x.id)
}
