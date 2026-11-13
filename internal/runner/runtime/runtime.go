// Package runtime defines the Runtime contract a sandbox backend implements and
// the types shared by the Docker and Firecracker implementations.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
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

// ErrSandboxNotFound is returned when a sandbox ID is not found.
var ErrSandboxNotFound = errors.New("sandbox not found")

// ErrSandboxNetworkUnavailable is returned when a sandbox exists but has no
// reachable network attachment yet.
var ErrSandboxNetworkUnavailable = errors.New("sandbox network unavailable")

// ErrSandboxNotRunning is returned when a sandbox exists but is not running.
var ErrSandboxNotRunning = errors.New("sandbox not running")

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
//
// Kind KindCode selects a code sandbox (Task API). The other code fields apply
// only to that kind. All fields are comparable, because the API compares the
// applied options with the requested options.
type CreateOptions struct {
	Egress       Egress `json:"egress,omitempty"`
	Kind         string `json:"kind,omitempty"`
	Runtime      string `json:"runtime,omitempty"`
	Slots        int    `json:"slots,omitempty"`
	TaskMemoryMB int    `json:"taskMemoryMb,omitempty"`
	SpareWorkers bool   `json:"spareWorkers,omitempty"`
	ReuseWorkers bool   `json:"reuseWorkers,omitempty"`
}

// KindCode is the kind of a code sandbox, created through the Task API.
const KindCode = "code"

// Limits of a code sandbox.
const (
	CodeMaxSlots        = 32
	CodeMinTaskMemoryMB = 64
	CodeMaxTaskMemoryMB = 16 * 1024
	// CodeMaxTotalTaskMemoryMB bounds slots x taskMemoryMb, so one create
	// cannot ask for a container limit far beyond a runner's memory.
	CodeMaxTotalTaskMemoryMB = 16 * 1024
)

var codeRuntimeRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// IsCode reports whether the options create a code sandbox.
func (o *CreateOptions) IsCode() bool { return o != nil && o.Kind == KindCode }

// validateKind checks the code sandbox fields.
func (o *CreateOptions) validateKind() error {
	switch o.Kind {
	case "":
		if o.Runtime != "" || o.Slots != 0 || o.TaskMemoryMB != 0 || o.SpareWorkers || o.ReuseWorkers {
			return errors.New("code sandbox fields need kind \"code\"")
		}
		return nil
	case KindCode:
	default:
		return fmt.Errorf("invalid kind %q", o.Kind)
	}
	if o.Egress != EgressNone {
		return errors.New("a code sandbox must have egress \"none\"")
	}
	if !codeRuntimeRegex.MatchString(o.Runtime) {
		return fmt.Errorf("invalid runtime %q", o.Runtime)
	}
	if o.Slots < 1 || o.Slots > CodeMaxSlots {
		return fmt.Errorf("slots must be in [1, %d]", CodeMaxSlots)
	}
	if o.TaskMemoryMB < CodeMinTaskMemoryMB || o.TaskMemoryMB > CodeMaxTaskMemoryMB {
		return fmt.Errorf("taskMemoryMb must be in [%d, %d]", CodeMinTaskMemoryMB, CodeMaxTaskMemoryMB)
	}
	if o.Slots*o.TaskMemoryMB > CodeMaxTotalTaskMemoryMB {
		return fmt.Errorf("slots x taskMemoryMb must be at most %d", CodeMaxTotalTaskMemoryMB)
	}
	return nil
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
	if err := opts.validateKind(); err != nil {
		return nil, err
	}
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

// Runtime is the sandbox backend contract used by the shared runner process.
type Runtime interface {
	Prepare(ctx context.Context)
	Ready(ctx context.Context) error
	ReadyCh() <-chan struct{}
	Capacity(ctx context.Context) (Capacity, error)

	CreateSandbox(ctx context.Context, sandboxID string, opts *CreateOptions) (*SandboxInfo, error)
	GetSandboxInfo(ctx context.Context, sandboxID string) (*SandboxInfo, error)
	DeleteSandbox(ctx context.Context, sandboxID string) error
	StopSandbox(ctx context.Context, sandboxID string) error
	EnsureSandboxRunning(ctx context.Context, sandboxID string) (WakeResult, error)
	DaemonURL(ctx context.Context, sandboxID string) (string, error)

	Shutdown(ctx context.Context)
}

// DaemonURLForgetter is a Runtime that caches the result of DaemonURL. A proxy
// that cannot reach the daemon at a returned URL calls ForgetDaemonURL, so the
// next request looks the sandbox up again.
type DaemonURLForgetter interface {
	ForgetDaemonURL(sandboxID string)
}
