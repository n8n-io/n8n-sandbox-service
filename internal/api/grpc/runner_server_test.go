package grpcapi

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/n8n-io/sandbox-service/internal/api/grpc/pb"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
)

func assertInvalidArgument(t *testing.T, err error) {
	t.Helper()
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

func testHeartbeat(runnerID string) *pb.RunnerHeartbeat {
	return &pb.RunnerHeartbeat{
		RunnerId:        runnerID,
		HttpBaseUrl:     "http://runner:8080",
		ControlGrpcAddr: "runner:9091",
		Healthy:         true,
		CapacityTotal:   10,
		CapacityUsed:    0,
	}
}

func TestConnectRejectsInvalidHTTPBaseURL(t *testing.T) {
	reg := registry.New(time.Hour)
	token := "test-token"
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	pb.RegisterRunnerRegistryServer(srv, &RunnerRegistryServer{Token: token, Reg: reg})
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+token)
	stream, err := pb.NewRunnerRegistryClient(conn).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}

	hb := &pb.RunnerHeartbeat{
		RunnerId:        "r1",
		HttpBaseUrl:     "ftp://host",
		ControlGrpcAddr: "runner:9091",
		Healthy:         true,
		CapacityTotal:   10,
		CapacityUsed:    0,
	}
	if err := stream.Send(hb); err != nil {
		assertInvalidArgument(t, err)
	} else {
		if _, err := stream.Recv(); err == nil {
			t.Fatal("expected error for invalid http_base_url")
		} else {
			assertInvalidArgument(t, err)
		}
	}
	if _, err := reg.PickRoundRobin(); !errors.Is(err, registry.ErrNoRunners) {
		t.Fatalf("registry: want ErrNoRunners, got %v", err)
	}
}

func TestConnectRunnerIDCannotChange(t *testing.T) {
	reg := registry.New(time.Hour)
	token := "test-token"
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	pb.RegisterRunnerRegistryServer(srv, &RunnerRegistryServer{Token: token, Reg: reg})
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+token)
	stream, err := pb.NewRunnerRegistryClient(conn).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if err := stream.Send(testHeartbeat("runner-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	// Application errors can surface on the next Send or Recv depending on flow control.
	if err := stream.Send(testHeartbeat("runner-b")); err != nil {
		assertInvalidArgument(t, err)
	} else {
		if _, err := stream.Recv(); err == nil {
			t.Fatal("expected error after runner_id change")
		} else {
			assertInvalidArgument(t, err)
		}
	}

	if _, err := reg.PickRoundRobin(); !errors.Is(err, registry.ErrNoRunners) {
		t.Fatalf("registry should have no runners after failed stream; err=%v", err)
	}
}

func TestConnectEmptyRunnerIDAfterCommitUsesCommittedID(t *testing.T) {
	reg := registry.New(time.Hour)
	token := "test-token"
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	pb.RegisterRunnerRegistryServer(srv, &RunnerRegistryServer{Token: token, Reg: reg})
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+token)
	stream, err := pb.NewRunnerRegistryClient(conn).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.CloseSend()

	if err := stream.Send(testHeartbeat("stable-id")); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	// Omitted / empty runner_id must not clear the stream binding (or leak cleanup).
	if err := stream.Send(testHeartbeat("")); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}

	if _, err := reg.PickRoundRobin(); err != nil {
		t.Fatalf("expected runner still registered: %v", err)
	}
}

func TestConnectRequiresControlGRPCAddr(t *testing.T) {
	reg := registry.New(time.Hour)
	token := "test-token"
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	pb.RegisterRunnerRegistryServer(srv, &RunnerRegistryServer{Token: token, Reg: reg})
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+token)
	stream, err := pb.NewRunnerRegistryClient(conn).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}

	hb := &pb.RunnerHeartbeat{
		RunnerId:      "r1",
		HttpBaseUrl:   "http://runner:8080",
		Healthy:       true,
		CapacityTotal: 10,
		CapacityUsed:  0,
	}
	if err := stream.Send(hb); err != nil {
		assertInvalidArgument(t, err)
	} else {
		if _, err := stream.Recv(); err == nil {
			t.Fatal("expected error for missing control_grpc_addr")
		} else {
			assertInvalidArgument(t, err)
		}
	}
}
