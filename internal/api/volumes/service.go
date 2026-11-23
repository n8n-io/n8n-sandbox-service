package volumes

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/api/volumestore"
	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/safetar"
	"github.com/n8n-io/sandbox-service/internal/volume"
)

// timing holds the durations of a publish's lease and of cleanups.
type timing struct {
	// lease is how long a publishing row is held without a renewal.
	lease time.Duration
	// renewEvery is how often a publish renews its lease.
	renewEvery time.Duration
	// giveUp ends a publish that could not renew for this long, before
	// another publish may take its row over.
	giveUp time.Duration
	// cleanup bounds the removal of what a failed publish or a delete left.
	cleanup time.Duration
}

// limits are the caps a publish is held to: the package's constants, smaller
// in tests.
type limits struct {
	maxTarBytes int64
	policy      safetar.Policy
	// maxLiveVolumes counts the publishing and ready shared volumes of the
	// whole service, not of one API pod.
	maxLiveVolumes int
}

var defaultTiming = timing{
	lease:      2 * time.Minute,
	renewEvery: 30 * time.Second,
	giveUp:     90 * time.Second,
	cleanup:    30 * time.Second,
}

// Role is the role of a key that may change shared volumes.
type Role string

const (
	RoleAdmin           Role = "admin"
	RoleVolumePublisher Role = "volume_publisher"
)

// Service publishes, lists and deletes shared volumes. A row is written before
// any of its objects and purged after them, so every object has a row.
type Service struct {
	records store.SharedVolumes
	objects volumestore.Store
	metrics *metrics.SharedVolumeRecorder
	// ctx ends at shutdown, which interrupts every publish.
	ctx context.Context
	// runningPublishes holds a token for each publish in progress on this API
	// pod, at most MaxPublishesPerPod.
	runningPublishes chan struct{}
	now              func() time.Time
	newID            func() string
	timing           timing
	limits           limits
}

// NewService keeps shared volume rows in records and their objects in objects.
// Publishes in progress are interrupted when ctx ends.
func NewService(ctx context.Context, records store.SharedVolumes, objects volumestore.Store, rec *metrics.SharedVolumeRecorder) *Service {
	return &Service{
		records:          records,
		objects:          objects,
		metrics:          rec,
		ctx:              ctx,
		runningPublishes: make(chan struct{}, MaxPublishesPerPod),
		now:              time.Now,
		newID:            func() string { return uuid.New().String() },
		timing:           defaultTiming,
		limits:           limits{maxTarBytes: MaxTarBytes, policy: tarPolicy(), maxLiveVolumes: MaxSharedVolumes},
	}
}

// Get returns the ready shared volume with the sha256, or ErrNotFound.
func (s *Service) Get(ctx context.Context, sha256 string) (*store.SharedVolume, error) {
	row, err := s.records.GetLiveSharedVolume(ctx, sha256)
	if err != nil {
		return nil, err
	}
	if row == nil || row.State != store.SharedVolumeReady {
		return nil, ErrNotFound
	}
	return row, nil
}

// List returns the ready shared volumes, newest first; only the one with the
// name when name is set.
func (s *Service) List(ctx context.Context, name string) ([]*store.SharedVolume, error) {
	if name != "" && !ValidName(name) {
		return nil, ErrInvalidName
	}
	return s.records.ListReadySharedVolumes(ctx, name)
}

// Delete hides the ready shared volume with the sha256 and frees its name at
// once, then purges it. If the purge fails, the row stays deleted with what is
// left of it. It returns ErrNotFound, or ErrPublishInProgress while the sha256
// is being published.
func (s *Service) Delete(ctx context.Context, sha256 string, role Role) error {
	row, err := s.records.MarkSharedVolumeDeleted(ctx, sha256)
	if err != nil {
		return err
	}
	switch {
	case row == nil:
		return ErrNotFound
	case row.State == store.SharedVolumePublishing:
		return ErrPublishInProgress
	case row.State != store.SharedVolumeDeleted:
		return fmt.Errorf("delete of shared volume %s returned a %s row", row.ID, row.State)
	}
	slog.InfoContext(ctx, "shared volume deleted", "version_id", row.ID, "sha256", row.SHA256, "name", row.Name, "role", role)
	purgeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.timing.cleanup)
	defer cancel()
	if err := s.purge(purgeCtx, row.ID); err != nil {
		slog.WarnContext(ctx, "shared volume purge failed; the row stays deleted", "version_id", row.ID, "sha256", row.SHA256, "error", err)
	}
	return nil
}

// purge deletes the objects of the failed or deleted shared volume id, then
// its row. It is the one place that removes a volume.
func (s *Service) purge(ctx context.Context, id string) error {
	for _, kind := range []volume.Kind{volume.KindTar, volume.KindExt4} {
		if err := s.objects.Delete(ctx, objectKey(id, kind)); err != nil {
			return err
		}
	}
	_, err := s.records.PurgeSharedVolume(ctx, id)
	return err
}

func objectKey(id string, kind volume.Kind) volumestore.Key {
	return volumestore.Key{Scope: volume.SharedScope, ID: id, Kind: kind}
}
