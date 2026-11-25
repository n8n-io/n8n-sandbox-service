// This file creates the shared volume service of the API.

package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/api/volumes"
	"github.com/n8n-io/sandbox-service/internal/api/volumestore"
	"github.com/n8n-io/sandbox-service/internal/metrics"
)

var errStoreKeepsNoSharedVolumes = errors.New("the API store keeps no shared volumes")

// newSharedVolumeService returns the shared volume service, or nil without a
// volume store. Ending shutdownCtx interrupts the publishes in progress.
func newSharedVolumeService(shutdownCtx context.Context, cfg *config.APIConfig, sandboxStore store.SandboxStore, volumeStore volumestore.Store, apiRecorder *metrics.APIRecorder) (*volumes.Service, error) {
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
	return volumes.NewService(shutdownCtx, sharedVolumeStore, volumeStore, metrics.NewSharedVolumeRecorder(apiRecorder)), nil
}
