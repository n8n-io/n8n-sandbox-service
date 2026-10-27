package runner

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/n8n-io/sandbox-service/internal/api/grpc/pb"
	"github.com/n8n-io/sandbox-service/internal/api/runnerctl"
	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/obs"
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
}

var _ pb.SandboxControlServer = (*SandboxControlGRPC)(nil)

func toGRPCError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return status.Error(codes.Canceled, context.Canceled.Error())
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
	// A capacity refusal makes the API retry the create on another runner.
	if s.Cordon.Cordoned() {
		return nil, runnerctl.CapacityRefusal("runner is cordoned")
	}
	if err := s.Runtime.Ready(ctx); err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	sandboxID := strings.TrimSpace(req.GetSandboxId())
	if !isValidID(sandboxID) {
		return nil, status.Error(codes.InvalidArgument, "invalid sandbox id")
	}
	// An unknown egress is refused rather than defaulted, so an API ahead of this
	// runner cannot have a policy it asked for silently dropped.
	opts, err := runnerruntime.ParseCreateOptions(req.GetCreateOptionsJson())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid create options: "+err.Error())
	}
	// Echoed so the API can tell a runner that applied the options from one that
	// ignored them. Marshalled here, before anything is created.
	applied, err := json.Marshal(opts)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	ctx = withTrace(ctx)
	start := time.Now()
	info, err := s.Runtime.CreateSandbox(ctx, sandboxID, opts)
	s.Rec.ObserveContainerOp(metrics.OpCreate, err == nil && info != nil, time.Since(start))
	if err != nil {
		return nil, toGRPCError(err)
	}
	if info == nil {
		return nil, status.Error(codes.Internal, "create sandbox returned nil info")
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
	ctx = withTrace(ctx)
	start := time.Now()
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
	err := s.Runtime.DeleteSandbox(ctx, sandboxID)
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
	// Another runner may now hold the address the caller has for this one.
	if id := req.GetRunnerId(); id != "" && id != s.Cfg.RunnerID {
		return nil, status.Errorf(codes.FailedPrecondition, "this is runner %q, not %q", s.Cfg.RunnerID, id)
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
