package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/n8n-io/sandbox-service/internal/api/archivestore"
	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/runnerctl"
	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/archive"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// Sandbox states of archive and restore.
const (
	statusArchived  = "archived"
	statusRestoring = "restoring"
)

const (
	reasonRestoring   = "restoring"
	reasonSandboxBusy = "sandbox_busy"
	reasonEphemeral   = "ephemeral"
)

// Budgets: an archive call moves a whole disk, a restore does that and boots.
var (
	archiveBudget = 30 * time.Minute
	restoreBudget = 35 * time.Minute
)

// restoreRetryAfter is the Retry-After of a 503 restoring.
const restoreRetryAfter = "5"

// archiver archives and restores sandboxes. Nil when no archive store is
// configured.
type archiver struct {
	s     store.SandboxStore
	reg   registry.RunnerRegistry
	cfg   *config.APIConfig
	store archivestore.Store
	tls   *runnerctl.TLS

	// drains are the runners this API pod is archiving for a drain.
	mu     sync.Mutex
	drains map[string]bool
}

// newArchiver builds the archiver and the store handler, or nils when
// archiving is off.
func newArchiver(s store.SandboxStore, reg registry.RunnerRegistry, cfg *config.APIConfig) (*archiver, *archivestore.Dir, error) {
	if !cfg.ArchiveEnabled() {
		return nil, nil, nil
	}
	a := &archiver{s: s, reg: reg, cfg: cfg, tls: runnerControlTLS(cfg), drains: map[string]bool{}}
	if cfg.ArchiveS3Bucket != "" {
		s3, err := archivestore.NewS3(cfg.ArchiveS3Endpoint, cfg.ArchiveS3Bucket, cfg.ArchiveS3Region, cfg.ArchiveS3AccessKey, cfg.ArchiveS3SecretKey)
		if err != nil {
			return nil, nil, fmt.Errorf("archive store: %w", err)
		}
		a.store = s3
		return a, nil, nil
	}
	// The URL-signing key is derived, so the master key wraps archive keys only.
	mac := hmac.New(sha256.New, cfg.ArchiveKey)
	mac.Write([]byte("archive-url-signing"))
	dir, err := archivestore.NewDir(cfg.ArchiveDir, cfg.ArchiveURL, mac.Sum(nil))
	if err != nil {
		return nil, nil, fmt.Errorf("archive store: %w", err)
	}
	a.store = dir
	return a, dir, nil
}

// archive moves a stopped sandbox's disk to the archive store. The caller
// holds the sandbox lock. The archive is recorded before the runner's copy is
// deleted, so a failure at any step leaves the sandbox stopped on its runner.
func (a *archiver) archive(ctx context.Context, rec *store.SandboxRecord) error {
	if rec.Status != "stopped" || rec.Ephemeral {
		return fmt.Errorf("sandbox %s is %s, not an archivable stopped sandbox", rec.ID, rec.Status)
	}
	key, err := archive.NewKey()
	if err != nil {
		return err
	}
	object := rec.TenantID + "/" + rec.ID + "/" + strings.ReplaceAll(uuid.NewString(), "-", "")
	upload, err := a.store.UploadURL(object)
	if err != nil {
		return err
	}
	var version string
	if run, ok := a.reg.Get(rec.RunnerID); ok {
		version = run.TemplateVersion(recordTemplate(rec))
	}
	controlAddr := resolveControlAddr(rec, a.reg)
	callCtx, cancel := context.WithTimeout(ctx, archiveBudget)
	resp, err := runnerctl.ArchiveSandbox(callCtx, controlAddr, a.cfg.RunnerAPIKey, a.tls, rec.ID, upload, key)
	cancel()
	if err != nil {
		_ = a.store.Delete(context.WithoutCancel(ctx), object)
		return &runnerCallError{err}
	}
	// The store's copy must be the runner's whole upload.
	if size, err := a.store.Size(ctx, object); err != nil || size != resp.GetSize() {
		_ = a.store.Delete(context.WithoutCancel(ctx), object)
		return fmt.Errorf("archive of %s stored %d bytes, runner sent %d: %v", rec.ID, size, resp.GetSize(), err)
	}
	wrapped, err := archive.WrapKey(a.cfg.ArchiveKey, key)
	if err != nil {
		return err
	}
	if err := a.s.SetArchived(rec.ID, object, base64.StdEncoding.EncodeToString(wrapped), version, resp.GetSize(), time.Now().Unix()); err != nil {
		_ = a.store.Delete(context.WithoutCancel(ctx), object)
		return err
	}
	slog.InfoContext(ctx, "sandbox archived", "sandbox_id", rec.ID, "runner_id", rec.RunnerID, "bytes", resp.GetSize())
	a.dropRunnerCopy(ctx, rec)
	return nil
}

