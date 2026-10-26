package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/n8n-io/sandbox-service/internal/api/grpc/pb"
	"github.com/n8n-io/sandbox-service/internal/api/runnerctl"
	"github.com/n8n-io/sandbox-service/internal/egressproxy"
	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/obs"
	"github.com/n8n-io/sandbox-service/internal/runner/activity"
	"github.com/n8n-io/sandbox-service/internal/runner/config"
	"github.com/n8n-io/sandbox-service/internal/runner/cordon"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// SandboxControlGRPC implements runner.v1.SandboxControl (API → runner).
type SandboxControlGRPC struct {
	pb.UnimplementedSandboxControlServer
	Runtime runnerruntime.Runtime
	Cfg     *config.Config
	Rec     *metrics.RunnerRecorder
	Cordon  *cordon.State
	// Activity is what the runner is still running; nil tracks nothing.
	Activity *activity.Tracker
	// Egress is the egress proxy; nil, or a runtime that is no
	// EgressProxyHost, refuses egress-proxy creates as Unimplemented.
	Egress *egressproxy.Server

	creating inflight
}

// inflight is the sandbox ids with a create in progress.
type inflight struct {
	mu  sync.Mutex
	ids map[string]struct{}
}

func (f *inflight) begin(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.ids[id]; ok {
		return false
	}
	if f.ids == nil {
		f.ids = map[string]struct{}{}
	}
	f.ids[id] = struct{}{}
	return true
}

func (f *inflight) end(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.ids, id)
}

var _ pb.SandboxControlServer = (*SandboxControlGRPC)(nil)

func toGRPCError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return status.Error(codes.Canceled, context.Canceled.Error())
	}
	// The API deletes a sandbox whose archive is gone; nothing else may say so.
	if errors.Is(err, errArchiveLost) {
		return status.Error(codes.DataLoss, errArchiveLost.Error())
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, context.DeadlineExceeded.Error())
	}
	// The API retries this on another runner, so it must only ever mean that
	// nothing was created.
	if errors.Is(err, runnerruntime.ErrCapacityExhausted) {
		return runnerctl.CapacityRefusal(err.Error())
	}
	return status.Errorf(codes.Internal, "%v", err)
}

// withTrace carries the API's trace context into the runtime call so the
// lifecycle event the runner emits joins the API's event for the same request.
// A missing or malformed header yields a fresh id, so events always correlate
// internally even when the caller sends nothing.
func withTrace(ctx context.Context) context.Context {
	header := ""
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get(obs.HeaderTraceparent); len(v) > 0 {
			header = v[0]
		}
	}
	return obs.WithTraceparent(ctx, obs.EnsureTraceparent(header))
}

func (s *SandboxControlGRPC) checkAPIKey(ctx context.Context) error {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing metadata")
	}
	key := ""
	if v := md.Get("x-api-key"); len(v) > 0 {
		key = strings.TrimSpace(v[0])
	}
	if key == "" {
		return status.Error(codes.Unauthenticated, "missing x-api-key")
	}
	if _, ok := s.Cfg.APIKeys[key]; !ok {
		return status.Error(codes.Unauthenticated, "invalid api key")
	}
	return nil
}

// CreateSandbox creates a managed sandbox for the given sandbox id.
func (s *SandboxControlGRPC) CreateSandbox(ctx context.Context, req *pb.CreateSandboxRequest) (*pb.CreateSandboxResponse, error) {
	if err := s.checkAPIKey(ctx); err != nil {
		return nil, err
	}
	return s.create(ctx, req.GetSandboxId(), req.GetCreateOptionsJson(), nil)
}

