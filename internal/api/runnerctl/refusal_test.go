package runnerctl

import (
	"errors"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestIsCapacityRefusal(t *testing.T) {
	withInfo := func(code codes.Code, domain, reason string) error {
		st, err := status.New(code, "refused").WithDetails(&errdetails.ErrorInfo{Domain: domain, Reason: reason})
		if err != nil {
			t.Fatalf("WithDetails: %v", err)
		}
		return st.Err()
	}

	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "runner refusal", err: CapacityRefusal("firecracker runner capacity exhausted"), want: true},
		// What gRPC itself reports for an ENHANCE_YOUR_CALM reset or an oversized message.
		{name: "bare ResourceExhausted", err: status.Error(codes.ResourceExhausted, "grpc: received message larger than max"), want: false},
		{name: "marker on another code", err: withInfo(codes.Internal, refusalDomain, refusalReason), want: false},
		{name: "other reason", err: withInfo(codes.ResourceExhausted, refusalDomain, "QUOTA"), want: false},
		{name: "other domain", err: withInfo(codes.ResourceExhausted, "elsewhere", refusalReason), want: false},
		{name: "not a status", err: errors.New("dial failed"), want: false},
		{name: "nil", err: nil, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsCapacityRefusal(tc.err); got != tc.want {
				t.Fatalf("IsCapacityRefusal(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
