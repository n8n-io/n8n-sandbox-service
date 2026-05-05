package grpctls

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"google.golang.org/grpc/credentials"
)

// NewClientTransportCredentials builds mTLS client credentials for dialing the API registry.
// serverCAFile must contain PEM certificate(s) for the CA that signed the API server certificate.
// serverName is used for certificate verification (SNI / hostname); may be empty to use the dial target host.
func NewClientTransportCredentials(serverCAFile, clientCertFile, clientKeyFile, serverName string) (credentials.TransportCredentials, error) {
	caPEM, err := os.ReadFile(serverCAFile)
	if err != nil {
		return nil, fmt.Errorf("grpctls: read server CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("grpctls: no PEM certificates in %s", serverCAFile)
	}

	reloader := &KeyPairReloader{CertPath: clientCertFile, KeyPath: clientKeyFile}
	if err := reloader.Prime(); err != nil {
		return nil, fmt.Errorf("grpctls: load client key pair: %w", err)
	}

	tlsConf := &tls.Config{
		RootCAs:              pool,
		ServerName:           serverName,
		GetClientCertificate: reloader.GetClientCertificate,
		MinVersion:           tls.VersionTLS12,
	}
	return credentials.NewTLS(tlsConf), nil
}
