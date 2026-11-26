// This file starts the shared runtime on demand, stops it on Close, and
// dispatches tasks to it.

package codedaemon

import (
	"context"
	"fmt"
	"time"
)

// getWorker starts the shared runtime on demand and waits until it is ready to accept commands.
func (d *Daemon) getWorker(ctx context.Context) (*worker, error) {
	d.workerMu.Lock()
	if d.closed || d.startFailed {
		d.workerMu.Unlock()
		return nil, errWorkerUnavailable
	}
	if d.worker == nil {
		w, err := startWorker(d.cfg, d.startupTimeout)
		if err != nil {
			// Like a runtime that stops, a failed start is not retried.
			d.startFailed = true
			d.workerMu.Unlock()
			return nil, fmt.Errorf("start runtime: %w", err)
		}
		d.worker = w
	}
	w := d.worker
	d.workerMu.Unlock()
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

// Close prevents worker startup and waits for any existing worker to exit and be reaped.
func (d *Daemon) Close() {
	d.workerMu.Lock()
	d.closed = true
	w := d.worker
	d.workerMu.Unlock()
	if w != nil {
		w.kill()
		<-w.done
	}
}

// dispatch registers a result receiver before sending the start command to the shared runtime.
func (d *Daemon) dispatch(ctx context.Context, id, dir string, timeout time.Duration) (*execution, error) {
	w, err := d.getWorker(ctx)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	x := &execution{worker: w, id: id, result: make(chan workerMessage, 1), deadline: time.Now().Add(timeout)}
	w.mu.Lock()
	if w.isStopped() {
		w.mu.Unlock()
		return nil, errWorkerUnavailable
	}
	if _, exists := w.executionsByTaskID[id]; exists {
		w.mu.Unlock()
		return nil, errDuplicateTask
	}
	w.executionsByTaskID[id] = x
	w.mu.Unlock()
	d.markDispatched(id)
	if err := w.send(workerMessage{Type: "start", TaskID: id, Dir: dir, Deadline: x.deadline.UnixMilli()}); err != nil {
		w.kill() // a partial command leaves the shared protocol unusable
		x.close()
		return nil, err
	}
	return x, nil
}