// create is CreateSandbox, or with archive set RestoreSandbox: archive opens
// the sandbox's disk, and is only called once every check has passed, so a
// refused restore downloads nothing.
func (s *SandboxControlGRPC) create(ctx context.Context, rawID, optionsJSON string, archive func(context.Context) (io.ReadCloser, error)) (*pb.CreateSandboxResponse, error) {
	// A capacity refusal makes the API retry the create on another runner.
	if s.Cordon.Cordoned() {
		return nil, runnerctl.CapacityRefusal("runner is cordoned")
	}
	if err := s.Runtime.Ready(ctx); err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	sandboxID := strings.TrimSpace(rawID)
	if !isValidID(sandboxID) {
		return nil, status.Error(codes.InvalidArgument, "invalid sandbox id")
	}
	var archiver runnerruntime.Archiver
	if archive != nil {
		var ok bool
		if archiver, ok = s.Runtime.(runnerruntime.Archiver); !ok {
			return nil, status.Error(codes.Unimplemented, "this runner cannot restore archives")
		}
	}
	// An unknown egress is refused rather than defaulted, so an API ahead of this
	// runner cannot have a policy it asked for silently dropped.
	opts, err := runnerruntime.ParseCreateOptions(optionsJSON)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid create options: "+err.Error())
	}
	// A template this runner does not serve is refused, never booted as
	// another, so a sandbox never runs an image it did not ask for. The API
	// only places a sandbox on a runner that lists its template; one that
	// dropped it since is refused like a full runner, so another is tried.
	if !runnerruntime.ServesTemplate(s.Runtime.Templates(), opts.Template) {
		return nil, runnerctl.CapacityRefusal(fmt.Sprintf("runner has no template %q", opts.Template))
	}
	// A create for a sandbox in flight or live is refused before touching it, so
	// a failure below cannot roll back another create's sandbox.
	if !s.creating.begin(sandboxID) {
		return nil, status.Error(codes.AlreadyExists, "sandbox already exists")
	}
	defer s.creating.end(sandboxID)
	if s.egressRecordLive(ctx, sandboxID) {
		return nil, status.Error(codes.AlreadyExists, "sandbox already exists")
	}
	var host runnerruntime.EgressProxyHost
	if opts.Egress == runnerruntime.EgressProxy {
		var err error
		if host, err = s.admitEgressProxy(opts); err != nil {
			return nil, err
		}
		// Installed before the sandbox exists, so its first request finds it.
		s.Egress.Set(sandboxID, opts.Tenant, opts.EgressProxy)
	}
	// Echoed so the API can tell a runner that applied the options from one that
	// ignored them. Marshalled here, before anything is created.
	applied, err := json.Marshal(opts)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	ctx = withTrace(ctx)
	start := time.Now()
	var info *runnerruntime.SandboxInfo
	if archive == nil {
		info, err = s.Runtime.CreateSandbox(ctx, sandboxID, opts)
	} else {
		info, err = restoreFrom(ctx, archiver, sandboxID, opts, archive)
	}
	s.Rec.ObserveContainerOp(metrics.OpCreate, err == nil && info != nil, time.Since(start))
	if err == nil && info == nil {
		err = errors.New("create sandbox returned nil info")
	}
	if err == nil && host != nil {
		err = s.installEgressCA(ctx, sandboxID)
	}
	if err != nil {
		if host != nil {
			s.Egress.Remove(sandboxID)
		}
		if info != nil {
			// The sandbox exists but cannot reach its proxy: remove it rather
			// than hand out one whose egress does not work.
			_ = s.Runtime.DeleteSandbox(context.WithoutCancel(ctx), sandboxID)
		}
		return nil, toGRPCError(err)
	}
	return &pb.CreateSandboxResponse{SandboxId: sandboxID, ContainerIp: info.IP, AppliedCreateOptionsJson: string(applied)}, nil
}

// StopSandbox stops the sandbox without removing it.
func (s *SandboxControlGRPC) StopSandbox(ctx context.Context, req *pb.StopSandboxRequest) (*pb.StopSandboxResponse, error) {
	if err := s.checkAPIKey(ctx); err != nil {
		return nil, err
	}
	sandboxID := strings.TrimSpace(req.GetSandboxId())
	if !isValidID(sandboxID) {
		return nil, status.Error(codes.InvalidArgument, "invalid sandbox id")
	}
	// Only the idle sweeper stops sandboxes. Its view of activity can lag by a
	// heartbeat, or miss one an API pod lost, so the runner has the last word:
	// a stop never kills a run in progress.
	if s.Activity.Active(sandboxID) {
		return nil, status.Error(codes.FailedPrecondition, "sandbox is active")
	}
	ctx = withTrace(ctx)
	start := time.Now()
	defer s.Activity.Forget(sandboxID)
	if err := s.Runtime.StopSandbox(ctx, sandboxID); err != nil {
		if errors.Is(err, runnerruntime.ErrSandboxNotFound) {
			s.Rec.ObserveContainerOp(metrics.OpStop, true, time.Since(start))
			return &pb.StopSandboxResponse{}, nil
		}
		s.Rec.ObserveContainerOp(metrics.OpStop, false, time.Since(start))
		return nil, toGRPCError(err)
	}
	s.Rec.ObserveContainerOp(metrics.OpStop, true, time.Since(start))
	return &pb.StopSandboxResponse{}, nil
}

