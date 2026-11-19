// Package runtime defines the Runtime contract a sandbox backend implements and
// the types shared by the Docker and Firecracker implementations.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/n8n-io/sandbox-service/internal/daemon/volumebuild"
)

// CreateBudget is how long a runtime may spend on CreateSandbox. It is part of
// the contract because the API derives its create RPC deadline from it: giving
// up earlier than the runtime would leave a sandbox that finishes creating with
// no store row. Firecracker enforces it itself, detached from the caller;
// Docker inherits the caller's deadline.
const CreateBudget = 3 * time.Minute

// TransitionBudget is how long a runtime may spend on stop, wake, or delete.
// The idle sweeper derives its RPC deadline from it.
const TransitionBudget = 2 * time.Minute

// DiskFloorPercent is the free space, as a percentage of a disk, that new
// sandboxes, volume builds and volume downloads leave alone. New sandboxes take almost no
// space at first and grow as they write, so the floor is room for the
// sandboxes already there to grow into.
const DiskFloorPercent = 5

// ErrSandboxNotFound is returned when a sandbox ID is not found.
var ErrSandboxNotFound = errors.New("sandbox not found")

// ErrSandboxNetworkUnavailable is returned when a sandbox exists but has no
// reachable network attachment yet.
var ErrSandboxNetworkUnavailable = errors.New("sandbox network unavailable")

// ErrSandboxNotRunning is returned when a sandbox exists but is not running.
var ErrSandboxNotRunning = errors.New("sandbox not running")

// ErrCapacityExhausted is returned when CreateSandbox refuses a sandbox for want
// of capacity. The refusal comes before anything is created, which is what lets
// the API place the same sandbox on another runner.
var ErrCapacityExhausted = errors.New("runner capacity exhausted")

// ErrVolumesUnsupported is returned when CreateSandbox refuses volumes this
// runner can't attach, before anything is created.
var ErrVolumesUnsupported = errors.New("runner can't attach these volumes")

// Egress is a sandbox's outbound network policy.
type Egress string

const (
	// EgressPublic allows the public internet; the private ranges in netpolicy stay
	// blocked. Default when field is omitted or empty.
	EgressPublic Egress = "public"
	// EgressNone lets no guest-initiated connection leave the sandbox, DNS included.
	EgressNone Egress = "none"
	// EgressProxy lets the guest reach only the runner's egress proxy, which
	// enforces the sandbox's EgressProxySpec. No runner serves it yet.
	EgressProxy Egress = "proxy"
)

// ParseEgress maps the wire value to an Egress, treating empty as EgressPublic.
func ParseEgress(s string) (Egress, error) {
	switch Egress(s) {
	case "", EgressPublic:
		return EgressPublic, nil
	case EgressNone:
		return EgressNone, nil
	case EgressProxy:
		return EgressProxy, nil
	}
	return "", fmt.Errorf("invalid egress %q: must be %q, %q or %q", s, EgressPublic, EgressNone, EgressProxy)
}

// DefaultTemplate is the template a create that names none gets: the runner's
// one rootfs or snapshot. On the wire it is sent as an empty Template, so a
// runner from before templates confirms it.
const DefaultTemplate = "default"

// CreateOptions holds optional parameters for sandbox creation. It is also the
// JSON shape of the create RPC's create_options_json field; the echo in
// applied_create_options_json is an AppliedCreateOptions.
type CreateOptions struct {
	Egress  Egress  `json:"egress,omitempty"`
	Volumes Volumes `json:"volumes,omitempty"`
	// Template is the image the sandbox boots; empty is DefaultTemplate.
	Template string `json:"template,omitempty"`
	// EgressProxy is set exactly when Egress is EgressProxy.
	EgressProxy *EgressProxySpec `json:"egress_proxy,omitempty"`
	// Tenant owns the sandbox; set by the API with EgressProxy only. Runners
	// otherwise know nothing of tenants: this scopes the secrets the proxy
	// reads and attributes the usage it meters.
	Tenant string `json:"tenant,omitempty"`
}

// Equal reports whether o and p ask for the same sandbox. Both must have come
// through ParseCreateOptions or been built from validated values.
func (o *CreateOptions) Equal(p *CreateOptions) bool {
	a, errA := json.Marshal(o)
	b, errB := json.Marshal(p)
	return errA == nil && errB == nil && string(a) == string(b)
}

