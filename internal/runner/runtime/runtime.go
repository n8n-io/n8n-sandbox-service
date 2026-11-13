// Package runtime defines the Runtime contract a sandbox backend implements and
// the types shared by the Docker and Firecracker implementations.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// Egress is a sandbox's outbound network policy.
type Egress string

const (
	// EgressPublic allows the public internet; the private ranges in netpolicy stay
	// blocked. Default when field is omitted or empty.
	EgressPublic Egress = "public"
	// EgressNone lets no guest-initiated connection leave the sandbox, DNS included.
	EgressNone Egress = "none"
)

// ParseEgress maps the wire value to an Egress, treating empty as EgressPublic.
func ParseEgress(s string) (Egress, error) {
	switch Egress(s) {
	case "", EgressPublic:
		return EgressPublic, nil
	case EgressNone:
		return EgressNone, nil
	}
	return "", fmt.Errorf("invalid egress %q: must be %q or %q", s, EgressPublic, EgressNone)
}

// CreateOptions holds optional parameters for sandbox creation. It is also the
// JSON shape of the create RPC's create_options_json and
// applied_create_options_json fields.
type CreateOptions struct {
	Egress Egress `json:"egress,omitempty"`
}

// ParseCreateOptions decodes the wire form of CreateOptions. Empty means the
// defaults; otherwise it must be an object with a valid Egress.
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
	return opts, nil
}

// BlockEgress reports whether the sandbox gets no egress at all. Options reach a
// runtime through ParseCreateOptions, so any other value is a bug, not input.
func (o *CreateOptions) BlockEgress() bool {
	if o == nil {
		return false
	}
	switch o.Egress {
	case "", EgressPublic:
		return false
	case EgressNone:
		return true
	}
	panic(fmt.Sprintf("unvalidated egress %q", o.Egress))
}

// SandboxInfo represents information about a created sandbox.
type SandboxInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	IP   string `json:"ip"`
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

// Runtime is the sandbox backend contract used by the shared runner process.
type Runtime interface {
	Prepare(ctx context.Context)
	Ready(ctx context.Context) error
	ReadyCh() <-chan struct{}
	Capacity(ctx context.Context) (Capacity, error)

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
