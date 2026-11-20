package volumes

import (
	"context"
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
	// builderWait is how long a build waits for a runner that takes it,
	// trying again after retryFirst, then twice as long each time up to
	// retryMax.
	builderWait, retryFirst, retryMax time.Duration
	// buildCall bounds one call to a runner to build an image.
	buildCall time.Duration
	// sweepEvery is the time between two sweeps, and failedGrace how long a
	// failed row stays before a sweep removes it.
	sweepEvery, failedGrace time.Duration
}

// limits are the caps a publish is held to: the package's constants, smaller
// in tests.
type limits struct {
	maxTarBytes int64
	policy      safetar.Policy
	maxVolumes  int
	// maxImageBytes caps an image; maxRunnerBytes what a volume takes on a
	// runner, as volume.RunnerBytes counts it.
	maxImageBytes  int64
	maxRunnerBytes int64
}

var defaultTiming = timing{
	lease:       2 * time.Minute,
	renewEvery:  30 * time.Second,
	giveUp:      90 * time.Second,
	cleanup:     30 * time.Second,
	builderWait: 5 * time.Minute,
	retryFirst:  time.Second,
	retryMax:    10 * time.Second,
	buildCall:   30 * time.Minute,
	sweepEvery:  5 * time.Minute,
	failedGrace: 10 * time.Minute,
}

// Service publishes, lists and deletes shared volumes. A row is written before
// any of its objects and removed after them, so every object has a row.
type Service struct {
	records store.SharedVolumes
	objects volumestore.Store
	metrics *metrics.SharedVolumeRecorder
	// ctx ends at shutdown, which interrupts every publish.
	ctx    context.Context
	slots  chan struct{}
	now    func() time.Time
	newID  func() string
	timing timing
	limits limits
	// builds is nil on a service whose runners attach tars.
	builds *Builds
}

// NewService keeps shared volume rows in records and their objects in objects.
// Publishes in progress are interrupted when ctx ends.
func NewService(ctx context.Context, records store.SharedVolumes, objects volumestore.Store, rec *metrics.SharedVolumeRecorder) *Service {
	return &Service{
		records: records,
		objects: objects,
		metrics: rec,
		ctx:     ctx,
		slots:   make(chan struct{}, MaxPublishesPerPod),
		now:     time.Now,
		newID:   func() string { return uuid.New().String() },
		timing:  defaultTiming,
		limits: limits{
			maxTarBytes:    MaxTarBytes,
			policy:         tarPolicy(),
			maxVolumes:     MaxSharedVolumes,
			maxImageBytes:  MaxImageBytes,
			maxRunnerBytes: volume.MaxSandboxVolumeBytes,
		},
	}
}

// Get returns the ready shared volume with the sha256, or ErrNotFound.
func (s *Service) Get(ctx context.Context, sha256 string) (*store.SharedVolume, error) {
	row, err := s.records.GetSharedVolume(ctx, sha256)
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
	return s.records.ListSharedVolumes(ctx, name)
}

// Delete hides the ready shared volume with the sha256 and frees its name at
// once, then removes its objects and row. If that removal fails, the row stays
// deleted for a later removal. It returns ErrNotFound, or ErrPublishInProgress
// while the sha256 is being published.
func (s *Service) Delete(ctx context.Context, sha256, role string) error {
	row, err := s.records.DeleteSharedVolume(ctx, sha256, s.now().Unix())
	if err != nil {
		return err
	}
	switch {
	case row == nil:
		return ErrNotFound
	case row.State == store.SharedVolumePublishing:
		return ErrPublishInProgress
	}
	slog.InfoContext(ctx, "shared volume deleted", "version_id", row.ID, "sha256", row.SHA256, "name", row.Name, "role", role)
	removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.timing.cleanup)
	defer cancel()
	if _, err := s.remove(removeCtx, row.ID); err != nil {
		slog.WarnContext(ctx, "shared volume removal failed; the row stays deleted", "version_id", row.ID, "sha256", row.SHA256, "error", err)
	}
	return nil
}

// remove deletes the objects of the shared volume id, then its row unless it
// is ready, and reports whether it removed the row: false when another
// removal got there first. It is the one place that removes a volume: a
// delete, a failed publish's cleanup and the sweep all call it.
func (s *Service) remove(ctx context.Context, id string) (bool, error) {
	for _, kind := range []volume.Kind{volume.KindTar, volume.KindExt4} {
		if err := s.objects.Delete(ctx, objectKey(id, kind)); err != nil {
			return false, err
		}
	}
	return s.records.RemoveSharedVolume(ctx, id)
}

func objectKey(id string, kind volume.Kind) volumestore.Key {
	return volumestore.Key{Scope: volume.SharedScope, ID: id, Kind: kind}
}
