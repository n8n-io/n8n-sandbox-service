package runner

import (
	"context"
	"errors"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/n8n-io/sandbox-service/internal/api/grpc/pb"
	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/runner/config"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// SandboxControlGRPC implements runner.v1.SandboxControl (API → runner).
type SandboxControlGRPC struct {
	pb.UnimplementedSandboxControlServer
	Runtime runnerruntime.Runtime
	Cfg     *config.Config
	Rec     *metrics.RunnerRecorder
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
	return status.Errorf(codes.Internal, "%v", err)
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
	if err := s.Runtime.Ready(ctx); err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	sandboxID := strings.TrimSpace(req.GetSandboxId())
	if !isValidID(sandboxID) {
		return nil, status.Error(codes.InvalidArgument, "invalid sandbox id")
	}
	start := time.Now()
	info, err := s.Runtime.CreateSandbox(ctx, sandboxID, &runnerruntime.CreateOptions{})
	s.Rec.ObserveContainerOp(metrics.OpCreate, err == nil && info != nil, time.Since(start))
	if err != nil {
		return nil, toGRPCError(err)
	}
	if info == nil {
		return nil, status.Error(codes.Internal, "create sandbox returned nil info")
	}
	return &pb.CreateSandboxResponse{SandboxId: sandboxID, ContainerIp: info.IP}, nil
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
	if err := s.Runtime.StopSandbox(ctx, sandboxID); err != nil {
		if errors.Is(err, runnerruntime.ErrSandboxNotFound) {
			return &pb.StopSandboxResponse{}, nil
		}
		return nil, toGRPCError(err)
	}
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