// ParseCreateOptions decodes the wire form of CreateOptions. Empty means the
// defaults; otherwise it must be an object with a valid Egress, valid volumes,
// and an EgressProxy spec exactly when Egress is EgressProxy.
func ParseCreateOptions(s string) (*CreateOptions, error) {
	opts := &CreateOptions{}
	if strings.TrimSpace(s) != "" {
		// Via &opts so a JSON null shows up as nil rather than as the defaults.
		if err := json.Unmarshal([]byte(s), &opts); err != nil {
			return nil, err
		}
		if opts == nil {
			return nil, errors.New("create options: null")
		}
	}
	egress, err := ParseEgress(string(opts.Egress))
	if err != nil {
		return nil, err
	}
	opts.Egress = egress
	if err := validateVolumes(opts.Volumes); err != nil {
		return nil, err
	}
	if len(opts.Volumes) == 0 {
		opts.Volumes = nil
	}
	if opts.Template == DefaultTemplate {
		opts.Template = ""
	}
	if opts.Template != "" {
		if err := ValidateTemplateName(opts.Template); err != nil {
			return nil, err
		}
	}
	if opts.Tenant != "" && (egress != EgressProxy || !tenantRe.MatchString(opts.Tenant)) {
		return nil, errors.New("create options: tenant is only set, well formed, with egress proxy")
	}
	switch {
	case egress == EgressProxy && opts.Tenant == "":
		return nil, errors.New("egress proxy needs the owning tenant")
	case egress == EgressProxy && opts.EgressProxy == nil:
		return nil, errors.New("egress proxy needs egress_proxy")
	case egress != EgressProxy && opts.EgressProxy != nil:
		return nil, fmt.Errorf("egress_proxy is only allowed with egress %q", EgressProxy)
	case opts.EgressProxy != nil:
		if err := opts.EgressProxy.Validate(); err != nil {
			return nil, err
		}
	}
	return opts, nil
}

var tenantRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// BlockEgress reports whether the sandbox gets no direct egress at all. Options
// reach a runtime through ParseCreateOptions, so any other value is a bug, not
// input. A proxy sandbox is blocked too: whatever it may reach goes through
// the proxy, never straight out.
func (o *CreateOptions) BlockEgress() bool {
	if o == nil {
		return false
	}
	switch o.Egress {
	case "", EgressPublic:
		return false
	case EgressNone, EgressProxy:
		return true
	}
	panic(fmt.Sprintf("unvalidated egress %q", o.Egress))
}

// EgressProxyHost is a Runtime that can put sandboxes behind the runner's
// egress proxy: an egress-proxy sandbox's only route out is to a proxy
// endpoint the runtime serves.
type EgressProxyHost interface {
	// ServeEgressProxy serves the proxy to egress-proxy sandboxes until ctx
	// ends. handler is called once per listener with the function naming the
	// sandbox a connection comes from.
	ServeEgressProxy(ctx context.Context, handler func(identify func(remoteAddr string) (string, bool)) http.Handler) error
	// EgressProxyURL is the URL sandboxID's guest reaches the proxy at.
	EgressProxyURL(sandboxID string) (string, bool)
}

// Archiver is a Runtime that can move a sandbox's disk out and back: an
// archive holds its files, not its memory.
type Archiver interface {
	// ExportSandbox writes stopped sandboxID's disk to w, in a format of the
	// runtime's own; it fails for a running sandbox.
	ExportSandbox(ctx context.Context, sandboxID string, w io.Writer) error
	// ImportSandbox creates sandboxID from an export and starts it with a cold
	// boot, as CreateSandbox would with opts.
	ImportSandbox(ctx context.Context, sandboxID string, opts *CreateOptions, r io.Reader) (*SandboxInfo, error)
}

// SandboxInfo represents information about a created sandbox.
type SandboxInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	IP   string `json:"ip"`

	// VolumesLeftOut maps the index in CreateOptions.Volumes of each optional
	// volume the sandbox did not get to why, a VolumeLeftOut value. Only
	// CreateSandbox sets it.
	VolumesLeftOut map[int]string `json:"-"`
}

// WakeResult reports how a sandbox came back.
type WakeResult struct {
	// Recovered marks a sandbox that came back through crash recovery rather than
	// an ordinary wake from an idle stop. Its files are intact, but everything that
	// was in memory is gone — processes an earlier request started, and the
	// daemon's in-memory execution history — and none of that is visible in a
	// healthy-looking sandbox. So the request that triggered the recovery is failed
	// with 409 sandbox_restarted instead of proxied, which is the only point at
	// which the loss can be reported. An ordinary wake stays transparent.
	//
	// It describes what the wake was, not that it worked: alongside a non-nil error
	// it means a recovery was attempted and failed, which is what lets a caller
	// meter recoveries apart from ordinary wakes. Check the error first.
	Recovered bool
}

// Capacity reports concurrent slot usage and optionally how many managed
// sandboxes are stopped (not slot-blocking).
type Capacity struct {
	Used    int32 // slot-blocking sandboxes (Firecracker: running microVMs; Docker: all managed containers, stopped included)
	Total   int32
	Stopped int32 // managed but not slot-blocking (Firecracker stopped snapshots)
}

