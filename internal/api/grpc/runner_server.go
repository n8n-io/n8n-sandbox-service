// Package grpcapi serves the RunnerRegistry gRPC stream on which runners register
// and send heartbeats.
package grpcapi

import (
	"crypto/subtle"
	"io"
	"log/slog"
	"regexp"
	"slices"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/n8n-io/sandbox-service/internal/api/grpc/pb"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// RunnerRegistryServer implements runner registration over gRPC.
type RunnerRegistryServer struct {
	pb.UnimplementedRunnerRegistryServer
	Token string
	Reg   registry.RunnerRegistry
	// Activity receives the sandboxes a heartbeat reports active; nil ignores them.
	Activity ActivityRecorder
}

// ActivityRecorder is the part of the sandbox store heartbeats write to.
type ActivityRecorder interface {
	TouchRunnerActive(runnerID string, ids []string) error
}

// Bounds on what one heartbeat may carry; a runner never needs more.
const (
	maxHeartbeatActive    = 4096
	maxHeartbeatTemplates = 64
)

var sandboxIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// heartbeatTemplates keeps the well-formed templates, deduplicated by name.
func heartbeatTemplates(runnerID string, in []*pb.TemplateInfo) []registry.Template {
	if len(in) > maxHeartbeatTemplates {
		slog.Warn("runner heartbeat templates truncated", "runner_id", runnerID, "count", len(in))
		in = in[:maxHeartbeatTemplates]
	}
	out := make([]registry.Template, 0, len(in))
	for _, t := range in {
		name := t.GetName()
		if runnerruntime.ValidateTemplateName(name) != nil || t.GetVcpus() < 0 || t.GetMemoryMb() < 0 || t.GetDiskMb() < 0 {
			slog.Warn("runner heartbeat template ignored", "runner_id", runnerID, "template", name)
			continue
		}
		if slices.ContainsFunc(out, func(o registry.Template) bool { return o.Name == name }) {
			continue
		}
		out = append(out, registry.Template{Name: name, VCPUs: t.GetVcpus(), MemoryMB: t.GetMemoryMb(), DiskMB: t.GetDiskMb()})
	}
	return out
}

// heartbeatActive keeps the well-formed sandbox ids.
func heartbeatActive(ids []string) []string {
	if len(ids) > maxHeartbeatActive {
		ids = ids[:maxHeartbeatActive]
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if sandboxIDRe.MatchString(id) {
			out = append(out, id)
		}
	}
	return out
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
			return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
		}
	}
	return false
}

// Connect accepts bi-directional streams from runners.
func (s *RunnerRegistryServer) Connect(stream grpc.BidiStreamingServer[pb.RunnerHeartbeat, pb.ControlMessage]) error {
	md, ok := metadata.FromIncomingContext(stream.Context())
	if !ok || !validateBearer(md, s.Token) {
		slog.Warn("runner registration rejected", "reason", "missing_or_invalid_token")
		return status.Errorf(codes.Unauthenticated, "missing or invalid registration token")
	}

	// Bind to the first non-empty runner_id for this stream and reject changes so we
	// never leave an earlier ID registered when the client disconnects.
	var committedRunnerID, committedHTTPBase, committedControlGRPC string
	var registered bool
	defer func() {
		if committedRunnerID != "" {
			s.Reg.Remove(committedRunnerID)
			slog.Info("runner registration removed", "runner_id", committedRunnerID)
		}
	}()

	for {
		hb, err := stream.Recv()
		if err == io.EOF {
			if committedRunnerID != "" {
				slog.Info("runner registration stream closed", "runner_id", committedRunnerID)
			}
			return nil
		}
		if err != nil {
			if committedRunnerID != "" {
				slog.Warn("runner registration stream receive failed", "runner_id", committedRunnerID, "error", err)
			} else {
				slog.Warn("runner registration stream receive failed", "error", err)
			}
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
			slog.Warn("runner registration rejected", "runner_id", committedRunnerID, "reason", "invalid_http_base_url", "http_base_url", httpBase)
			return status.Errorf(codes.InvalidArgument, "http_base_url must be an absolute https URL with a host")
		}
		controlGRPC := strings.TrimSpace(hb.GetControlGrpcAddr())
		if committedRunnerID != "" && controlGRPC == "" {
			controlGRPC = committedControlGRPC
		}
		if controlGRPC == "" {
			slog.Warn("runner registration rejected", "runner_id", committedRunnerID, "reason", "missing_control_grpc_addr")
			return status.Errorf(codes.InvalidArgument, "control_grpc_addr must be set")
		}
		committedHTTPBase = httpBase
		committedControlGRPC = controlGRPC
		s.Reg.UpsertRunner(registry.Runner{
			ID:              committedRunnerID,
			HTTPBaseURL:     httpBase,
			ControlGRPCAddr: controlGRPC,
			Healthy:         hb.GetHealthy(),
			CapacityTotal:   hb.GetCapacityTotal(),
			CapacityUsed:    hb.GetCapacityUsed(),
			CapacityStopped: hb.GetCapacityStopped(),
			Cordoned:        hb.GetCordoned(),
			Templates:       heartbeatTemplates(committedRunnerID, hb.GetTemplates()),
		})
		if active := heartbeatActive(hb.GetActiveSandboxIds()); s.Activity != nil && len(active) > 0 {
			if err := s.Activity.TouchRunnerActive(committedRunnerID, active); err != nil {
				slog.Warn("runner heartbeat activity not recorded", "runner_id", committedRunnerID, "error", err)
			}
		}
		if !registered {
			registered = true
			slog.Info(
				"runner registered",
				"runner_id", committedRunnerID,
				"http_base_url", httpBase,
				"control_grpc_addr", controlGRPC,
				"healthy", hb.GetHealthy(),
				"capacity_total", hb.GetCapacityTotal(),
				"capacity_used", hb.GetCapacityUsed(),
				"capacity_stopped", hb.GetCapacityStopped(),
			)
		}
		if err := stream.Send(&pb.ControlMessage{Ack: true}); err != nil {
			slog.Warn("runner registration ack failed", "runner_id", committedRunnerID, "error", err)
			return err
		}
	}
}
