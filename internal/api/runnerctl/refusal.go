package runnerctl

import (
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A runner marks a create it refused for want of capacity with this ErrorInfo.
// The code alone is not enough: gRPC itself reports ResourceExhausted for
// transport conditions (an ENHANCE_YOUR_CALM stream reset, an oversized
// response) that can arise after the runner has created the sandbox.
const (
	refusalDomain = "sandbox-runner"
	refusalReason = "CAPACITY_EXHAUSTED"
)

// CapacityRefusal is what a runner returns for a create it refused for want of
// capacity, before creating anything.
func CapacityRefusal(msg string) error {
	st, err := status.New(codes.ResourceExhausted, msg).WithDetails(&errdetails.ErrorInfo{
		Domain: refusalDomain,
		Reason: refusalReason,
	})
	if err != nil {
		// Without the marker the API does not retry, which is the safe side.
		return status.Error(codes.ResourceExhausted, msg)
	}
	return st.Err()
}

// IsCapacityRefusal reports whether err is a runner's CapacityRefusal, so the
// sandbox can be created on another runner without leaving one behind here.
func IsCapacityRefusal(err error) bool {
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.ResourceExhausted {
		return false
	}
	for _, detail := range st.Details() {
		if info, ok := detail.(*errdetails.ErrorInfo); ok && info.GetDomain() == refusalDomain && info.GetReason() == refusalReason {
			return true
		}
	}
	return false
}
