package grpctls

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"google.golang.org/grpc/credentials"
)

// NewServerTLSConfig builds an mTLS server config that presents serverCertFile
// and verifies client certificates against clientCAFile.
//
// clientAuth selects how strict that verification is. Use
// tls.RequireAndVerifyClientCert to reject an unauthenticated peer during the
// handshake. Use tls.VerifyClientCertIfGiven when some routes must stay
// reachable without a certificate, such as health probes that cannot present
// one; the handler is then responsible for requiring a peer certificate on
// every route that needs it.
func NewServerTLSConfig(serverCertFile, serverKeyFile, clientCAFile string, clientAuth tls.ClientAuthType) (*tls.Config, error) {
	caPEM, err := os.ReadFile(clientCAFile)
	if err != nil {
		return nil, fmt.Errorf("grpctls: read client CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("grpctls: no PEM certificates in %s", clientCAFile)
	}

	reloader := &KeyPairReloader{CertPath: serverCertFile, KeyPath: serverKeyFile}
	if err := reloader.Prime(); err != nil {
		return nil, fmt.Errorf("grpctls: load server key pair: %w", err)
	}

	return &tls.Config{
		ClientAuth:     clientAuth,
		ClientCAs:      pool,
		GetCertificate: reloader.GetCertificate,
		MinVersion:     tls.VersionTLS12,
	}, nil
}

// NewServerTransportCredentials builds mTLS server credentials for the runner registry.
// clientCAFile must contain PEM certificate(s) for the CA that signs runner client certificates.
func NewServerTransportCredentials(serverCertFile, serverKeyFile, clientCAFile string) (credentials.TransportCredentials, error) {
	tlsConf, err := NewServerTLSConfig(serverCertFile, serverKeyFile, clientCAFile, tls.RequireAndVerifyClientCert)
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(tlsConf), nil
}
