package main

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/grpc/pb"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/runnerctl"
	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/api/volumes"
	"github.com/n8n-io/sandbox-service/internal/api/volumestore"
	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/volume"
)

// newSharedVolumes returns the shared volume service, or nil without a volume
// store. Publishes in progress are interrupted when ctx ends. When the
// service's runners attach images, each publish has one of runners build its
// image.
func newSharedVolumes(ctx context.Context, cfg *config.APIConfig, sandboxStore store.SandboxStore, objects volumestore.Store, runners registry.RunnerRegistry, mrec *metrics.APIRecorder) (*volumes.Service, error) {
	if objects == nil {
		if len(cfg.PublisherKeys) > 0 {
			slog.Warn("SANDBOX_API_PUBLISHER_KEYS is set but SANDBOX_API_VOLUME_STORE is not: the shared volume routes answer 501")
		}
		return nil, nil
	}
	records, ok := sandboxStore.(store.SharedVolumes)
	if !ok {
		return nil, errors.New("the API store keeps no shared volumes")
	}
	service := volumes.NewService(ctx, records, objects, metrics.NewSharedVolumeRecorder(mrec))
	if cfg.RunnerVolumeKind == volume.KindExt4 {
		service.BuildImages(volumes.Builds{Runners: runners, Build: runnerBuild(cfg), HeartbeatGrace: cfg.HeartbeatGrace})
		slog.Info("shared volume images are built on runners", "runner_volume_kind", cfg.RunnerVolumeKind)
	}
	return service, nil
}

// runnerBuild calls a runner's BuildVolume with the API's runner key and
// control-plane client certificate.
func runnerBuild(cfg *config.APIConfig) volumes.BuildFunc {
	tlsCfg := &runnerctl.TLS{
		CAFile:     cfg.RunnerControlGRPCClientCAFile,
		CertFile:   cfg.RunnerControlGRPCClientCertFile,
		KeyFile:    cfg.RunnerControlGRPCClientKeyFile,
		ServerName: cfg.RunnerControlGRPCClientServerName,
	}
	return func(ctx context.Context, runner registry.Runner, header *pb.BuildVolumeHeader, tar io.Reader, image io.Writer) (runnerctl.VolumeImage, error) {
		return runnerctl.BuildVolume(ctx, runner.ControlGRPCAddr, cfg.RunnerAPIKey, tlsCfg, header, tar, image)
	}
}