// dropRunnerCopy deletes an archived sandbox from its runner; a failure is
// retried by the sweeper (ListArchivedOnRunner).
func (a *archiver) dropRunnerCopy(ctx context.Context, rec *store.SandboxRecord) {
	if rec.RunnerID == "" && rec.RunnerControlGRPCAddr == "" {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, runnerLifecycleBudget)
	err := runnerctl.DeleteSandbox(callCtx, resolveControlAddr(rec, a.reg), a.cfg.RunnerAPIKey, a.tls, rec.ID)
	cancel()
	if err != nil && !errors.Is(err, runnerruntime.ErrSandboxNotFound) {
		slog.WarnContext(ctx, "archived sandbox not yet removed from its runner", "sandbox_id", rec.ID, "runner_id", rec.RunnerID, "err", err)
		return
	}
	if err := a.s.ClearRunner(rec.ID); err != nil {
		slog.WarnContext(ctx, "clear runner of archived sandbox", "sandbox_id", rec.ID, "err", err)
	}
}

// restoreRetryDelay spaces restore attempts: each one moves the whole disk.
var restoreRetryDelay = 30 * time.Second

// restorable reports whether rec is archived with no restore begun in the
// last restoreRetryDelay, or restoring under a restore that has run past its
// budget (its API pod went away).
func restorable(rec *store.SandboxRecord, now time.Time) bool {
	return (rec.Status == statusArchived && now.Unix()-rec.RestoreStartedAt >= int64(restoreRetryDelay/time.Second)) ||
		(rec.Status == statusRestoring && now.Unix()-rec.RestoreStartedAt > int64((restoreBudget+time.Minute)/time.Second))
}

// startRestore begins restoring rec in the background unless a restore runs.
// The claim is taken under the sandbox lock, so it never overlaps the sweeper
// removing the sandbox's old runner copy.
func (a *archiver) startRestore(rec *store.SandboxRecord) {
	if !restorable(rec, time.Now()) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), runnerLifecycleBudget+time.Minute)
		defer cancel()
		var started int64
		_ = withLockedSandbox(ctx, a.s, rec.ID, func(fresh *store.SandboxRecord) {
			now := time.Now()
			if !restorable(fresh, now) {
				return
			}
			if won, err := a.s.BeginRestore(fresh.ID, fresh.Status, fresh.RestoreStartedAt, now.Unix()); err == nil && won {
				started = now.Unix()
			}
		})
		if started != 0 {
			a.restore(rec.ID, started)
		}
	}()
}

// restore brings an archived sandbox back on a runner serving its template at
// the archived version, under the restore claim begun at started. An archive
// that is gone or does not open deletes the sandbox; anything else leaves it
// archived for the next attempt.
func (a *archiver) restore(id string, started int64) {
	ctx, cancel := context.WithTimeout(context.Background(), restoreBudget)
	defer cancel()
	rec, err := a.s.Get(id)
	if err != nil || rec == nil || rec.Status != statusRestoring || rec.RestoreStartedAt != started {
		return
	}
	backToArchived := func(why string, err error) {
		slog.Warn("sandbox restore failed, still archived", "sandbox_id", id, "why", why, "err", err)
		_ = a.s.AbortRestore(id, started)
	}
	lost := func() {
		slog.Error("sandbox archive lost, deleting the sandbox", "sandbox_id", id)
		_ = a.store.Delete(ctx, rec.ArchiveObject)
		_ = a.s.Delete(id)
	}
	// Only the store says an archive is gone: a runner's failed download may
	// be the network or a store that is not ready.
	switch size, err := a.store.Size(ctx, rec.ArchiveObject); {
	case errors.Is(err, archivestore.ErrNotFound), err == nil && rec.ArchiveSize > 0 && size != rec.ArchiveSize:
		lost()
		return
	case err != nil:
		backToArchived("archive store", err)
		return
	}
	wrapped, err := base64.StdEncoding.DecodeString(rec.ArchiveKey)
	if err != nil {
		backToArchived("archive key", err)
		return
	}
	key, err := archive.UnwrapKey(a.cfg.ArchiveKey, wrapped)
	if err != nil {
		backToArchived("archive key", err)
		return
	}
	optsJSON, err := json.Marshal(restoreOptions(rec))
	if err != nil {
		backToArchived("options", err)
		return
	}
	download, err := a.store.DownloadURL(rec.ArchiveObject)
	if err != nil {
		backToArchived("download url", err)
		return
	}
	// A runner copy left behind would collide with the restored sandbox.
	if rec.RunnerID != "" {
		a.dropRunnerCopy(ctx, rec)
	}

	var excluded []string
	for attempt := 0; attempt < createPlacementAttempts; attempt++ {
		run, err := a.reg.PickForTemplate(recordTemplate(rec), excluded...)
		if err != nil {
			backToArchived("no runner", err)
			return
		}
		if rec.ArchiveVersion != "" && run.TemplateVersion(recordTemplate(rec)) != rec.ArchiveVersion {
			excluded = append(excluded, run.ID)
			continue
		}
		resp, err := runnerctl.RestoreSandbox(ctx, run.ControlGRPCAddr, a.cfg.RunnerAPIKey, a.tls, id, string(optsJSON), download, key)
		switch {
		case runnerctl.IsCapacityRefusal(err):
			excluded = append(excluded, run.ID)
			continue
		case status.Code(err) == codes.DataLoss:
			lost()
			return
		case err != nil:
			backToArchived("runner restore", err)
			return
		}
		applied, err := runnerruntime.ParseCreateOptions(resp.GetAppliedCreateOptionsJson())
		requested := restoreOptions(rec)
		if err != nil || !applied.Equal(&requested) {
			delCtx, cancelDel := context.WithTimeout(context.WithoutCancel(ctx), runnerCreateBudget)
			_ = runnerctl.DeleteSandbox(delCtx, run.ControlGRPCAddr, a.cfg.RunnerAPIKey, a.tls, id)
			cancelDel()
			backToArchived("runner did not apply the options", err)
			return
		}
		ok, err := a.s.SetRestored(id, started, run.ID, run.HTTPBaseURL, run.ControlGRPCAddr, resp.GetContainerIp(), time.Now().Unix())
		if err != nil || !ok {
			// Another pod took the restore over: this copy is not the sandbox's.
			delCtx, cancelDel := context.WithTimeout(context.WithoutCancel(ctx), runnerCreateBudget)
			_ = runnerctl.DeleteSandbox(delCtx, run.ControlGRPCAddr, a.cfg.RunnerAPIKey, a.tls, id)
			cancelDel()
			backToArchived("store", err)
			return
		}
		_ = a.store.Delete(ctx, rec.ArchiveObject)
		slog.Info("sandbox restored", "sandbox_id", id, "runner_id", run.ID)
		return
	}
	backToArchived("no runner with the template version had room", nil)
}

