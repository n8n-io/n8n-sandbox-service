package grpctls

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

// CertSerial formats a certificate's serial number like `openssl x509 -serial`
// does, so log lines can be compared with what openssl shows for a file.
func CertSerial(c *x509.Certificate) string {
	if c == nil || c.SerialNumber == nil {
		return ""
	}
	return fmt.Sprintf("%X", c.SerialNumber.Bytes())
}

func fingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	return string(sum[:])
}

// PeerCertificate returns the leaf certificate the peer of a gRPC call presented
// in the TLS handshake, or nil.
func PeerCertificate(ctx context.Context) *x509.Certificate {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(info.State.PeerCertificates) == 0 {
		return nil
	}
	return info.State.PeerCertificates[0]
}

// NotePeerUnaryClientInterceptor passes the server certificate of every call
// that succeeded to NotePeer.
func NotePeerUnaryClientInterceptor(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	var p peer.Peer
	if err := invoker(ctx, method, req, reply, cc, append(opts, grpc.Peer(&p))...); err != nil {
		return err
	}
	if info, ok := p.AuthInfo.(credentials.TLSInfo); ok && len(info.State.PeerCertificates) > 0 {
		NotePeer(info.State.PeerCertificates[0])
	}
	return nil
}

// maxSeenPeers bounds the peers remembered so far. A deployment has one entry
// per peer identity (one per runner at most), so it only fills up in a process
// that has seen thousands of identities; the set then starts over.
const maxSeenPeers = 4096

// seenPeers maps a peer identity (subject and issuer) to the fingerprint of the
// certificate last logged for it.
var seenPeers = struct {
	mu sync.Mutex
	fp map[string]string
}{fp: map[string]string{}}

// NotePeer logs c the first time this process sees it for its subject and
// reports whether it did, so a renewal on the other side of a connection shows
// up in this process's log. Call it only with the certificate of a peer that
// completed the handshake and authenticated: tls.Config.VerifyConnection runs
// before the peer has proven it holds the key.
func NotePeer(c *x509.Certificate) bool {
	if c == nil {
		return false
	}
	id := c.Subject.String() + "\x00" + c.Issuer.String()
	fp := fingerprint(c)
	seenPeers.mu.Lock()
	if seenPeers.fp[id] == fp {
		seenPeers.mu.Unlock()
		return false
	}
	if len(seenPeers.fp) >= maxSeenPeers {
		clear(seenPeers.fp)
	}
	seenPeers.fp[id] = fp
	seenPeers.mu.Unlock()

	slog.Info("tls peer certificate first seen",
		"subject", c.Subject.CommonName,
		"serial", CertSerial(c),
		"not_after", c.NotAfter.UTC().Format(time.RFC3339))
	return true
}
