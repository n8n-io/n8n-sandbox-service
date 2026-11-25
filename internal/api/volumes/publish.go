// This file publishes a shared volume: it reserves the row, streams the tar to
// the volume store and makes the row ready.

package volumes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/api/volumestore"
	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/volume"
)

// PublishRequest is one PUT of a shared volume.
type PublishRequest struct {
	// SHA256 is the tar's digest, lowercase hex. The caller validates it.
	SHA256 string
	// Name is "" for none.
	Name string
	// SizeBytes is the tar's length, from Content-Length.
	SizeBytes int64
	// Body is the tar. Publish reads it only once it has reserved the row.
	Body io.Reader
	// Role is the role of the caller's key.
	Role Role
	// UploadDone, if set, is called once Body has been read to its end.
	UploadDone func()
	// Interrupt, if set, ends a read of Body that waits for the client. It is
	// called when the upload must stop early: at shutdown, when the lease is
	// lost, when the request ends, or when the tar is refused before Body ends.
	// Publish does not call it when it returns before it reads Body.
	Interrupt func()
}

// PublishResult is the result of a publish.
type PublishResult struct {
	Volume *store.SharedVolume
	// Created is false for a digest that was published before.
	Created bool
}

// Publish stores a shared volume, or returns the one with this digest that
// is ready. The upload ends with the request, and after it the publish goes
// on without the client. A failed publish leaves nothing behind, unless its
// cleanup fails: its row then stays failed for a later purge.
func (s *Service) Publish(ctx context.Context, request PublishRequest) (published PublishResult, err error) {
	start := s.now()
	defer func() {
		result := publishResultLabel(published, err)
		s.metrics.ObservePublish(result)
		if published.Created {
			s.metrics.ObservePublishDuration(s.now().Sub(start))
		}
	}()

	if request.Name != "" && !ValidName(request.Name) {
		return PublishResult{}, ErrInvalidName
	}
	if request.SizeBytes < 1 {
		return PublishResult{}, errTarEmpty
	}
	if request.SizeBytes > s.limits.maxTarBytes {
		return PublishResult{}, errUploadTooLarge
	}

	// A repeated publish gets its answer here, so it takes no publish slot and
	// its body is not read. The insert below still decides a race.
	current, err := s.records.GetLiveSharedVolume(ctx, request.SHA256)
	if err != nil {
		return PublishResult{}, err
	}
	if current != nil {
		return s.republish(ctx, current, request.Name, request.Role)
	}

	select {
	case s.runningPublishes <- struct{}{}:
		defer func() { <-s.runningPublishes }()
	default:
		return PublishResult{}, ErrPublishLimit
	}

	row := &store.SharedVolume{ID: s.generateID(), SHA256: request.SHA256, Name: request.Name, PublishedBy: string(request.Role)}
	live, err := s.records.StartSharedVolumePublish(ctx, row, s.timing.lease, s.limits.maxLiveVolumes)
	switch {
	case errors.Is(err, store.ErrSharedVolumeDigestLive) && live != nil:
		return s.republish(ctx, live, request.Name, request.Role)
	case errors.Is(err, store.ErrSharedVolumeDigestLive):
		return PublishResult{}, ErrPublishInProgress
	case errors.Is(err, store.ErrSharedVolumeNameTaken):
		return PublishResult{}, ErrNameTaken
	case errors.Is(err, store.ErrSharedVolumeLimit):
		return PublishResult{}, ErrVolumeLimit
	case err != nil:
		return PublishResult{}, err
	}

	// This publish owns the row now. It continues without the client until the
	// API shuts down or the lease is lost.
	publishCtx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	defer cancel(nil)
	stopShutdownWatch := context.AfterFunc(s.shutdownCtx, func() { cancel(errShuttingDown) })
	defer stopShutdownWatch()
	stopLease := s.keepLease(publishCtx, row.ID, cancel)

	err = s.uploadAndFinish(ctx, publishCtx, row, request)
	stopLease()
	if err != nil {
		// A shutdown or a lost lease also makes the upload fail. Report that
		// cause, not the upload error.
		if cause := context.Cause(publishCtx); cause != nil {
			err = fmt.Errorf("%w: %w", ErrInterrupted, cause)
		}
		s.discard(ctx, row, err)
		return PublishResult{}, err
	}
	slog.InfoContext(ctx, "shared volume published",
		"version_id", row.ID, "sha256", row.SHA256, "name", row.Name, "role", request.Role,
		"tar_bytes", row.TarBytes, "tar_entries", row.TarEntries, "tar_content_bytes", row.TarContentBytes,
		"tar_decompressed_bytes", row.TarDecompressedBytes, "duration_ms", s.now().Sub(start).Milliseconds())
	return PublishResult{Volume: row, Created: true}, nil
}

