package grpcapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
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
		HttpBaseUrl:     "https://runner:8080",
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

// A runner advertising a plaintext base must not enter the registry: the API's
// proxy transport applies its client certificate only to https URLs, so an
// accepted http base would silently downgrade exec and file traffic and put the
// runner API key on the wire.
func TestConnectRejectsPlaintextHTTPBaseURL(t *testing.T) {
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
		HttpBaseUrl:     "http://runner:8080",
		ControlGrpcAddr: "runner:9091",
		Healthy:         true,
		CapacityTotal:   10,
		CapacityUsed:    0,
	}
	if err := stream.Send(hb); err != nil {
		assertInvalidArgument(t, err)
	} else {
		if _, err := stream.Recv(); err == nil {
			t.Fatal("expected error for plaintext http_base_url")
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
		HttpBaseUrl:   "https://runner:8080",
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

// fakeRegistrationStream feeds one heartbeat to Connect and then ends the stream.
type fakeRegistrationStream struct {
	grpc.ServerStream
	ctx  context.Context
	sent bool
}

func (s *fakeRegistrationStream) Context() context.Context { return s.ctx }

func (s *fakeRegistrationStream) Recv() (*pb.RunnerHeartbeat, error) {
	if s.sent {
		return nil, io.EOF
	}
	s.sent = true
	return testHeartbeat("r1"), nil
}

func (s *fakeRegistrationStream) Send(*pb.ControlMessage) error { return nil }

func TestConnectLogsTheRunnerClientCertificate(t *testing.T) {
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	notAfter := time.Date(2027, 1, 20, 8, 39, 49, 0, time.UTC)
	cert := &x509.Certificate{SerialNumber: big.NewInt(0x0853fb42), NotAfter: notAfter}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer test-token"))
	ctx = peer.NewContext(ctx, &peer.Peer{
		AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}},
	})

	srv := &RunnerRegistryServer{Token: "test-token", Reg: registry.New(time.Hour)}
	if err := srv.Connect(&fakeRegistrationStream{ctx: ctx}); err != nil {
		t.Fatal(err)
	}

	for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		var ev map[string]any
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Fatal(err)
		}
		if ev["msg"] != "runner registered" {
			continue
		}
		if ev["client_cert_serial"] != "0853FB42" || ev["client_cert_not_after"] != "2027-01-20T08:39:49Z" {
			t.Fatalf("runner registered without the client certificate: %v", ev)
		}
		return
	}
	t.Fatalf("no runner registered event in:\n%s", buf.String())
}
