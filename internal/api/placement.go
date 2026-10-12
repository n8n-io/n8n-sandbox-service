package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/grpc/pb"
	"github.com/n8n-io/sandbox-service/internal/api/registry"
	"github.com/n8n-io/sandbox-service/internal/api/runnerctl"
	"github.com/n8n-io/sandbox-service/internal/metrics"
	"github.com/n8n-io/sandbox-service/internal/obs"
)

// Bounds on placing one create: how many runners it may try, and how long after
// the first attempt another may start. A refusal arrives before the runner does
// any work, so attempts are normally milliseconds apart; the budget covers the
// case where they are not. Vars so tests can shrink them.
var (
	createPlacementAttempts = 3
	createPlacementBudget   = 5 * time.Second
)

// reasonNoCapacity marks a 503 for which nothing was created, so the client can
// repeat the create even without a caller-supplied id.
const reasonNoCapacity = "no_capacity"

// capacityRetryAfter is the Retry-After sent with a reasonNoCapacity 503.
const capacityRetryAfter = "1"

// errRunnersAtCapacity is returned when every runner a create tried refused it.
var errRunnersAtCapacity = errors.New("sandbox runners are at capacity")

// writeNoCapacity answers a create no runner took.
func writeNoCapacity(w http.ResponseWriter, err error) {
	w.Header().Set("Retry-After", capacityRetryAfter)
	writeErrorReason(w, http.StatusServiceUnavailable, err.Error(), reasonNoCapacity)
}

// placement is a runner that took a create.
type placement struct {
	run  *registry.Runner
	resp *pb.CreateSandboxResponse
}

// placeSandbox creates the sandbox on the eligible runner with the lowest
// capacity_used. A runner that answers with runnerctl.CapacityRefusal has
// created nothing, so the create moves on to the next runner, up to
// createPlacementAttempts runners and no later than createPlacementBudget after
// the first attempt. Any other runner error ends the placement, because that
// runner may have created the sandbox.
//
// Returns registry.ErrNoRunners when no runner was eligible and
// errRunnersAtCapacity when every runner tried refused; in both cases nothing
// was created. logAttrs are added to each runner-selected event.
func placeSandbox(r *http.Request, reg registry.RunnerRegistry, cfg *config.APIConfig, rec *metrics.APIRecorder, sandboxID, createOptionsJSON string, logAttrs ...any) (*placement, error) {
	ctx := r.Context()
	tlsCfg := runnerControlTLS(cfg)
	start := time.Now()
	var refusedBy []string
	attempts := 0
	defer func() { obs.FieldsFrom(ctx).Add("placement_attempts", attempts) }()

	for {
		run, err := reg.PickLowestUsed(refusedBy...)
		if errors.Is(err, registry.ErrNoRunners) && len(refusedBy) > 0 {
			break
		}
		if errors.Is(err, registry.ErrNoRunners) {
			rec.ObservePlacement(metrics.PlacementNoRunner)
			slog.WarnContext(ctx, "create sandbox failed: no eligible runners", "sandbox_id", sandboxID)
			return nil, err
		}
		if err != nil {
			slog.ErrorContext(ctx, "create sandbox failed: pick runner", "sandbox_id", sandboxID, "error", err)
			return nil, err
		}

		slog.InfoContext(ctx, "create sandbox: runner selected", append([]any{
			"sandbox_id", sandboxID,
			"runner_id", run.ID,
			"runner_http_base_url", run.HTTPBaseURL,
			"runner_control_grpc_addr", run.ControlGRPCAddr,
			"runner_healthy", run.Healthy,
			"runner_capacity_total", run.CapacityTotal,
			"runner_capacity_used", run.CapacityUsed,
			"runner_capacity_stopped", run.CapacityStopped,
			"attempt", attempts + 1,
		}, logAttrs...)...)
		// Not the client's context: a disconnect must not cancel a create the runner
		// finishes anyway (see runnerCreateBudget). Fresh for every attempt, so a
		// refusal never shortens the next runner's deadline.
		createCtx, cancelCreate := context.WithTimeout(context.WithoutCancel(ctx), runnerCreateBudget)
		resp, err := runnerctl.CreateSandbox(createCtx, run.ControlGRPCAddr, cfg.RunnerAPIKey, tlsCfg, sandboxID, createOptionsJSON)
		cancelCreate()
		attempts++

		if !runnerctl.IsCapacityRefusal(err) {
			if len(refusedBy) == 0 {
				rec.ObservePlacement(metrics.PlacementFirstChoice)
			} else {
				rec.ObservePlacement(metrics.PlacementRetried)
			}
			obs.FieldsFrom(ctx).Add("runner_id", run.ID)
			if err != nil {
				slog.ErrorContext(
					ctx,
					"create sandbox failed: runner control create",
					"sandbox_id", sandboxID,
					"runner_id", run.ID,
					"runner_control_grpc_addr", run.ControlGRPCAddr,
					"error", err,
				)
				return nil, fmt.Errorf("failed to create container: %w", err)
			}
			return &placement{run: run, resp: resp}, nil
		}

		refusedBy = append(refusedBy, run.ID)
		slog.WarnContext(ctx, "create sandbox: runner refused for capacity",
			"sandbox_id", sandboxID, "runner_id", run.ID, "attempt", attempts, "error", err)
		// A client that has gone gets no new attempt: nothing is in flight, so there
		// is nothing for the API to finish on its behalf.
		if attempts >= createPlacementAttempts || time.Since(start) >= createPlacementBudget || ctx.Err() != nil {
			break
		}
	}

	rec.ObservePlacement(metrics.PlacementExhausted)
	slog.WarnContext(ctx, "create sandbox failed: runners refused for capacity",
		"sandbox_id", sandboxID, "refused_by", refusedBy)
	return nil, errRunnersAtCapacity
}
