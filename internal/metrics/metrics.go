// Package metrics provides the Prometheus instrumentation shared by the API
// and runner binaries. Each binary builds its own recorder via NewAPIRecorder
// or NewRunnerRecorder; passing enabled=false returns a recorder whose
// observation methods are no-ops and whose Registry is nil.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Namespace is the prefix applied to every metric this package emits.
const Namespace = "sandbox"

// Role label values distinguish metrics emitted by the API vs the runner.
const (
	RoleAPI    = "api"
	RoleRunner = "runner"
)

// Operation label values used with ObserveSandboxOp / ObserveContainerOp.
const (
	OpCreate        = "create"
	OpDelete        = "delete"
	OpStop          = "stop"
	OpEnsureRunning = "ensure_running"
	OpEvict         = "evict"
)

// Handler returns the http.Handler that serves the registry's metrics in
// Prometheus exposition format.
func Handler(reg *prometheus.Registry) http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg})
}

// newRegistry constructs a registry pre-loaded with the standard Go runtime
// and process collectors.
func newRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return reg
}

// buildHTTPMetrics registers the shared HTTP request counter / latency
// histogram pair for the given role and returns them.
func buildHTTPMetrics(reg *prometheus.Registry, role string) (*prometheus.CounterVec, *prometheus.HistogramVec) {
	requests := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   Namespace,
			Name:        "http_requests_total",
			Help:        "Total HTTP requests handled, labeled by route pattern, method, and status code.",
			ConstLabels: prometheus.Labels{"role": role},
		},
		[]string{"route", "method", "status"},
	)
	duration := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace:   Namespace,
			Name:        "http_request_duration_seconds",
			Help:        "Duration of HTTP requests in seconds, labeled by route pattern and method.",
			Buckets:     prometheus.DefBuckets,
			ConstLabels: prometheus.Labels{"role": role},
		},
		[]string{"route", "method"},
	)
	reg.MustRegister(requests, duration)
	return requests, duration
}

// APIRecorder owns the metric instruments emitted by the API binary.
//
// A recorder built with NewAPIRecorder(false) is a no-op: every observation
// method returns immediately and Registry() returns nil.
type APIRecorder struct {
	reg          *prometheus.Registry
	httpRequests *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec
	sandboxOps   *prometheus.CounterVec
}

// NewAPIRecorder builds the API recorder. If enabled is false, the returned
// recorder discards all observations and exposes a nil Registry.
func NewAPIRecorder(enabled bool) *APIRecorder {
	if !enabled {
		return &APIRecorder{}
	}
	reg := newRegistry()
	httpReq, httpDur := buildHTTPMetrics(reg, RoleAPI)
	sandboxOps := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   Namespace,
			Name:        "sandbox_operations_total",
			Help:        "Sandbox lifecycle operations performed by the API, labeled by operation and result.",
			ConstLabels: prometheus.Labels{"role": RoleAPI},
		},
		[]string{"operation", "result"},
	)
	reg.MustRegister(sandboxOps)
	return &APIRecorder{
		reg:          reg,
		httpRequests: httpReq,
		httpDuration: httpDur,
		sandboxOps:   sandboxOps,
	}
}

// Registry returns the underlying Prometheus registry, or nil if disabled.
func (r *APIRecorder) Registry() *prometheus.Registry { return r.reg }

// Enabled reports whether metrics observations are active.
func (r *APIRecorder) Enabled() bool { return r.reg != nil }

// ObserveHTTP records a finished HTTP request.
func (r *APIRecorder) ObserveHTTP(route, method string, status int, dur time.Duration) {
	if r.reg == nil {
		return
	}
	r.httpRequests.WithLabelValues(route, method, strconv.Itoa(status)).Inc()
	r.httpDuration.WithLabelValues(route, method).Observe(dur.Seconds())
}

// ObserveSandboxOp records a sandbox lifecycle operation result.
func (r *APIRecorder) ObserveSandboxOp(operation string, success bool) {
	if r.reg == nil {
		return
	}
	r.sandboxOps.WithLabelValues(operation, resultLabel(success)).Inc()
}

// SetActiveSandboxes registers a scrape-time gauge that calls f to read the
// current sandbox count. Safe to call exactly once per recorder.
func (r *APIRecorder) SetActiveSandboxes(f func() float64) {
	if r.reg == nil {
		return
	}
	r.reg.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Namespace:   Namespace,
			Name:        "sandboxes_active",
			Help:        "Current number of sandboxes tracked by the API store.",
			ConstLabels: prometheus.Labels{"role": RoleAPI},
		},
		f,
	))
}

