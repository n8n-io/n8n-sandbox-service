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
	// SHA256 is the tar's digest, lowercase hex.
	SHA256 string
	// Name is "" for none.
	Name string
	// Size is the tar's length, from Content-Length.
	Size int64
	// Body is the tar. Publish reads it only once it has reserved the row.
	Body io.Reader
	// Role is the role of the caller's key.
	Role Role
	// UploadDone, if set, is called once Body has been read to its end.
	UploadDone func()
	// Interrupt, if set, ends a read of Body that waits for the client. It is
	// called when the upload must stop early: at shutdown, when the lease is
	// lost, or when the request ends.
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
func (s *Service) Publish(ctx context.Context, req PublishRequest) (published PublishResult, err error) {
	start := s.now()
	defer func() {
		result := publishResult(published, err)
		s.metrics.ObservePublish(result)
		if published.Created {
			s.metrics.ObservePublishDuration(s.now().Sub(start))
		}
	}()

	if req.Name != "" && !ValidName(req.Name) {
		return PublishResult{}, ErrInvalidName
	}
	if req.Size < 1 {
		return PublishResult{}, errTarEmpty
	}
	if req.Size > s.limits.maxTarBytes {
		return PublishResult{}, errUploadTooLarge
	}

	// Answers a repeated publish without a running publish or the body.
	current, err := s.records.GetLiveSharedVolume(ctx, req.SHA256)
	if err != nil {
		return PublishResult{}, err
	}
	if current != nil {
		return s.republish(ctx, current, req.Name, req.Role)
	}

	select {
	case s.runningPublishes <- struct{}{}:
		defer func() { <-s.runningPublishes }()
	default:
		return PublishResult{}, ErrPublishLimit
	}

	row := &store.SharedVolume{ID: s.newID(), SHA256: req.SHA256, Name: req.Name, PublishedBy: string(req.Role)}
	live, err := s.records.StartSharedVolumePublish(ctx, row, s.timing.lease, s.limits.maxLiveVolumes)
	switch {
	case errors.Is(err, store.ErrSharedVolumeDigestLive) && live != nil:
		return s.republish(ctx, live, req.Name, req.Role)
	case errors.Is(err, store.ErrSharedVolumeDigestLive):
		return PublishResult{}, ErrPublishInProgress
	case errors.Is(err, store.ErrSharedVolumeNameTaken):
		return PublishResult{}, ErrNameTaken
	case errors.Is(err, store.ErrSharedVolumeLimit):
		return PublishResult{}, ErrVolumeLimit
	case err != nil:
		return PublishResult{}, err
	}

	// The row is this publish's from here on: it goes on without the client,
	// until the API shuts down or the lease is lost.
	publishCtx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	defer cancel(nil)
	stopShutdownWatch := context.AfterFunc(s.ctx, func() { cancel(errShuttingDown) })
	defer stopShutdownWatch()
	stopLease := s.keepLease(publishCtx, row.ID, cancel)

	err = s.publish(ctx, publishCtx, row, req)
	stopLease()
	if err != nil {
		// An interrupted upload fails as an upload; the cause is what ended it.
		if cause := context.Cause(publishCtx); cause != nil {
			err = fmt.Errorf("%w: %w", ErrInterrupted, cause)
		}
		s.discard(ctx, row, err)
		return PublishResult{}, err
	}
	slog.InfoContext(ctx, "shared volume published",
		"version_id", row.ID, "sha256", row.SHA256, "name", row.Name, "role", req.Role,
		"tar_bytes", row.TarBytes, "tar_entries", row.TarEntries, "tar_content_bytes", row.TarContentBytes,
		"tar_decompressed_bytes", row.TarDecompressedBytes, "duration_ms", s.now().Sub(start).Milliseconds())
	return PublishResult{Volume: row, Created: true}, nil
}

