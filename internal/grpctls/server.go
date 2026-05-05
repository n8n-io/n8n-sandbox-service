package grpctls

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"google.golang.org/grpc/credentials"
)

// NewServerTransportCredentials builds mTLS server credentials for the runner registry.
// clientCAFile must contain PEM certificate(s) for the CA that signs runner client certificates.
func NewServerTransportCredentials(serverCertFile, serverKeyFile, clientCAFile string) (credentials.TransportCredentials, error) {
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

	tlsConf := &tls.Config{
		ClientAuth:     tls.RequireAndVerifyClientCert,
		ClientCAs:      pool,
		GetCertificate: reloader.GetCertificate,
		MinVersion:     tls.VersionTLS12,
	}
	return credentials.NewTLS(tlsConf), nil
}