// DeleteSandbox removes the sandbox if it exists.
func (s *SandboxControlGRPC) DeleteSandbox(ctx context.Context, req *pb.DeleteSandboxRequest) (*pb.DeleteSandboxResponse, error) {
	if err := s.checkAPIKey(ctx); err != nil {
		return nil, err
	}
	sandboxID := strings.TrimSpace(req.GetSandboxId())
	if !isValidID(sandboxID) {
		return nil, status.Error(codes.InvalidArgument, "invalid sandbox id")
	}
	ctx = withTrace(ctx)
	start := time.Now()
	defer s.Activity.Forget(sandboxID)
	err := s.Runtime.DeleteSandbox(ctx, sandboxID)
	if err == nil || errors.Is(err, runnerruntime.ErrSandboxNotFound) {
		if s.Egress != nil {
			s.Egress.Remove(sandboxID)
		}
	}
	s.Rec.ObserveContainerOp(metrics.OpDelete, err == nil, time.Since(start))
	if err != nil {
		if errors.Is(err, runnerruntime.ErrSandboxNotFound) {
			return &pb.DeleteSandboxResponse{}, nil
		}
		return nil, toGRPCError(err)
	}
	return &pb.DeleteSandboxResponse{}, nil
}

// GetCordon reports whether the runner is cordoned.
func (s *SandboxControlGRPC) GetCordon(ctx context.Context, _ *pb.GetCordonRequest) (*pb.GetCordonResponse, error) {
	if err := s.checkAPIKey(ctx); err != nil {
		return nil, err
	}
	return &pb.GetCordonResponse{Cordoned: s.Cordon.Cordoned()}, nil
}

// SetCordon cordons or uncordons the runner. Idempotent.
func (s *SandboxControlGRPC) SetCordon(ctx context.Context, req *pb.SetCordonRequest) (*pb.SetCordonResponse, error) {
	if err := s.checkAPIKey(ctx); err != nil {
		return nil, err
	}
	if s.Cordon.Set(req.GetCordoned()) {
		remote := ""
		if p, ok := peer.FromContext(ctx); ok {
			remote = p.Addr.String()
		}
		slog.InfoContext(ctx, "runner cordon changed", "cordoned", req.GetCordoned(), "remote_addr", remote)
	}
	return &pb.SetCordonResponse{Cordoned: req.GetCordoned()}, nil
}

// GetActivity reports what drain waits for: the cordon, and the executions and
// streams the runner is still serving.
func (s *SandboxControlGRPC) GetActivity(ctx context.Context, _ *pb.GetActivityRequest) (*pb.GetActivityResponse, error) {
	if err := s.checkAPIKey(ctx); err != nil {
		return nil, err
	}
	capacity, err := s.Runtime.Capacity(ctx)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "runtime capacity: "+err.Error())
	}
	snap := s.Activity.Snapshot()
	return &pb.GetActivityResponse{
		Cordoned:         s.Cordon.Cordoned(),
		ActiveExecutions: int32(snap.Executions),
		OpenStreams:      int32(snap.Streams),
		CapacityTotal:    capacity.Total,
		CapacityUsed:     capacity.Used,
		CapacityStopped:  capacity.Stopped,
	}, nil
}

// admitEgressProxy checks that this runner can serve opts' proxy spec before
// anything is created. Unimplemented for what the runner lacks, so the API
// tells it from a bad request; FailedPrecondition for a secret the tenant
// does not have.
func (s *SandboxControlGRPC) admitEgressProxy(opts *runnerruntime.CreateOptions) (runnerruntime.EgressProxyHost, error) {
	host, ok := s.Runtime.(runnerruntime.EgressProxyHost)
	if !ok || s.Egress == nil {
		return nil, status.Error(codes.Unimplemented, "egress proxy is not supported by this runner")
	}
	if len(opts.EgressProxy.ModelHosts) > 0 && !s.Egress.HasSink() {
		return nil, status.Error(codes.Unimplemented, "egress proxy model hosts need a usage sink, and this runner has none")
	}
	for _, c := range opts.EgressProxy.Credentials {
		if !s.Egress.HasSecret(opts.Tenant, c.SecretRef) {
			return nil, status.Errorf(codes.FailedPrecondition, "unknown secret_ref %q", c.SecretRef)
		}
	}
	return host, nil
}

