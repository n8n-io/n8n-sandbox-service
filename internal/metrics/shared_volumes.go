// This file defines the metrics of shared volumes.

package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Shared volume publish results used with ObservePublish.
const (
	// SharedVolumePublishCreated is a publish that stored a new shared volume.
	SharedVolumePublishCreated = "created"
	// SharedVolumePublishExists is a publish of a digest that was already published.
	SharedVolumePublishExists = "exists"
	// SharedVolumePublishInvalidRequest is a publish with an invalid name.
	SharedVolumePublishInvalidRequest = "invalid_request"
	// SharedVolumePublishInvalidTar is a tar that breaks a volume tar rule.
	SharedVolumePublishInvalidTar = "invalid_tar"
	// SharedVolumePublishTooLarge is an upload or tar over a limit.
	SharedVolumePublishTooLarge = "too_large"
	// SharedVolumePublishDigestMismatch is an upload that does not hash to its digest.
	SharedVolumePublishDigestMismatch = "digest_mismatch"
	// SharedVolumePublishConflict is a name taken, a digest named otherwise, or a
	// publish of the same digest in progress.
	SharedVolumePublishConflict = "conflict"
	// SharedVolumePublishLimit is a publish refused by the shared volume limit or the
	// per-pod publish limit.
	SharedVolumePublishLimit = "limit"
	// SharedVolumePublishInterrupted is an upload the client broke off or stalled, or
	// a publish that lost its lease or was stopped by a shutdown.
	SharedVolumePublishInterrupted = "interrupted"
	// SharedVolumePublishError is a store or database failure.
	SharedVolumePublishError = "error"
)

// publishBuckets covers a publish from a small tar, done in under a second,
// to several gigabytes uploaded and built.
var publishBuckets = []float64{0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600, 1800}

// SharedVolumeRecorder counts shared volume publishes on the API's registry.
// One built on a disabled APIRecorder is a no-op.
type SharedVolumeRecorder struct {
	publishes *prometheus.CounterVec
	duration  prometheus.Histogram
}

// NewSharedVolumeRecorder registers the shared volume metrics on api's
// registry. Call it once per APIRecorder.
func NewSharedVolumeRecorder(api *APIRecorder) *SharedVolumeRecorder {
	if !api.Enabled() {
		return &SharedVolumeRecorder{}
	}
	publishes := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   Namespace,
			Name:        "shared_volume_publishes_total",
			Help:        "Shared volume publishes (PUT /shared-volumes/…), labeled by result.",
			ConstLabels: prometheus.Labels{"role": RoleAPI},
		},
		[]string{"result"},
	)
	duration := prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace:   Namespace,
		Name:        "shared_volume_publish_duration_seconds",
		Help:        "Duration of publishes that stored a new shared volume, from the request to the volume being ready.",
		Buckets:     publishBuckets,
		ConstLabels: prometheus.Labels{"role": RoleAPI},
	})
	api.reg.MustRegister(publishes, duration)
	return &SharedVolumeRecorder{publishes: publishes, duration: duration}
}

// ObservePublish records a publish's result.
func (r *SharedVolumeRecorder) ObservePublish(result string) {
	if r == nil || r.publishes == nil {
		return
	}
	r.publishes.WithLabelValues(result).Inc()
}

// ObservePublishDuration records how long a publish that created a shared
// volume took.
func (r *SharedVolumeRecorder) ObservePublishDuration(duration time.Duration) {
	if r == nil || r.duration == nil {
		return
	}
	r.duration.Observe(duration.Seconds())
}

// PublishCount returns the counter value for publishes with the result.
// Intended for tests in other packages.
func (r *SharedVolumeRecorder) PublishCount(result string) float64 {
	if r == nil || r.publishes == nil {
		return 0
	}
	return testutil.ToFloat64(r.publishes.WithLabelValues(result))
}
