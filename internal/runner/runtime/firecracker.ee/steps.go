package firecracker

import (
	"context"
	"time"

	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/obs"
)

// Step names recorded for a create or a wake. They are the label values of
// sandbox_lifecycle_step_duration_seconds and the field names on the create and
// wake events, so they are fixed strings rather than derived from anything.
const (
	stepCloneRootfs   = "clone_rootfs"
	stepCloneSnapshot = "clone_snapshot"
	stepPrepareJail   = "prepare_jail"
	stepSetupNetwork  = "setup_network"
	stepStartJailer   = "start_jailer"
	stepWaitSocket    = "wait_socket"
	stepLoadSnapshot  = "load_snapshot"
	stepColdBoot      = "cold_boot"
	stepStartProxy    = "start_proxy"
	stepProbeDaemon   = "probe_daemon"
)

// stepTimer measures each phase of one create or wake. The durations go to the
// per-step histogram, for fleet-wide percentiles, and to the event the runtime
// logs when the operation finishes, which is where a single slow create can be
// taken apart after the fact.
type stepTimer struct {
	op    string
	rec   *metrics.RunnerRecorder
	start time.Time
	attrs []any
}

func newStepTimer(op string, rec *metrics.RunnerRecorder) *stepTimer {
	return &stepTimer{op: op, rec: rec, start: time.Now()}
}

// step runs fn, recording how long it took under name. The error is returned
// untouched so callers keep their own wrapping.
func (t *stepTimer) step(name string, fn func() error) error {
	if t == nil {
		return fn()
	}
	stepStart := time.Now()
	err := fn()
	dur := time.Since(stepStart)
	t.attrs = append(t.attrs, name+"_ms", dur.Milliseconds())
	t.rec.ObserveLifecycleStep(t.op, name, dur)
	return err
}

// attrsFor returns the log attributes for the finished operation: the trace id
// that ties this event to the API's event for the same request, the operation,
// its total duration, and every step measured along the way.
func (t *stepTimer) attrsFor(ctx context.Context) []any {
	if t == nil {
		return nil
	}
	attrs := []any{
		"trace_id", obs.TraceID(ctx),
		"op", t.op,
		"total_ms", time.Since(t.start).Milliseconds(),
	}
	return append(attrs, t.attrs...)
}
