package api

import (
	"net/http"

	"github.com/n8n-io/sandbox-service/internal/limits"
	"github.com/n8n-io/sandbox-service/internal/metrics"
)

// reasonTenantRequestLimit tells a 429 from the per-tenant cap apart from the
// runner's per-sandbox one, which passes through with a reason of its own.
const reasonTenantRequestLimit = "tenant_request_limit"

// limitRetryAfter is the Retry-After sent with every limit response. A slot
// frees as soon as any request in progress finishes, so there is no point in
// asking a client to wait longer.
const limitRetryAfter = "1"

// maxExecutionDeletesPerTenant caps a tenant's execution DELETEs in progress
// on this replica. They are counted apart from its other requests, so it can
// still cancel work when its limit is full. Bounded, because a DELETE to a
// stalled daemon holds its slot too.
const maxExecutionDeletesPerTenant = 16

// tenantLimit wraps a route in a per-tenant cap held in l, counted per
// replica. Only tenant keys count: an admin key is the operator's own and a
// self-hosted deployment may use nothing else.
//
// The slot is taken before the sandbox lookup and keyed on the caller's own
// tenant, so a 429 says nothing about a sandbox the caller may not see.
func tenantLimit(l *limits.KeyedLimiter, rec *metrics.APIRecorder) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			id, ok := authFromContext(r.Context())
			if !ok || id.Role != roleTenant {
				next(w, r)
				return
			}
			if !l.TryAcquire(id.TenantID) {
				rec.ObserveRequestLimited(metrics.LimitTenant)
				w.Header().Set("Retry-After", limitRetryAfter)
				writeErrorReason(w, http.StatusTooManyRequests, "too many requests in progress for this tenant", reasonTenantRequestLimit)
				return
			}
			defer l.Release(id.TenantID)
			next(w, r)
		}
	}
}
