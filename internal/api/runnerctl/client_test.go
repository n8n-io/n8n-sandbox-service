package runnerctl

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"

	"github.com/n8n-io/sandbox-service/internal/obs"
)

func TestWithCallMetadataForwardsTrace(t *testing.T) {
	const traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	ctx := withCallMetadata(obs.WithTraceparent(context.Background(), traceparent), "runner-key")

	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		t.Fatal("outgoing context has no metadata")
	}
	if got := md.Get(obs.HeaderTraceparent); len(got) != 1 || got[0] != traceparent {
		t.Fatalf("traceparent metadata = %v, want [%s]", got, traceparent)
	}
	if got := md.Get("x-api-key"); len(got) != 1 || got[0] != "runner-key" {
		t.Fatalf("x-api-key metadata = %v", got)
	}
}

func TestWithCallMetadataWithoutTrace(t *testing.T) {
	ctx := withCallMetadata(context.Background(), "runner-key")

	md, _ := metadata.FromOutgoingContext(ctx)
	if got := md.Get(obs.HeaderTraceparent); len(got) != 0 {
		t.Fatalf("traceparent metadata = %v, want none for an untraced call", got)
	}
}