// egressRecordLive reports whether the proxy knows sandboxID and the runtime
// may still have it. A record whose container is gone (removed behind the
// runner's back; the API dropped its row on the runner's 404) is dropped, so a
// create under the same id succeeds; any doubt keeps the refusal.
func (s *SandboxControlGRPC) egressRecordLive(ctx context.Context, sandboxID string) bool {
	if s.Egress == nil || !s.Egress.Has(sandboxID) {
		return false
	}
	if _, err := s.Runtime.GetSandboxInfo(ctx, sandboxID); !errors.Is(err, runnerruntime.ErrSandboxNotFound) {
		return true
	}
	slog.WarnContext(ctx, "dropping the egress record of a sandbox whose container is gone", "sandbox_id", sandboxID)
	s.Egress.Remove(sandboxID)
	return false
}

func (s *SandboxControlGRPC) installEgressCA(ctx context.Context, sandboxID string) error {
	daemonURL, err := s.Runtime.DaemonURL(ctx, sandboxID)
	if err != nil {
		return err
	}
	return installGuestCA(ctx, daemonURL, s.Egress.CACertPEM())
}

// UpdateEgressProxy replaces a proxy sandbox's spec. The sandbox keeps its
// tenant; the spec is validated as at create.
func (s *SandboxControlGRPC) UpdateEgressProxy(ctx context.Context, req *pb.UpdateEgressProxyRequest) (*pb.UpdateEgressProxyResponse, error) {
	if err := s.checkAPIKey(ctx); err != nil {
		return nil, err
	}
	sandboxID := strings.TrimSpace(req.GetSandboxId())
	if !isValidID(sandboxID) {
		return nil, status.Error(codes.InvalidArgument, "invalid sandbox id")
	}
	var spec runnerruntime.EgressProxySpec
	if err := json.Unmarshal([]byte(req.GetEgressProxyJson()), &spec); err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid egress_proxy: "+err.Error())
	}
	if err := spec.Validate(); err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid egress_proxy: "+err.Error())
	}
	if s.Egress == nil {
		return nil, status.Error(codes.Unimplemented, "egress proxy is not supported by this runner")
	}
	tenant, ok := s.Egress.Tenant(sandboxID)
	if !ok {
		return nil, status.Error(codes.NotFound, "no egress proxy sandbox "+sandboxID)
	}
	if len(spec.ModelHosts) > 0 && !s.Egress.HasSink() {
		return nil, status.Error(codes.Unimplemented, "egress proxy model hosts need a usage sink, and this runner has none")
	}
	for _, c := range spec.Credentials {
		if !s.Egress.HasSecret(tenant, c.SecretRef) {
			return nil, status.Errorf(codes.FailedPrecondition, "unknown secret_ref %q", c.SecretRef)
		}
	}
	s.Egress.Set(sandboxID, tenant, &spec)
	return &pb.UpdateEgressProxyResponse{}, nil
}

// SetModelBlocked blocks or unblocks a proxy sandbox's model requests.
func (s *SandboxControlGRPC) SetModelBlocked(ctx context.Context, req *pb.SetModelBlockedRequest) (*pb.SetModelBlockedResponse, error) {
	if err := s.checkAPIKey(ctx); err != nil {
		return nil, err
	}
	sandboxID := strings.TrimSpace(req.GetSandboxId())
	if !isValidID(sandboxID) {
		return nil, status.Error(codes.InvalidArgument, "invalid sandbox id")
	}
	if s.Egress == nil || !s.Egress.SetModelBlocked(sandboxID, req.GetBlocked()) {
		return nil, status.Error(codes.NotFound, "no egress proxy sandbox "+sandboxID)
	}
	return &pb.SetModelBlockedResponse{}, nil
}