// restoreOptions are the create options a restore sends: the sandbox's own.
func restoreOptions(rec *store.SandboxRecord) runnerruntime.CreateOptions {
	opts := runnerruntime.CreateOptions{Egress: runnerruntime.Egress(rec.Egress)}
	if t := recordTemplate(rec); t != runnerruntime.DefaultTemplate {
		opts.Template = t
	}
	if opts.Egress == runnerruntime.EgressProxy && rec.EgressProxy != "" {
		var spec runnerruntime.EgressProxySpec
		if json.Unmarshal([]byte(rec.EgressProxy), &spec) == nil && spec.Validate() == nil {
			opts.EgressProxy = &spec
		}
		opts.Tenant = rec.TenantID
	}
	return opts
}

// deleteArchived removes an archived sandbox: its object, any runner copy,
// and its row.
func (a *archiver) deleteArchived(ctx context.Context, rec *store.SandboxRecord) error {
	if rec.RunnerID != "" {
		callCtx, cancel := context.WithTimeout(ctx, runnerLifecycleBudget)
		err := runnerctl.DeleteSandbox(callCtx, resolveControlAddr(rec, a.reg), a.cfg.RunnerAPIKey, a.tls, rec.ID)
		cancel()
		if err != nil && !errors.Is(err, runnerruntime.ErrSandboxNotFound) {
			return &runnerCallError{err}
		}
	}
	if err := a.store.Delete(ctx, rec.ArchiveObject); err != nil {
		return err
	}
	return a.s.Delete(rec.ID)
}

// drainArchiveInterval is how often a drain looks for sandboxes left to archive.
var drainArchiveInterval = 15 * time.Second

func (a *archiver) drainArchiving(runnerID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.drains[runnerID]
}

func (a *archiver) stopDrainArchive(runnerID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.drains, runnerID)
}

// startDrainArchive archives every non-ephemeral sandbox on runnerID as it
// goes idle, until none is left or the drain stops. It runs on this API pod.
func (a *archiver) startDrainArchive(runnerID string) {
	a.mu.Lock()
	if a.drains[runnerID] {
		a.mu.Unlock()
		return
	}
	a.drains[runnerID] = true
	a.mu.Unlock()
	go func() {
		for a.drainArchiving(runnerID) {
			if left := a.drainArchiveOnce(runnerID); left == 0 {
				return
			}
			time.Sleep(drainArchiveInterval)
		}
	}()
}

// drainArchiveOnce archives what it can and reports how many are left.
func (a *archiver) drainArchiveOnce(runnerID string) int {
	recs, err := a.s.ListByRunner(runnerID)
	if err != nil {
		return 1
	}
	left := 0
	for _, r := range recs {
		if r.Ephemeral || r.Status == statusArchived || r.Status == statusRestoring {
			continue
		}
		left++
		ctx, cancel := context.WithTimeout(context.Background(), archiveBudget+runnerLifecycleBudget)
		_ = withLockedSandbox(ctx, a.s, r.ID, func(rec *store.SandboxRecord) {
			if rec.RunnerID != runnerID || rec.Ephemeral {
				return
			}
			if rec.Status == "running" {
				callCtx, cancelStop := context.WithTimeout(ctx, runnerLifecycleBudget)
				err := runnerctl.StopSandbox(callCtx, resolveControlAddr(rec, a.reg), a.cfg.RunnerAPIKey, a.tls, rec.ID)
				cancelStop()
				if err != nil || a.s.UpdateStatus(rec.ID, "stopped") != nil {
					return // a run in progress: next pass
				}
				rec.Status = "stopped"
			}
			if err := a.archive(ctx, rec); err == nil {
				left--
			}
		})
		cancel()
	}
	return left
}
