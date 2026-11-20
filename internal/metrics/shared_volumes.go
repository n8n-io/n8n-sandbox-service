package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Shared volume publish results used with ObservePublish.
const (
	// SharedVolumeCreated is a publish that stored a new shared volume.
	SharedVolumeCreated = "created"
	// SharedVolumeExists is a publish of a digest that was already published.
	SharedVolumeExists = "exists"
	// SharedVolumeInvalidRequest is a publish with an invalid name.
	SharedVolumeInvalidRequest = "invalid_request"
	// SharedVolumeInvalidTar is a tar that breaks a volume tar rule.
	SharedVolumeInvalidTar = "invalid_tar"
	// SharedVolumeTooLarge is an upload or tar over a limit.
	SharedVolumeTooLarge = "too_large"
	// SharedVolumeDigestMismatch is an upload that does not hash to its digest.
	SharedVolumeDigestMismatch = "digest_mismatch"
	// SharedVolumeConflict is a name taken, a digest named otherwise, or a
	// publish of the same digest in progress.
	SharedVolumeConflict = "conflict"
	// SharedVolumeLimit is a publish refused by the shared volume limit or the
	// per-pod publish limit.
	SharedVolumeLimit = "limit"
	// SharedVolumeInterrupted is an upload the client broke off or stalled, or
	// a publish that lost its lease or was stopped by a shutdown.
	SharedVolumeInterrupted = "interrupted"
	// SharedVolumeError is a store or database failure.
	SharedVolumeError = "error"
	// SharedVolumeNoCapacity is a publish whose image build no runner took in
	// time.
	SharedVolumeNoCapacity = "no_capacity"
	// SharedVolumeBuildFailed is a publish whose image build failed.
	SharedVolumeBuildFailed = "build_failed"
)

// Shared volume build results used with ObserveBuild, besides the runner's
// reasons for a build that failed.
const (
	SharedVolumeBuildSuccess = "success"
	// SharedVolumeBuildRefused is a runner that refused for capacity.
	SharedVolumeBuildRefused = "refused"
	// SharedVolumeBuildUnsupported is a runner that can't build images.
	SharedVolumeBuildUnsupported = "unsupported"
	// SharedVolumeBuildTransport is a call to a runner that failed without a
	// runner's reason, or whose answer was not a valid image.
	SharedVolumeBuildTransport = "transport"
	// SharedVolumeBuildError is a store failure around a build.
	SharedVolumeBuildError = "error"
)

// publishBuckets covers a publish from a small tar, done in under a second,
// to several gigabytes uploaded and built.
var publishBuckets = []float64{0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600, 1800}

// SharedVolumeRecorder counts shared volume publishes on the API's registry.
// One built on a disabled APIRecorder is a no-op.
type SharedVolumeRecorder struct {
	publishes *prometheus.CounterVec
	duration  prometheus.Histogram
	builds    *prometheus.CounterVec
	swept     *prometheus.CounterVec
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
	builds := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   Namespace,
			Name:        "shared_volume_builds_total",
			Help:        "Calls to runners to build the image of a shared volume, labeled by result: success, refused, unsupported, transport, error or the runner's reason.",
			ConstLabels: prometheus.Labels{"role": RoleAPI},
		},
		[]string{"result"},
	)
	swept := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   Namespace,
			Name:        "shared_volume_sweep_removals_total",
			Help:        "Shared volumes the sweep removed with their objects, labeled by the state they were in: failed or deleted.",
			ConstLabels: prometheus.Labels{"role": RoleAPI},
		},
		[]string{"state"},
	)
	api.reg.MustRegister(publishes, duration, builds, swept)
	return &SharedVolumeRecorder{publishes: publishes, duration: duration, builds: builds, swept: swept}
}

// ObserveBuild records one call to a runner to build an image.
func (r *SharedVolumeRecorder) ObserveBuild(result string) {
	if r == nil || r.builds == nil {
		return
	}
	r.builds.WithLabelValues(result).Inc()
}

// BuildCount returns the counter value for builds with the result. Intended
// for tests in other packages.
func (r *SharedVolumeRecorder) BuildCount(result string) float64 {
	if r == nil || r.builds == nil {
		return 0
	}
	return testutil.ToFloat64(r.builds.WithLabelValues(result))
}

// ObserveSweepRemoval records a shared volume the sweep removed, by the state
// it was in.
func (r *SharedVolumeRecorder) ObserveSweepRemoval(state string) {
	if r == nil || r.swept == nil {
		return
	}
	r.swept.WithLabelValues(state).Inc()
}

// SweepRemovalCount returns the counter value for sweep removals of rows in
// the state. Intended for tests in other packages.
func (r *SharedVolumeRecorder) SweepRemovalCount(state string) float64 {
	if r == nil || r.swept == nil {
		return 0
	}
	return testutil.ToFloat64(r.swept.WithLabelValues(state))
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
