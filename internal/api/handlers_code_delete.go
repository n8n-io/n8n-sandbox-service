// This file serves the routes that delete code sandboxes, by id and by name.

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/runnerctl"
	"github.com/n8n-io/sandbox-service/internal/api/store"
	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/obs"
)

// handleDeleteCodeSandbox handles DELETE /code/sandboxes/{id}. It deletes the
// sandbox, draining or not, and stops all its tasks.
func handleDeleteCodeSandbox(s codeSandboxStore, reg registry.RunnerRegistry, cfg *config.APIConfig, mrec *metrics.APIRecorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		success := false
		defer func() { mrec.ObserveSandboxOp(metrics.OpDelete, success) }()

		id := r.PathValue("id")
		if !isValidUUID(id) {
			writeError(w, http.StatusBadRequest, "invalid sandbox id")
			return
		}
		obs.FieldsFrom(r.Context()).Add("sandbox_id", id)

		deleted, err := deleteCodeSandbox(r.Context(), s, reg, cfg, id, func(rec *store.SandboxRecord) bool {
			return canAccessSandbox(r, rec)
		})
		if err != nil {
			writeCodeDeleteError(w, err)
			return
		}
		if !deleted {
			// The same 404 for missing and inaccessible sandboxes, so a
			// tenant cannot find the sandboxes of other tenants.
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		success = true
	}
}

// handleDeleteCodeSandboxByName handles DELETE /code/sandboxes/names/{name}.
// It deletes all code sandboxes of the caller's tenant that have the name,
// draining ones too, and stops all their tasks. It tries each sandbox even
// when one fails, so a retry has less left to do.
func handleDeleteCodeSandboxByName(s codeSandboxStore, reg registry.RunnerRegistry, cfg *config.APIConfig, mrec *metrics.APIRecorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		success := false
		defer func() { mrec.ObserveSandboxOp(metrics.OpDelete, success) }()

		authID, ok := authFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "invalid API key")
			return
		}
		// Admin keys share the admin pseudo-tenant, which must never apply to
		// a role other than admin.
		tenantID := store.AdminTenantID
		switch authID.Role {
		case roleAdmin:
		case roleTenant:
			tenantID = authID.TenantID
		default:
			writeError(w, http.StatusForbidden, "sandbox access requires an admin or tenant API key")
			return
		}

		name := r.PathValue("name")
		if !isValidCodeName(name) {
			writeError(w, http.StatusBadRequest, "invalid name")
			return
		}

		recs, err := s.ListCodeSandboxesByName(tenantID, name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}

		var deletedIDs []string
		var firstErr error
		for _, found := range recs {
			if err := r.Context().Err(); err != nil {
				// The client has gone. The deletes that started are done. Do
				// not report success, because sandboxes can remain.
				if firstErr == nil {
					firstErr = err
				}
				break
			}
			deleted, err := deleteCodeSandbox(r.Context(), s, reg, cfg, found.ID, func(rec *store.SandboxRecord) bool {
				return rec.TenantID == tenantID && rec.Name == name
			})
			if err != nil {
				slog.ErrorContext(r.Context(), "delete code sandbox by name failed",
					"sandbox_id", found.ID, "runner_id", found.RunnerID, "error", err)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if deleted {
				deletedIDs = append(deletedIDs, found.ID)
			}
		}
		obs.FieldsFrom(r.Context()).Add("sandbox_ids", deletedIDs)
		if firstErr != nil {
			writeCodeDeleteError(w, firstErr)
			return
		}
		if len(deletedIDs) == 0 {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		success = true
	}
}

func writeCodeDeleteError(w http.ResponseWriter, err error) {
	var runnerErr *codeRunnerDeleteError
	if errors.As(err, &runnerErr) {
		writeError(w, http.StatusBadGateway, "failed to delete sandbox: "+runnerErr.err.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}

// codeRunnerDeleteError is a failed delete of a code sandbox on its runner.
type codeRunnerDeleteError struct{ err error }

func (e *codeRunnerDeleteError) Error() string { return "runner delete: " + e.err.Error() }

func (e *codeRunnerDeleteError) Unwrap() error { return e.err }

// deleteCodeSandbox locks the code sandbox id and reads its record again.
// When the record exists and shouldDelete returns true for it, it deletes the
// sandbox on its runner, which stops all its tasks, and then deletes the
// record. It reports whether it deleted the sandbox. ctx bounds only the wait
// for the lock: once the delete starts, it goes on without ctx's
// cancellation, so a caller that goes away does not leave the sandbox half
// deleted.
func deleteCodeSandbox(ctx context.Context, s codeSandboxStore, reg registry.RunnerRegistry, cfg *config.APIConfig, id string, shouldDelete func(*store.SandboxRecord) bool) (bool, error) {
	unlock, err := s.LockSandbox(ctx, id)
	if err != nil {
		return false, err
	}
	defer unlock()
	rec, err := s.GetCodeSandbox(id)
	if err != nil {
		return false, err
	}
	if rec == nil || !shouldDelete(rec) {
		return false, nil
	}
	deleteCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runnerLifecycleBudget)
	defer cancel()
	if err := runnerctl.DeleteSandbox(deleteCtx, resolveControlAddr(rec, reg), cfg.RunnerAPIKey, runnerControlTLS(cfg), rec.ID); err != nil {
		return false, &codeRunnerDeleteError{err: err}
	}
	if err := s.Delete(rec.ID); err != nil {
		return false, err
	}
	return true, nil
}
