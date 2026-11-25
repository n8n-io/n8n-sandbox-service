// This file creates the shared volume service of the API.

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

var errStoreKeepsNoSharedVolumes = errors.New("the API store keeps no shared volumes")

// newSharedVolumeService returns the shared volume service, or nil without a
// volume store. Ending shutdownCtx interrupts the publishes in progress. When
// the runners of the service attach images, each publish has one of them build
// its image.
func newSharedVolumeService(shutdownCtx context.Context, cfg *config.APIConfig, sandboxStore store.SandboxStore, volumeStore volumestore.Store, runners registry.RunnerRegistry, apiRecorder *metrics.APIRecorder) (*volumes.Service, error) {
	if volumeStore == nil {
		if len(cfg.VolumePublisherKeys) > 0 {
			slog.Warn("SANDBOX_API_VOLUME_PUBLISHER_KEYS is set but SANDBOX_API_VOLUME_STORE is not: the shared volume routes answer 501")
		}
		return nil, nil
	}
	sharedVolumeStore, ok := sandboxStore.(store.SharedVolumes)
	if !ok {
		return nil, errStoreKeepsNoSharedVolumes
	}
	service := volumes.NewService(shutdownCtx, sharedVolumeStore, volumeStore, metrics.NewSharedVolumeRecorder(apiRecorder))
	if cfg.RunnerVolumeKind == volume.KindExt4 {
		service.EnableImageBuilds(volumes.Builds{Runners: runners, Build: newRunnerBuildFunc(cfg), HeartbeatGrace: cfg.HeartbeatGrace})
		slog.Info("shared volume images are built on runners", "runner_volume_kind", cfg.RunnerVolumeKind)
	}
	return service, nil
}

// newRunnerBuildFunc returns a BuildFunc that calls a runner's BuildVolume with
// the API's runner key and control-plane client certificate.
func newRunnerBuildFunc(cfg *config.APIConfig) volumes.BuildFunc {
	tlsConfig := &runnerctl.TLS{
		CAFile:     cfg.RunnerControlGRPCClientCAFile,
		CertFile:   cfg.RunnerControlGRPCClientCertFile,
		KeyFile:    cfg.RunnerControlGRPCClientKeyFile,
		ServerName: cfg.RunnerControlGRPCClientServerName,
	}
	return func(ctx context.Context, runner registry.Runner, header *pb.BuildVolumeHeader, tar io.Reader, image io.Writer) (runnerctl.VolumeImage, error) {
		return runnerctl.BuildVolume(ctx, runner.ControlGRPCAddr, cfg.RunnerAPIKey, tlsConfig, header, tar, image)
	}
}
