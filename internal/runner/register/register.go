package register

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/n8n-io/sandbox-service/internal/api/grpc/pb"
	"github.com/n8n-io/sandbox-service/internal/grpctls"
	"github.com/n8n-io/sandbox-service/internal/runner/config"
	"github.com/n8n-io/sandbox-service/internal/runner/manager"
)

// Run maintains a registration stream to the API until ctx is cancelled.
func Run(ctx context.Context, cfg *config.Config, mgr *manager.Manager) {
	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		err := connectOnce(ctx, cfg, mgr)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("runner registration stream ended", "error", err, "retry_in", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
				if backoff > 30*time.Second {
					backoff = 30 * time.Second
				}
			}
			continue
		}
		backoff = time.Second
	}
}

func connectOnce(ctx context.Context, cfg *config.Config, mgr *manager.Manager) error {
	serverName := cfg.GRPCServerName
	if serverName == "" {
		host, _, err := net.SplitHostPort(cfg.APIGRPCAddr)
		if err != nil {
			return err
		}
		serverName = host
	}
	slog.Info(
		"runner registration connecting",
		"runner_id", cfg.RunnerID,
		"api_grpc_addr", cfg.APIGRPCAddr,
		"server_name", serverName,
		"http_base_url", cfg.RunnerHTTPBaseURL,
		"control_grpc_addr", cfg.ResolvedControlGRPCAdvertiseAddr(),
	)
	creds, err := grpctls.NewClientTransportCredentials(
		cfg.GRPCServerCAFile,
		cfg.GRPCClientCertFile,
		cfg.GRPCClientKeyFile,
		serverName,
	)
	if err != nil {
		return err
	}
	dialOpts := []grpc.DialOption{grpc.WithTransportCredentials(creds)}

	conn, err := grpc.NewClient(cfg.APIGRPCAddr, dialOpts...)
	if err != nil {
		return err
	}
	defer conn.Close()

	mdCtx := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+cfg.RegistrationToken)
	client := pb.NewRunnerRegistryClient(conn)
	stream, err := client.Connect(mdCtx)
	if err != nil {
		return err
	}
	slog.Info("runner registration stream established", "runner_id", cfg.RunnerID, "api_grpc_addr", cfg.APIGRPCAddr)

	errCh := make(chan error, 1)
	go func() {
		for {
			_, err := stream.Recv()
			if err != nil {
				if errors.Is(err, io.EOF) {
					errCh <- nil
					return
				}
				errCh <- err
				return
			}
		}
	}()

	send := func() error {
		n, err := mgr.ManagedContainerCount(ctx)
		if err != nil {
			slog.Debug("managed container count failed", "error", err)
			n = 0
		}
		hb := &pb.RunnerHeartbeat{
			RunnerId:        cfg.RunnerID,
			HttpBaseUrl:     cfg.RunnerHTTPBaseURL,
			Healthy:         mgr.ImageReady(),
			CapacityTotal:   cfg.CapacityTotal,
			CapacityUsed:    int32(n),
			ControlGrpcAddr: cfg.ResolvedControlGRPCAdvertiseAddr(),
		}
		return stream.Send(hb)
	}

	if err := send(); err != nil {
		return err
	}
	slog.Info(
		"runner registration heartbeat sent",
		"runner_id", cfg.RunnerID,
		"http_base_url", cfg.RunnerHTTPBaseURL,
		"control_grpc_addr", cfg.ResolvedControlGRPCAdvertiseAddr(),
		"capacity_total", cfg.CapacityTotal,
	)

	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()

	imageReadyCh := mgr.ImageReadyCh()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errCh:
			return err
		case <-imageReadyCh:
			imageReadyCh = nil
			if err := send(); err != nil {
				return err
			}
		case <-tick.C:
			if err := send(); err != nil {
				return err
			}
		}
	}
}
