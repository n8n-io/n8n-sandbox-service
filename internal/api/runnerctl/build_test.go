package runnerctl

import (
	"errors"
	"fmt"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestBuildFailureReason(t *testing.T) {
	withInfo := func(info *errdetails.ErrorInfo) error {
		st, err := status.New(codes.InvalidArgument, "failed").WithDetails(info)
		if err != nil {
			t.Fatalf("WithDetails: %v", err)
		}
		return st.Err()
	}
	reason := map[string]string{buildFailureMetadataKey: "tar"}

	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{name: "a bad tar", err: BuildFailure(codes.InvalidArgument, "tar", "volume build failed: tar"), want: "tar"},
		{name: "a runner fault", err: BuildFailure(codes.Internal, "fsck", "volume build failed: fsck"), want: "fsck"},
		{name: "wrapped", err: fmt.Errorf("build: %w", BuildFailure(codes.Internal, "mount", "failed")), want: "mount"},
		{name: "a time limit", err: BuildFailure(codes.DeadlineExceeded, "timeout", "volume build failed: timeout"), want: "timeout"},
		// A runner is trusted less than the API: its reason becomes a metric label.
		{name: "a reason no runner has", err: BuildFailure(codes.Internal, "tar\",result=\"success", "failed"), want: "internal"},
		// What gRPC or a handler reports for a call that never became a build.
		{name: "a bare status", err: status.Error(codes.InvalidArgument, "invalid header")},
		{name: "a capacity refusal", err: CapacityRefusal("a volume build is already running")},
		{name: "another domain", err: withInfo(&errdetails.ErrorInfo{Domain: "elsewhere", Reason: buildFailureReason, Metadata: reason})},
		{name: "another reason", err: withInfo(&errdetails.ErrorInfo{Domain: refusalDomain, Reason: refusalReason, Metadata: reason})},
		{name: "no reason in the metadata", err: withInfo(&errdetails.ErrorInfo{Domain: refusalDomain, Reason: buildFailureReason})},
		{name: "not a status", err: errors.New("dial failed")},
		{name: "nil"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := BuildFailureReason(tc.err)
			if got != tc.want || ok != (tc.want != "") {
				t.Fatalf("BuildFailureReason(%v) = %q, %v; want %q", tc.err, got, ok, tc.want)
			}
		})
	}
}

func TestBuildFailureIsNotACapacityRefusal(t *testing.T) {
	// The API retries a refusal on another runner; a failed build it must not.
	if IsCapacityRefusal(BuildFailure(codes.ResourceExhausted, "no_space", "volume build failed: no_space")) {
		t.Fatal("a failed build reads as a capacity refusal")
	}
}