// DiskReserver is a runtime whose creates and volume builds leave data volume
// space free for something else on the runner: the volume cache's unused
// budget.
type DiskReserver interface {
	SetDiskReservation(reserved func() int64)
}

// VolumeCache keeps the volume objects a runtime attaches on the runner's
// disk: the volume cache.
type VolumeCache interface {
	// Acquire returns the path of a verified copy of vol's object and pins it
	// for holder. ctx bounds only the wait. An error wrapping
	// ErrCapacityExhausted is a refusal for want of room.
	Acquire(ctx context.Context, vol Volume, holder string) (string, error)
	// ReleaseAll unpins every object holder holds.
	ReleaseAll(holder string)
}

// VolumeAttacher is a runtime that attaches volumes to the sandboxes it
// creates. A runtime that isn't refuses a create that names any.
type VolumeAttacher interface {
	SetVolumeCache(cache VolumeCache)
}

// TemplateInfo is a template a runtime boots sandboxes from, with what one
// sandbox of it gets.
type TemplateInfo struct {
	Name     string
	VCPUs    int32
	MemoryMB int64
	// DiskMB is the sandbox's disk: the rootfs on Firecracker, the
	// writable-layer quota on Docker (0 when none is enforced).
	DiskMB int64
	// Version identifies the build of the template's image. An archive
	// restores only onto the version it was made from.
	Version string
}

// ServesTemplate reports whether templates includes name ("" is the default).
func ServesTemplate(templates []TemplateInfo, name string) bool {
	if name == "" {
		name = DefaultTemplate
	}
	for _, t := range templates {
		if t.Name == name {
			return true
		}
	}
	return false
}

// Runtime is the sandbox backend contract used by the shared runner process.
type Runtime interface {
	Prepare(ctx context.Context)
	Ready(ctx context.Context) error
	ReadyCh() <-chan struct{}
	Capacity(ctx context.Context) (Capacity, error)
	// Templates lists the templates the runtime serves, the default first.
	// Each has passed admission once Ready returns nil.
	Templates() []TemplateInfo

	CreateSandbox(ctx context.Context, sandboxID string, opts *CreateOptions) (*SandboxInfo, error)
	GetSandboxInfo(ctx context.Context, sandboxID string) (*SandboxInfo, error)
	// DeleteSandbox bounds itself by TransitionBudget, so callers may pass a
	// context without a deadline.
	DeleteSandbox(ctx context.Context, sandboxID string) error
	StopSandbox(ctx context.Context, sandboxID string) error
	EnsureSandboxRunning(ctx context.Context, sandboxID string) (WakeResult, error)
	DaemonURL(ctx context.Context, sandboxID string) (string, error)

	Shutdown(ctx context.Context)
}

// VolumeBuilder is the part of a Runtime that builds volume images. Only the
// Firecracker runtime has it.
type VolumeBuilder interface {
	// BuildVolume builds an ext4 image from the tar and hands it to deliver.
	// The image is gone once BuildVolume returns. params must be valid, and
	// tar must end after exactly params.TarBytes.
	//
	// It refuses before reading from tar, and before creating anything, with
	// an error wrapping ErrCapacityExhausted: ErrVolumeBuildBusy, or too
	// little disk. A build that ran and failed is a *VolumeBuildError.
	BuildVolume(ctx context.Context, params volumebuild.Params, tar io.Reader, deliver func(image io.Reader, size int64) error) error
}

// ErrVolumeBuildBusy refuses a build while the runner's one build runs.
var ErrVolumeBuildBusy = fmt.Errorf("a volume build is already running: %w", ErrCapacityExhausted)

// VolumeBuildTimeout is the Reason of a build the runner ended at its time
// limit. The other reasons are volumebuild's.
const VolumeBuildTimeout = "timeout"

// VolumeBuildReasons are the reasons a VolumeBuildError can have. Only these
// leave the runner, as metric labels or to the API.
var VolumeBuildReasons = []string{
	volumebuild.ReasonTar, volumebuild.ReasonTooLarge, volumebuild.ReasonNoSpace,
	volumebuild.ReasonFormat, volumebuild.ReasonMount, volumebuild.ReasonFsck, volumebuild.ReasonInternal,
	VolumeBuildTimeout,
}

// VolumeBuildError is a volume build that ran and failed.
type VolumeBuildError struct {
	// Reason is one of VolumeBuildReasons.
	Reason string
	Err    error
}

func (e *VolumeBuildError) Error() string {
	return fmt.Sprintf("volume build failed (%s): %v", e.Reason, e.Err)
}

func (e *VolumeBuildError) Unwrap() error { return e.Err }

// CallerFault reports whether the tar caused the failure, not the runner.
func (e *VolumeBuildError) CallerFault() bool {
	switch e.Reason {
	case volumebuild.ReasonTar, volumebuild.ReasonTooLarge, volumebuild.ReasonNoSpace:
		return true
	}
	return false
}
