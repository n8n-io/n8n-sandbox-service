package grpcapi

import (
	"io"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/n8n-io/sandbox-service/internal/api/grpc/pb"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
)

// RunnerRegistryServer implements runner registration over gRPC.
type RunnerRegistryServer struct {
	pb.UnimplementedRunnerRegistryServer
	Token string
	Reg   *registry.Registry
}

var _ pb.RunnerRegistryServer = (*RunnerRegistryServer)(nil)

func validateBearer(md metadata.MD, token string) bool {
	if token == "" {
		return false
	}
	for _, v := range md.Get("authorization") {
		v = strings.TrimSpace(v)
		const prefix = "Bearer "
		if strings.HasPrefix(strings.ToLower(v), strings.ToLower(prefix)) {
			got := strings.TrimSpace(v[len(prefix):])
			return got == token
		}
	}
	return false
}

// Connect accepts bi-directional streams from runners.
func (s *RunnerRegistryServer) Connect(stream grpc.BidiStreamingServer[pb.RunnerHeartbeat, pb.ControlMessage]) error {
	md, ok := metadata.FromIncomingContext(stream.Context())
	if !ok || !validateBearer(md, s.Token) {
		return status.Errorf(codes.Unauthenticated, "missing or invalid registration token")
	}

	var runnerID string
	defer func() {
		if runnerID != "" {
			s.Reg.Remove(runnerID)
		}
	}()

	for {
		hb, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		runnerID = hb.GetRunnerId()
		s.Reg.Upsert(
			runnerID,
			hb.GetHttpBaseUrl(),
			hb.GetHealthy(),
			hb.GetCapacityTotal(),
			hb.GetCapacityUsed(),
		)
		if err := stream.Send(&pb.ControlMessage{Ack: true}); err != nil {
			return err
		}
	}
}