// SetRunnersRegistered registers a scrape-time gauge that calls f to read the
// current count of registered runners.
func (r *APIRecorder) SetRunnersRegistered(f func() float64) {
	if r.reg == nil {
		return
	}
	r.reg.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Namespace:   Namespace,
			Name:        "runners_registered",
			Help:        "Current number of runners registered with the API.",
			ConstLabels: prometheus.Labels{"role": RoleAPI},
		},
		f,
	))
}

// containerOpBuckets sizes the runner's container-op latency histogram for the
// observed range: container creation is slow (image pull, dind warm-up) and
// can take tens of seconds in cold cases, so the upper bound runs to 120s.
var containerOpBuckets = []float64{0.5, 1, 2, 5, 10, 30, 60, 120}

// RunnerRecorder owns the metric instruments emitted by the runner binary.
type RunnerRecorder struct {
	reg                 *prometheus.Registry
	httpRequests        *prometheus.CounterVec
	httpDuration        *prometheus.HistogramVec
	containerOps        *prometheus.CounterVec
	containerOpDuration *prometheus.HistogramVec
}

// NewRunnerRecorder builds the runner recorder. If enabled is false, the
// returned recorder discards all observations and exposes a nil Registry.
func NewRunnerRecorder(enabled bool) *RunnerRecorder {
	if !enabled {
		return &RunnerRecorder{}
	}
	reg := newRegistry()
	httpReq, httpDur := buildHTTPMetrics(reg, RoleRunner)
	containerOps := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace:   Namespace,
			Name:        "container_operations_total",
			Help:        "Container lifecycle operations performed by the runner, labeled by operation and result.",
			ConstLabels: prometheus.Labels{"role": RoleRunner},
		},
		[]string{"operation", "result"},
	)
	containerOpDuration := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace:   Namespace,
			Name:        "container_operation_duration_seconds",
			Help:        "Duration of runner container lifecycle operations in seconds, labeled by operation.",
			Buckets:     containerOpBuckets,
			ConstLabels: prometheus.Labels{"role": RoleRunner},
		},
		[]string{"operation"},
	)
	reg.MustRegister(containerOps, containerOpDuration)
	return &RunnerRecorder{
		reg:                 reg,
		httpRequests:        httpReq,
		httpDuration:        httpDur,
		containerOps:        containerOps,
		containerOpDuration: containerOpDuration,
	}
}

// Registry returns the underlying Prometheus registry, or nil if disabled.
func (r *RunnerRecorder) Registry() *prometheus.Registry { return r.reg }

// Enabled reports whether metrics observations are active.
func (r *RunnerRecorder) Enabled() bool { return r.reg != nil }

// ObserveHTTP records a finished HTTP request.
func (r *RunnerRecorder) ObserveHTTP(route, method string, status int, dur time.Duration) {
	if r.reg == nil {
		return
	}
	r.httpRequests.WithLabelValues(route, method, strconv.Itoa(status)).Inc()
	r.httpDuration.WithLabelValues(route, method).Observe(dur.Seconds())
}

// ObserveContainerOp records the outcome and duration of a container
// lifecycle operation.
func (r *RunnerRecorder) ObserveContainerOp(operation string, success bool, dur time.Duration) {
	if r.reg == nil {
		return
	}
	r.containerOps.WithLabelValues(operation, resultLabel(success)).Inc()
	r.containerOpDuration.WithLabelValues(operation).Observe(dur.Seconds())
}

// SetActiveContainers registers a scrape-time gauge that calls f to read the
// current count of sandbox containers tracked by the runner.
func (r *RunnerRecorder) SetActiveContainers(f func() float64) {
	if r.reg == nil {
		return
	}
	r.reg.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Namespace:   Namespace,
			Name:        "containers_active",
			Help:        "Current number of slot-blocking sandboxes on the runner.",
			ConstLabels: prometheus.Labels{"role": RoleRunner},
		},
		f,
	))
}

// SetStoppedContainers registers a scrape-time gauge for managed stopped sandboxes.
func (r *RunnerRecorder) SetStoppedContainers(f func() float64) {
	if r.reg == nil {
		return
	}
	r.reg.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Namespace:   Namespace,
			Name:        "containers_stopped",
			Help:        "Current number of managed stopped sandboxes not occupying a concurrent slot.",
			ConstLabels: prometheus.Labels{"role": RoleRunner},
		},
		f,
	))
}

// ContainerOpCount returns the counter value for a runner container operation.
// Intended for tests in other packages.
func (r *RunnerRecorder) ContainerOpCount(operation string, success bool) float64 {
	if r.reg == nil {
		return 0
	}
	return testutil.ToFloat64(r.containerOps.WithLabelValues(operation, resultLabel(success)))
}

func resultLabel(success bool) string {
	if success {
		return "success"
	}
	return "error"
}
