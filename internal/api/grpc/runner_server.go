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

	// Bind to the first non-empty runner_id for this stream and reject changes so we
	// never leave an earlier ID registered when the client disconnects.
	var committedRunnerID, committedHTTPBase, committedControlGRPC string
	defer func() {
		if committedRunnerID != "" {
			s.Reg.Remove(committedRunnerID)
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
		id := strings.TrimSpace(hb.GetRunnerId())
		if committedRunnerID == "" {
			if id == "" {
				if err := stream.Send(&pb.ControlMessage{Ack: true}); err != nil {
					return err
				}
				continue
			}
			committedRunnerID = id
		} else {
			if id != "" && id != committedRunnerID {
				return status.Errorf(codes.InvalidArgument, "runner_id cannot change during connection")
			}
		}
		httpBase := strings.TrimRight(hb.GetHttpBaseUrl(), "/")
		if committedRunnerID != "" && httpBase == "" {
			httpBase = committedHTTPBase
		}
		if httpBase == "" || !registry.IsValidRunnerHTTPBaseURL(httpBase) {
			return status.Errorf(codes.InvalidArgument, "http_base_url must be an absolute http or https URL with a host")
		}
		controlGRPC := strings.TrimSpace(hb.GetControlGrpcAddr())
		if committedRunnerID != "" && controlGRPC == "" {
			controlGRPC = committedControlGRPC
		}
		if controlGRPC == "" {
			return status.Errorf(codes.InvalidArgument, "control_grpc_addr must be set")
		}
		committedHTTPBase = httpBase
		committedControlGRPC = controlGRPC
		s.Reg.Upsert(
			committedRunnerID,
			httpBase,
			controlGRPC,
			hb.GetHealthy(),
			hb.GetCapacityTotal(),
			hb.GetCapacityUsed(),
		)
		if err := stream.Send(&pb.ControlMessage{Ack: true}); err != nil {
			return err
		}
	}
}