// uploadAndFinish uploads the tar of row and makes the row ready. The upload
// also stops when the request ends. The rest stops only with publishCtx.
func (s *Service) uploadAndFinish(requestCtx, publishCtx context.Context, row *store.SharedVolume, request PublishRequest) error {
	uploadCtx, cancelUpload := context.WithCancel(publishCtx)
	stopRequestWatch := context.AfterFunc(requestCtx, cancelUpload)
	stopInterrupt := func() bool { return true }
	if request.Interrupt != nil {
		stopInterrupt = context.AfterFunc(uploadCtx, request.Interrupt)
	}
	counts, object, err := s.upload(uploadCtx, row.ID, request)
	stopInterrupt()
	stopRequestWatch()
	cancelUpload()
	if err != nil {
		// The store may see the cancelled request before the body fails.
		if requestCtx.Err() != nil && context.Cause(publishCtx) == nil && !isCallerError(err) {
			err = fmt.Errorf("%w: the client went away: %w", ErrUpload, err)
		}
		return err
	}
	if request.UploadDone != nil {
		request.UploadDone()
	}
	if err := counts.Validate(); err != nil {
		return fmt.Errorf("%w: %w", errTarRules, err)
	}

	row.TarBytes = object.Size
	row.TarEntries, row.TarContentBytes, row.TarDecompressedBytes = counts.Entries, counts.ContentBytes, counts.DecompressedBytes
	finished, err := s.records.FinishSharedVolumePublish(publishCtx, row)
	if err != nil {
		return err
	}
	if !finished {
		return fmt.Errorf("%w: %w", ErrInterrupted, errLeaseLost)
	}
	return nil
}

// upload streams the body into the tar object of id, validating it on the
// way. A tar the validator refuses never reaches the end of the store's Put,
// so nothing of it is committed.
func (s *Service) upload(ctx context.Context, id string, request PublishRequest) (volume.TarCounts, volumestore.Object, error) {
	gate := newTarGate(request.Body, s.limits.policy, request.Interrupt)
	defer gate.close()
	object, err := s.objects.Put(ctx, objectKey(id, volume.KindTar), gate, volumestore.PutOptions{
		MaxBytes:       request.SizeBytes,
		ExpectedSHA256: request.SHA256,
	})
	if err != nil {
		if gateError := gate.err(); gateError != nil {
			return volume.TarCounts{}, volumestore.Object{}, gateError
		}
		if errors.Is(err, volumestore.ErrDigestMismatch) {
			return volume.TarCounts{}, volumestore.Object{}, ErrDigestMismatch
		}
		return volume.TarCounts{}, volumestore.Object{}, fmt.Errorf("store the tar: %w", err)
	}
	return gate.counts(), object, nil
}

// republish answers a publish of a digest that has a row already. A ready
// row gets the name if it has none.
func (s *Service) republish(ctx context.Context, row *store.SharedVolume, name string, role Role) (PublishResult, error) {
	if row.State != store.SharedVolumeReady {
		return PublishResult{}, ErrPublishInProgress
	}
	switch {
	case name == "" || name == row.Name:
		return PublishResult{Volume: row}, nil
	case row.Name != "":
		return PublishResult{}, ErrNameMismatch
	}
	storedName, err := s.records.NameReadySharedVolume(ctx, row.ID, name)
	switch {
	case errors.Is(err, store.ErrSharedVolumeNameTaken):
		return PublishResult{}, ErrNameTaken
	case err != nil:
		return PublishResult{}, err
	case storedName == "":
		// Deleted since it was read.
		return PublishResult{}, ErrPublishInProgress
	case storedName != name:
		// Named by another request since it was read.
		return PublishResult{}, ErrNameMismatch
	}
	slog.InfoContext(ctx, "shared volume named", "version_id", row.ID, "sha256", row.SHA256, "name", name, "role", role)
	row.Name = name
	return PublishResult{Volume: row}, nil
}

