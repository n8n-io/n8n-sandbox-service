// Package runnerctl dials a runner's SandboxControl gRPC (create/delete sandbox).
package runnerctl

import (
	"context"
	"fmt"
	"net"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/n8n-io/sandbox-service/internal/api/grpc/pb"
	"github.com/n8n-io/sandbox-service/internal/grpctls"
)

// TLS holds optional mTLS material for the API dialing a runner (all three PEM paths required when used).
type TLS struct {
	CAFile, CertFile, KeyFile string
	ServerName                string // optional; defaults to dial host
}

func dialOpts(target string, tlsCfg *TLS) ([]grpc.DialOption, error) {
	if tlsCfg == nil || (tlsCfg.CAFile == "" && tlsCfg.CertFile == "" && tlsCfg.KeyFile == "") {
		return []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, nil
	}
	if tlsCfg.CAFile == "" || tlsCfg.CertFile == "" || tlsCfg.KeyFile == "" {
		return nil, fmt.Errorf("runnerctl: CA, cert, and key must all be set for control-plane mTLS")
	}
	serverName := strings.TrimSpace(tlsCfg.ServerName)
	if serverName == "" {
		host, _, err := net.SplitHostPort(target)
		if err != nil {
			return nil, fmt.Errorf("runnerctl: parse target %q: %w", target, err)
		}
		serverName = host
	}
	creds, err := grpctls.NewClientTransportCredentials(tlsCfg.CAFile, tlsCfg.CertFile, tlsCfg.KeyFile, serverName)
	if err != nil {
		return nil, err
	}
	return []grpc.DialOption{grpc.WithTransportCredentials(creds)}, nil
}

func withAPIKey(ctx context.Context, apiKey string) context.Context {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, "x-api-key", apiKey)
}

// CreateSandbox calls SandboxControl.CreateSandbox and closes the connection.
func CreateSandbox(ctx context.Context, target, apiKey string, tlsCfg *TLS, sandboxID, createJSON string) (*pb.CreateSandboxResponse, error) {
	opts, err := dialOpts(target, tlsCfg)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(target, opts...)
	if err != nil {
		return nil, fmt.Errorf("runnerctl dial: %w", err)
	}
	defer conn.Close()

	cli := pb.NewSandboxControlClient(conn)
	return cli.CreateSandbox(withAPIKey(ctx, apiKey), &pb.CreateSandboxRequest{
		SandboxId:  sandboxID,
		CreateJson: createJSON,
	})
}

// DeleteSandbox calls SandboxControl.DeleteSandbox and closes the connection.
func DeleteSandbox(ctx context.Context, target, apiKey string, tlsCfg *TLS, sandboxID string) error {
	opts, err := dialOpts(target, tlsCfg)
	if err != nil {
		return err
	}
	conn, err := grpc.NewClient(target, opts...)
	if err != nil {
		return fmt.Errorf("runnerctl dial: %w", err)
	}
	defer conn.Close()

	cli := pb.NewSandboxControlClient(conn)
	_, err = cli.DeleteSandbox(withAPIKey(ctx, apiKey), &pb.DeleteSandboxRequest{SandboxId: sandboxID})
	return err
}
