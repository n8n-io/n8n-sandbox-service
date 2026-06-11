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
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// Run maintains a registration stream to the API until ctx is cancelled.
func Run(ctx context.Context, cfg *config.Config, rt runnerruntime.Runtime) {
	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		err := connectOnce(ctx, cfg, rt)
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

func connectOnce(ctx context.Context, cfg *config.Config, rt runnerruntime.Runtime) error {
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
		capacity, err := rt.Capacity(ctx)
		if err != nil {
			slog.Debug("runtime capacity failed", "error", err)
			capacity = runnerruntime.Capacity{Total: cfg.CapacityTotal}
		}
		healthy := true
		if err := rt.Ready(ctx); err != nil {
			healthy = false
		}
		hb := &pb.RunnerHeartbeat{
			RunnerId:        cfg.RunnerID,
			HttpBaseUrl:     cfg.RunnerHTTPBaseURL,
			Healthy:         healthy,
			CapacityTotal:   capacity.Total,
			CapacityUsed:    capacity.Used,
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

	readyCh := rt.ReadyCh()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errCh:
			return err
		case <-readyCh:
			readyCh = nil
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
