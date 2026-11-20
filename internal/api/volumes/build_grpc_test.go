package volumes

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	"github.com/n8n-io/sandbox-service/internal/api/grpc/pb"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/runnerctl"
	"github.com/n8n-io/sandbox-service/internal/grpctls"
)

// controlPKI is a CA, a runner's server certificate for localhost and the
// API's client certificate, as files.
type controlPKI struct {
	caFile, serverCert, serverKey, clientCert, clientKey string
}

func newControlPKI(t *testing.T) controlPKI {
	t.Helper()
	dir := t.TempDir()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "control-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	pki := controlPKI{
		caFile:     filepath.Join(dir, "ca.crt"),
		serverCert: filepath.Join(dir, "server.crt"), serverKey: filepath.Join(dir, "server.key"),
		clientCert: filepath.Join(dir, "client.crt"), clientKey: filepath.Join(dir, "client.key"),
	}
	writePEM(t, pki.caFile, "CERTIFICATE", caDER)
	for serial, leaf := range []struct {
		usage             x509.ExtKeyUsage
		certFile, keyFile string
	}{
		{x509.ExtKeyUsageServerAuth, pki.serverCert, pki.serverKey},
		{x509.ExtKeyUsageClientAuth, pki.clientCert, pki.clientKey},
	} {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		template := &x509.Certificate{
			SerialNumber: big.NewInt(int64(serial + 2)), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{leaf.usage},
		}
		der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		writePEM(t, leaf.certFile, "CERTIFICATE", der)
		writePEM(t, leaf.keyFile, "PRIVATE KEY", keyDER)
	}
	return pki
}

func writePEM(t *testing.T, path, kind string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

// controlServer is a runner's SandboxControl that answers builds with refusal,
// or with failure after the tar, or else with image.
type controlServer struct {
	pb.UnimplementedSandboxControlServer
	refusal error
	failure error
	image   []byte
	header  *pb.BuildVolumeHeader
	tar     []byte
}

func (server *controlServer) BuildVolume(stream grpc.BidiStreamingServer[pb.BuildVolumeRequest, pb.BuildVolumeResponse]) error {
	if server.refusal != nil {
		return server.refusal
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	server.header = first.GetHeader()
	for {
		message, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		server.tar = append(server.tar, message.GetTarChunk()...)
	}
	if server.failure != nil {
		return server.failure
	}
	if err := stream.Send(&pb.BuildVolumeResponse{Part: &pb.BuildVolumeResponse_ImageBytes{ImageBytes: int64(len(server.image))}}); err != nil {
		return err
	}
	const chunkBytes = 1 << 20 // 1 MiB, as a runner sends
	for start := 0; start < len(server.image); start += chunkBytes {
		chunk := server.image[start:min(start+chunkBytes, len(server.image))]
		if err := stream.Send(&pb.BuildVolumeResponse{Part: &pb.BuildVolumeResponse_ImageChunk{ImageChunk: chunk}}); err != nil {
			return err
		}
	}
	digest := sha256.Sum256(server.image)
	return stream.Send(&pb.BuildVolumeResponse{Part: &pb.BuildVolumeResponse_ImageSha256{ImageSha256: hex.EncodeToString(digest[:])}})
}

// serveControl serves server over mTLS and returns a runner registered at it.
func serveControl(t *testing.T, pki controlPKI, id string, used int32, server *controlServer) registry.Runner {
	t.Helper()
	creds, err := grpctls.NewServerTransportCredentials(pki.serverCert, pki.serverKey, pki.caFile)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	grpcServer := grpc.NewServer(grpc.Creds(creds))
	pb.RegisterSandboxControlServer(grpcServer, server)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	runner := builder(id, used)
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	runner.ControlGRPCAddr = net.JoinHostPort("localhost", port)
	return runner
}

// The build as cmd/api wires it: runnerctl.BuildVolume over the runner's
// control channel.
func TestBuildOverTheControlChannel(t *testing.T) {
	pki := newControlPKI(t)
	tlsCfg := &runnerctl.TLS{CAFile: pki.caFile, CertFile: pki.clientCert, KeyFile: pki.clientKey}
	build := func(ctx context.Context, runner registry.Runner, header *pb.BuildVolumeHeader, tar io.Reader, image io.Writer) (runnerctl.VolumeImage, error) {
		return runnerctl.BuildVolume(ctx, runner.ControlGRPCAddr, "", tlsCfg, header, tar, image)
	}
	image := testImage()
	busy := &controlServer{refusal: runnerctl.CapacityRefusal("a volume build is already running")}
	building := &controlServer{image: append(image, image...)}
	env := newTestEnv(t).withBuilds(build, serveControl(t, pki, "busy", 0, busy), serveControl(t, pki, "building", 1, building))
	data := validTar(t)

	published, err := env.publish(t, data, "")

	if err != nil {
		t.Fatalf("Publish() = %v", err)
	}
	if string(building.tar) != string(data) || building.header.GetTarBytes() != int64(len(data)) {
		t.Fatalf("the runner got %d tar bytes and header %+v, want the %d published", len(building.tar), building.header, len(data))
	}
	if published.Volume.ImageSHA256 != digestOf(building.image) || published.Volume.ImageBytes != int64(len(building.image)) {
		t.Fatalf("image = %d bytes, %s; want the runner's", published.Volume.ImageBytes, published.Volume.ImageSHA256)
	}

	failing := &controlServer{failure: runnerctl.BuildFailure(codes.InvalidArgument, "tar", "volume build failed: tar")}
	env = newTestEnv(t).withBuilds(build, serveControl(t, pki, "failing", 0, failing))
	_, err = env.publish(t, data, "")
	var failure *BuildError
	if !errors.As(err, &failure) || failure.Reason != "tar" || !failure.CallerFault() {
		t.Fatalf("Publish() = %v, want a build that failed for the tar", err)
	}
	env.assertNothingLeft(t)
}
