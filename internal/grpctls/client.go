package grpctls

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"google.golang.org/grpc/credentials"
)

// NewClientTLSConfig builds an mTLS client config that presents clientCertFile
// and verifies the server against serverCAFile.
// serverName is used for certificate verification (SNI / hostname); may be empty to use the dial target host.
func NewClientTLSConfig(serverCAFile, clientCertFile, clientKeyFile, serverName string) (*tls.Config, error) {
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

	return &tls.Config{
		RootCAs:              pool,
		ServerName:           serverName,
		GetClientCertificate: reloader.GetClientCertificate,
		MinVersion:           tls.VersionTLS12,
	}, nil
}

// NewClientTransportCredentials builds mTLS client credentials for dialing the API registry.
// serverCAFile must contain PEM certificate(s) for the CA that signed the API server certificate.
// serverName is used for certificate verification (SNI / hostname); may be empty to use the dial target host.
func NewClientTransportCredentials(serverCAFile, clientCertFile, clientKeyFile, serverName string) (credentials.TransportCredentials, error) {
	tlsConf, err := NewClientTLSConfig(serverCAFile, clientCertFile, clientKeyFile, serverName)
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(tlsConf), nil
}
