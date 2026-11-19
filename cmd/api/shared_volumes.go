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

// newSharedVolumes returns the shared volume service, or nil without a volume
// store. Publishes in progress are interrupted when ctx ends.
func newSharedVolumes(ctx context.Context, cfg *config.APIConfig, sandboxStore store.SandboxStore, objects volumestore.Store, mrec *metrics.APIRecorder) (*volumes.Service, error) {
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
	return volumes.NewService(ctx, records, objects, metrics.NewSharedVolumeRecorder(mrec)), nil
}
