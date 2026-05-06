package runner

import (
	"context"
	"errors"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/n8n-io/sandbox-service/internal/api/grpc/pb"
	"github.com/n8n-io/sandbox-service/internal/runner/config"
	"github.com/n8n-io/sandbox-service/internal/runner/manager"
)

// SandboxControlGRPC implements runner.v1.SandboxControl (API → runner).
type SandboxControlGRPC struct {
	pb.UnimplementedSandboxControlServer
	Mgr *manager.Manager
	Cfg *config.Config
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

// CreateSandbox creates a managed container for the given sandbox id.
func (s *SandboxControlGRPC) CreateSandbox(ctx context.Context, req *pb.CreateSandboxRequest) (*pb.CreateSandboxResponse, error) {
	if err := s.checkAPIKey(ctx); err != nil {
		return nil, err
	}
	sandboxID := strings.TrimSpace(req.GetSandboxId())
	if !isValidID(sandboxID) {
		return nil, status.Error(codes.InvalidArgument, "invalid sandbox id")
	}
	info, err := s.Mgr.CreateContainer(ctx, sandboxID, &manager.CreateOptions{})
	if err != nil {
		return nil, toGRPCError(err)
	}
	if info == nil {
		return nil, status.Error(codes.Internal, "create container returned nil info")
	}
	return &pb.CreateSandboxResponse{SandboxId: sandboxID, ContainerIp: info.IP}, nil
}

// DeleteSandbox removes the sandbox container if it exists.
func (s *SandboxControlGRPC) DeleteSandbox(ctx context.Context, req *pb.DeleteSandboxRequest) (*pb.DeleteSandboxResponse, error) {
	if err := s.checkAPIKey(ctx); err != nil {
		return nil, err
	}
	sandboxID := strings.TrimSpace(req.GetSandboxId())
	if !isValidID(sandboxID) {
		return nil, status.Error(codes.InvalidArgument, "invalid sandbox id")
	}
	containerID, err := s.Mgr.FindContainerIDByLabel(ctx, sandboxID)
	if err != nil {
		if errors.Is(err, manager.ErrSandboxNotFound) {
			return &pb.DeleteSandboxResponse{}, nil
		}
		return nil, toGRPCError(err)
	}
	cinfo, err := s.Mgr.GetContainerInfo(ctx, containerID)
	if err != nil {
		if errors.Is(err, manager.ErrSandboxNotFound) {
			return &pb.DeleteSandboxResponse{}, nil
		}
		return nil, toGRPCError(err)
	}
	if err := s.Mgr.DeleteContainer(ctx, containerID, cinfo.IP); err != nil {
		return nil, toGRPCError(err)
	}
	return &pb.DeleteSandboxResponse{}, nil
}
