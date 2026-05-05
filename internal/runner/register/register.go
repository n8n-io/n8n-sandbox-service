package register

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/n8n-io/sandbox-service/internal/api/grpc/pb"
	"github.com/n8n-io/sandbox-service/internal/grpctls"
	"github.com/n8n-io/sandbox-service/internal/runner/config"
	"github.com/n8n-io/sandbox-service/internal/runner/manager"
)

// Run maintains a registration stream to the API until ctx is cancelled.
func Run(ctx context.Context, cfg *config.Config, mgr *manager.Manager) {
	if cfg.APIGRPCAddr == "" || cfg.RegistrationToken == "" {
		slog.Warn("runner registration skipped (set SANDBOX_RUNNER_API_GRPC_ADDR and SANDBOX_RUNNER_REGISTRATION_TOKEN)")
		return
	}

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
	var dialOpts []grpc.DialOption
	if cfg.GRPCServerCAFile != "" {
		serverName := cfg.GRPCServerName
		if serverName == "" {
			host, _, err := net.SplitHostPort(cfg.APIGRPCAddr)
			if err != nil {
				return err
			}
			serverName = host
		}
		creds, err := grpctls.NewClientTransportCredentials(
			cfg.GRPCServerCAFile,
			cfg.GRPCClientCertFile,
			cfg.GRPCClientKeyFile,
			serverName,
		)
		if err != nil {
			return err
		}
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(creds))
	} else {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

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
			RunnerId:      cfg.RunnerID,
			HttpBaseUrl:   cfg.RunnerHTTPBaseURL,
			Healthy:       true,
			CapacityTotal: cfg.CapacityTotal,
			CapacityUsed:  int32(n),
		}
		return stream.Send(hb)
	}

	if err := send(); err != nil {
		return err
	}

	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errCh:
			return err
		case <-tick.C:
			if err := send(); err != nil {
				return err
			}
		}
	}
}
