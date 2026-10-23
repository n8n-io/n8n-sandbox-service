// Package runtime defines the Runtime contract a sandbox backend implements and
// the types shared by the Docker and Firecracker implementations.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
// JSON shape of the create RPC's create_options_json and
// applied_create_options_json fields.
type CreateOptions struct {
	Egress Egress `json:"egress,omitempty"`
	// Template is the image the sandbox boots; empty is DefaultTemplate.
	Template string `json:"template,omitempty"`
	// EgressProxy is set exactly when Egress is EgressProxy.
	EgressProxy *EgressProxySpec `json:"egress_proxy,omitempty"`
}

// Equal reports whether o and p ask for the same sandbox. Both must have come
// through ParseCreateOptions or been built from validated values.
func (o *CreateOptions) Equal(p *CreateOptions) bool {
	a, errA := json.Marshal(o)
	b, errB := json.Marshal(p)
	return errA == nil && errB == nil && string(a) == string(b)
}

// ParseCreateOptions decodes the wire form of CreateOptions. Empty means the
// defaults; otherwise it must be an object with a valid Egress, and an
// EgressProxy spec exactly when Egress is EgressProxy.
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
	if opts.Template == DefaultTemplate {
		opts.Template = ""
	}
	if opts.Template != "" {
		if err := ValidateTemplateName(opts.Template); err != nil {
			return nil, err
		}
	}
	switch {
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