// publish uploads the tar of row and makes the row ready. The upload follows
// requestCtx too, the rest only publishCtx.
func (s *Service) publish(requestCtx, publishCtx context.Context, row *store.SharedVolume, req PublishRequest) error {
	uploadCtx, cancelUpload := context.WithCancel(publishCtx)
	stopRequestWatch := context.AfterFunc(requestCtx, cancelUpload)
	stopInterrupt := func() bool { return true }
	if req.Interrupt != nil {
		stopInterrupt = context.AfterFunc(uploadCtx, req.Interrupt)
	}
	counts, object, err := s.upload(uploadCtx, row.ID, req)
	stopInterrupt()
	stopRequestWatch()
	cancelUpload()
	if err != nil {
		// The store may see the cancelled request before the body fails.
		if requestCtx.Err() != nil && context.Cause(publishCtx) == nil && !errors.Is(err, ErrUpload) && !isCallerError(err) {
			err = fmt.Errorf("%w: the client went away: %w", ErrUpload, err)
		}
		return err
	}
	if req.UploadDone != nil {
		req.UploadDone()
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
func (s *Service) upload(ctx context.Context, id string, req PublishRequest) (volume.TarCounts, volumestore.Object, error) {
	gate := newTarGate(req.Body, s.limits.policy)
	defer gate.close()
	object, err := s.objects.Put(ctx, objectKey(id, volume.KindTar), gate, volumestore.PutOptions{
		MaxBytes:       req.Size,
		ExpectedSHA256: req.SHA256,
	})
	if err != nil {
		if gateErr := gate.err(); gateErr != nil {
			return volume.TarCounts{}, volumestore.Object{}, gateErr
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
	named, err := s.records.NameReadySharedVolume(ctx, row.ID, name)
	if errors.Is(err, store.ErrSharedVolumeNameTaken) {
		return PublishResult{}, ErrNameTaken
	}
	if err != nil {
		return PublishResult{}, err
	}
	if !named {
		// Named or deleted since it was read.
		current, err := s.records.GetLiveSharedVolume(ctx, row.SHA256)
		if err != nil {
			return PublishResult{}, err
		}
		if current == nil || current.State != store.SharedVolumeReady {
			return PublishResult{}, ErrPublishInProgress
		}
		if current.Name != name {
			return PublishResult{}, ErrNameMismatch
		}
		return PublishResult{Volume: current}, nil
	}
	slog.InfoContext(ctx, "shared volume named", "version_id", row.ID, "sha256", row.SHA256, "name", name, "role", role)
	row.Name = name
	return PublishResult{Volume: row}, nil
}

// keepLease renews the lease of row id until stop is called, and ends the
// publish through cancel when the row was taken over or no renewal worked for
// timing.giveUp, which is before the lease runs out. A renewal that hangs
// ends at that point too, and stop ends one in progress.
func (s *Service) keepLease(ctx context.Context, id string, cancel context.CancelCauseFunc) (stop func()) {
	leaseCtx, stopRenewing := context.WithCancel(ctx)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(s.timing.renewEvery)
		defer ticker.Stop()
		renewed := s.now()
		for {
			select {
			case <-leaseCtx.Done():
				return
			case <-ticker.C:
			}
			now := s.now()
			renewCtx, renewDone := context.WithTimeout(leaseCtx, s.timing.giveUp-now.Sub(renewed))
			stillPublishing, err := s.records.RenewSharedVolumePublish(renewCtx, id, s.timing.lease)
			renewDone()
			switch {
			case leaseCtx.Err() != nil:
				return
			case err == nil && stillPublishing:
				renewed = now
			case err == nil:
				cancel(errLeaseLost)
				return
			case s.now().Sub(renewed) >= s.timing.giveUp:
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

// discard removes what a failed publish of row stored. It first marks the row
// failed, which also tells a row that became ready without the publish
// knowing, whose objects must stay. A purge that fails leaves the row failed
// for a later one.
func (s *Service) discard(ctx context.Context, row *store.SharedVolume, cause error) {
	attrs := []any{"version_id", row.ID, "sha256", row.SHA256, "name", row.Name, "result", publishResult(PublishResult{}, cause)}
	if !isCallerError(cause) {
		attrs = append(attrs, "error", cause)
	}
	slog.WarnContext(ctx, "shared volume publish failed", attrs...)

	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.timing.cleanup)
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
	for _, target := range []error{ErrInvalidTar, ErrTooLarge, ErrDigestMismatch} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// publishResult is the metric label of a publish's outcome.
func publishResult(published PublishResult, err error) string {
	switch {
	case err == nil && published.Created:
		return metrics.SharedVolumeCreated
	case err == nil:
		return metrics.SharedVolumeExists
	case errors.Is(err, ErrInvalidTar):
		return metrics.SharedVolumeInvalidTar
	case errors.Is(err, ErrTooLarge):
		return metrics.SharedVolumeTooLarge
	case errors.Is(err, ErrDigestMismatch):
		return metrics.SharedVolumeDigestMismatch
	case errors.Is(err, ErrInvalidName):
		return metrics.SharedVolumeInvalidRequest
	case errors.Is(err, ErrNameTaken), errors.Is(err, ErrNameMismatch), errors.Is(err, ErrPublishInProgress):
		return metrics.SharedVolumeConflict
	case errors.Is(err, ErrVolumeLimit), errors.Is(err, ErrPublishLimit):
		return metrics.SharedVolumeLimit
	case errors.Is(err, ErrUpload), errors.Is(err, ErrInterrupted):
		return metrics.SharedVolumeInterrupted
	}
	return metrics.SharedVolumeError
}
