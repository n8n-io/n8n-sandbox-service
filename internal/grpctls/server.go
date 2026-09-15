// Package grpctls builds the mTLS configurations shared by the gRPC and HTTP
// listeners and clients of the API and the runner.
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
// clientAuth selects how strict that verification is: RequireAndVerifyClientCert
// rejects an unauthenticated peer in the handshake; with VerifyClientCertIfGiven
// the handler must require a peer certificate on every route that needs one.
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

// NewServerTransportCredentials builds mTLS server credentials that require a
// verified client certificate. clientCAFile must contain PEM certificate(s) for
// the CA that signs the peer's client certificates.
func NewServerTransportCredentials(serverCertFile, serverKeyFile, clientCAFile string) (credentials.TransportCredentials, error) {
	tlsConf, err := NewServerTLSConfig(serverCertFile, serverKeyFile, clientCAFile, tls.RequireAndVerifyClientCert)
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(tlsConf), nil
}