// keepLease renews the lease of row id until stop is called. It calls cancel
// when another publish took the row over, or when no renewal worked for
// timing.giveUpAfter. giveUpAfter is shorter than the lease, so the publish
// stops before another one can take the row over. A renewal that hangs stops
// at giveUpAfter, and stop ends a renewal in progress.
func (s *Service) keepLease(ctx context.Context, id string, cancel context.CancelCauseFunc) (stop func()) {
	leaseCtx, stopRenewing := context.WithCancel(ctx)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(s.timing.renewEvery)
		defer ticker.Stop()
		lastRenewedAt := s.now()
		for {
			select {
			case <-leaseCtx.Done():
				return
			case <-ticker.C:
			}
			now := s.now()
			renewCtx, cancelRenewal := context.WithTimeout(leaseCtx, s.timing.giveUpAfter-now.Sub(lastRenewedAt))
			stillPublishing, err := s.records.RenewSharedVolumePublish(renewCtx, id, s.timing.lease)
			cancelRenewal()
			switch {
			case leaseCtx.Err() != nil:
				return
			case err == nil && stillPublishing:
				// Count from the start of the call: the database set the lease
				// at some time during it.
				lastRenewedAt = now
			case err == nil:
				cancel(errLeaseLost)
				return
			case s.now().Sub(lastRenewedAt) >= s.timing.giveUpAfter:
				slog.WarnContext(ctx, "shared volume publish ended: its lease could not be renewed", "version_id", id, "error", err)
				cancel(errLeaseLost)
				return
			}
		}
	}()
	return func() {
		stopRenewing()
		<-finished
	}
}

// discard logs a failed publish and removes what it stored. It marks the row
// failed first. That mark does not change a row that became ready anyway, for
// example after a lost finish reply, so the objects of that row stay. If the
// purge fails, the row stays failed for a later purge.
func (s *Service) discard(ctx context.Context, row *store.SharedVolume, cause error) {
	attributes := []any{"version_id", row.ID, "sha256", row.SHA256, "name", row.Name, "result", publishResultLabel(PublishResult{}, cause)}
	if !isCallerError(cause) {
		attributes = append(attributes, "error", cause)
	}
	slog.WarnContext(ctx, "shared volume publish failed", attributes...)

	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.timing.cleanupTimeout)
	defer cancel()
	failed, err := s.records.FailSharedVolumePublish(cleanupCtx, row.ID)
	if err != nil || !failed {
		if err != nil {
			slog.WarnContext(ctx, "shared volume publish cleanup: mark failed", "version_id", row.ID, "error", err)
		}
		return
	}
	if err := s.purge(cleanupCtx, row.ID); err != nil {
		slog.WarnContext(ctx, "shared volume publish cleanup failed; the row stays failed", "version_id", row.ID, "error", err)
	}
}

// isCallerError reports whether err is about the upload, not the service, so
// it is not logged: its text says nothing an operator needs.
func isCallerError(err error) bool {
	for _, target := range []error{ErrInvalidTar, ErrTooLarge, ErrDigestMismatch, ErrUpload} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// publishResultLabel is the metric label of a publish's outcome.
func publishResultLabel(published PublishResult, err error) string {
	switch {
	case err == nil && published.Created:
		return metrics.SharedVolumePublishCreated
	case err == nil:
		return metrics.SharedVolumePublishExists
	case errors.Is(err, ErrInvalidTar):
		return metrics.SharedVolumePublishInvalidTar
	case errors.Is(err, ErrTooLarge):
		return metrics.SharedVolumePublishTooLarge
	case errors.Is(err, ErrDigestMismatch):
		return metrics.SharedVolumePublishDigestMismatch
	case errors.Is(err, ErrInvalidName):
		return metrics.SharedVolumePublishInvalidRequest
	case errors.Is(err, ErrNameTaken), errors.Is(err, ErrNameMismatch), errors.Is(err, ErrPublishInProgress):
		return metrics.SharedVolumePublishConflict
	case errors.Is(err, ErrVolumeLimit), errors.Is(err, ErrPublishLimit):
		return metrics.SharedVolumePublishLimit
	case errors.Is(err, ErrUpload), errors.Is(err, ErrInterrupted):
		return metrics.SharedVolumePublishInterrupted
	}
	return metrics.SharedVolumePublishError
}
