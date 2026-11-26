// This file starts the shared runtime on demand, stops it on Close, and
// dispatches tasks to it.

package codedaemon

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// supervisor owns the shared runtime. It starts the worker on first use and
// never restarts it.
type supervisor struct {
	cfg Config

	workerMu       sync.Mutex
	startupTimeout time.Duration // how long a started worker has to report ready
	worker         *worker       // started lazily and never replaced once set
	startFailed    bool          // set when the worker could not be started; it is not retried
	closed         bool          // prevents starting a worker after Close
}

func newSupervisor(cfg Config) *supervisor {
	return &supervisor{cfg: cfg, startupTimeout: workerStartupTimeout}
}

// isHealthy is true before the worker starts. It is false once closed, the
// worker fails to start, retires, or stops.
func (s *supervisor) isHealthy() bool {
	s.workerMu.Lock()
	defer s.workerMu.Unlock()
	return !s.closed && !s.startFailed && (s.worker == nil || s.worker.acceptsTasks())
}

// requestCancel asks the worker to cancel a running task. It returns false if
// no worker runs the task.
func (s *supervisor) requestCancel(id string) bool {
	s.workerMu.Lock()
	w := s.worker
	s.workerMu.Unlock()
	return w != nil && w.requestCancel(id)
}

// getWorker starts the shared runtime on demand and waits until it is ready to accept commands.
func (s *supervisor) getWorker(ctx context.Context) (*worker, error) {
	s.workerMu.Lock()
	if s.closed || s.startFailed {
		s.workerMu.Unlock()
		return nil, errWorkerUnavailable
	}
	if s.worker == nil {
		w, err := startWorker(s.cfg, s.startupTimeout)
		if err != nil {
			// Like a runtime that stops, a failed start is not retried.
			s.startFailed = true
			s.workerMu.Unlock()
			return nil, fmt.Errorf("start runtime: %w", err)
		}
		s.worker = w
	}
	w := s.worker
	s.workerMu.Unlock()
	select {
	case <-w.stopped:
		return nil, errWorkerUnavailable
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-w.ready:
		if w.isStopped() {
			return nil, errWorkerUnavailable
		}
		return w, nil
	}
}

// close prevents worker startup and waits for any existing worker to exit and be reaped.
func (s *supervisor) close() {
	s.workerMu.Lock()
	s.closed = true
	w := s.worker
	s.workerMu.Unlock()
	if w != nil {
		w.kill()
		<-w.done
	}
}

// dispatch registers a result receiver before sending the start command to the shared runtime.
// It calls markDispatched after registration and before the send, so the task
// ID stays reserved even if the send fails.
func (s *supervisor) dispatch(ctx context.Context, id, dir string, timeout time.Duration, markDispatched func(id string)) (*execution, error) {
	w, err := s.getWorker(ctx)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	x, err := w.register(id, time.Now().Add(timeout))
	if err != nil {
		return nil, err
	}
	markDispatched(id)
	if err := w.send(workerMessage{Type: messageTypeStart, TaskID: id, Dir: dir, Deadline: x.deadline.UnixMilli()}, time.Now().Add(time.Second)); err != nil {
		w.kill() // a partial command leaves the shared protocol unusable
		x.close()
		return nil, err
	}
	return x, nil
}
