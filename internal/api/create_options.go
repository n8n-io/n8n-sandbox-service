package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/store"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// maxCreateBodyBytes bounds a create body. An egress_proxy spec is the only
// large part; its lists have limits of their own (runnerruntime.EgressProxySpec).
const maxCreateBodyBytes = 64 << 10

// Reasons on create errors a caller acts on: an unknown template is a
// configuration error, and each mismatch tells a reconnecting caller whether
// the option can be changed in place or the sandbox is not the one it wants.
const (
	reasonUnknownTemplate     = "unknown_template"
	reasonEgressMismatch      = "egress_mismatch"
	reasonEgressProxyMismatch = "egress_proxy_mismatch"
	reasonTemplateMismatch    = "template_mismatch"
	reasonIdleWindowMismatch  = "idle_window_mismatch"
)

// createError is a 400 with a reason.
type createError struct {
	msg, reason string
}

func (e *createError) Error() string { return e.msg }

type createSandboxRequest struct {
	ID        *string `json:"id"`
	Ephemeral bool    `json:"ephemeral"`
	// The fields below are nil when left out, so a reconnect can tell "not
	// named" from "named as the default". "" is the default, as on create.
	Egress           *string         `json:"egress"`
	Template         *string         `json:"template"`
	IdleStopAfter    *int64          `json:"idle_stop_after"`
	IdleDeleteAfter  *int64          `json:"idle_delete_after"`
	IdleArchiveAfter *int64          `json:"idle_archive_after"`
	EgressProxy      json.RawMessage `json:"egress_proxy"`
}

// createParams is a validated create request.
type createParams struct {
	egress      runnerruntime.Egress
	egressGiven bool
	template    string // stored form; runnerruntime.DefaultTemplate when left out
	// templateGiven, idleStopGiven and idleDeleteGiven are true when the
	// request named the field, so a reconnect has to match it.
	templateGiven    bool
	idleStopAfter    int64 // seconds; 0 = global window
	idleStopGiven    bool
	idleDeleteAfter  int64 // seconds; 0 = global window
	idleDeleteGiven  bool
	idleArchiveAfter int64 // seconds; 0 = global window
	idleArchiveGiven bool
	egressProxy      *runnerruntime.EgressProxySpec
	egressProxyJSON  string // canonical JSON of egressProxy, stored and compared on reconnect
}

// parseCreateRequest validates a create body. Every error is a 400. idleLimit
// returns the largest idle window the caller may set; it is only called when
// the body sets one.
func parseCreateRequest(req *createSandboxRequest, cfg *config.APIConfig, idleLimit func() (int64, error)) (*createParams, error) {
	p := &createParams{template: runnerruntime.DefaultTemplate}

	egressField := ""
	if req.Egress != nil {
		egressField = *req.Egress
		p.egressGiven = true
	}
	egress, err := runnerruntime.ParseEgress(egressField)
	if err != nil {
		return nil, err
	}
	p.egress = egress

	if req.Template != nil {
		p.templateGiven = true
		if *req.Template != "" {
			p.template = *req.Template
		}
		if err := runnerruntime.ValidateTemplateName(p.template); err != nil {
			return nil, err
		}
		if p.template != runnerruntime.DefaultTemplate && !slices.Contains(cfg.Templates, p.template) {
			return nil, &createError{msg: fmt.Sprintf("unknown template %q", p.template), reason: reasonUnknownTemplate}
		}
	}

	if req.IdleArchiveAfter != nil {
		if !cfg.ArchiveEnabled() {
			return nil, errors.New("idle_archive_after needs an archive store, and this API has none")
		}
		if req.Ephemeral {
			return nil, errors.New("idle_archive_after is not allowed with ephemeral: ephemeral sandboxes are never archived")
		}
	}
	if req.IdleStopAfter != nil || req.IdleDeleteAfter != nil || req.IdleArchiveAfter != nil {
		limit, err := idleLimit()
		if err != nil {
			return nil, err
		}
		if req.IdleStopAfter != nil {
			if err := checkIdleWindow("idle_stop_after", *req.IdleStopAfter, limit); err != nil {
				return nil, err
			}
			p.idleStopAfter, p.idleStopGiven = *req.IdleStopAfter, true
		}
		if req.IdleDeleteAfter != nil {
			if err := checkIdleWindow("idle_delete_after", *req.IdleDeleteAfter, limit); err != nil {
				return nil, err
			}
			p.idleDeleteAfter, p.idleDeleteGiven = *req.IdleDeleteAfter, true
		}
		if req.IdleArchiveAfter != nil {
			if err := checkIdleWindow("idle_archive_after", *req.IdleArchiveAfter, limit); err != nil {
				return nil, err
			}
			p.idleArchiveAfter, p.idleArchiveGiven = *req.IdleArchiveAfter, true
		}
	}
	if err := checkWindowOrder(p.idleStopAfter, p.idleArchiveAfter, p.idleDeleteAfter); err != nil {
		return nil, err
	}

	specGiven := len(req.EgressProxy) > 0 && !bytes.Equal(bytes.TrimSpace(req.EgressProxy), []byte("null"))
	switch {
	case egress == runnerruntime.EgressProxy && !specGiven:
		return nil, errors.New(`egress "proxy" needs egress_proxy`)
	case egress != runnerruntime.EgressProxy && specGiven:
		return nil, errors.New(`egress_proxy is only allowed with egress "proxy"`)
	case specGiven:
		var spec runnerruntime.EgressProxySpec
		if err := json.Unmarshal(req.EgressProxy, &spec); err != nil {
			return nil, err
		}
		if err := spec.Validate(); err != nil {
			return nil, err
		}
		canonical, err := json.Marshal(&spec)
		if err != nil {
			return nil, err
		}
		p.egressProxy, p.egressProxyJSON = &spec, string(canonical)
	}
	return p, nil
}

// idleWindowLimit is the largest idle window, in seconds, a sandbox of tenantID
// may have: the server's cap, narrowed by the tenant's own. Admin-owned
// sandboxes get the server's.
func idleWindowLimit(s store.SandboxStore, cfg *config.APIConfig, tenantID string) (int64, error) {
	limit := int64(cfg.MaxIdleWindow / time.Second)
	if limit == 0 || tenantID == "" || store.IsAdminTenantID(tenantID) {
		return limit, nil
	}
	t, err := s.GetTenant(tenantID)
	if err != nil {
		return 0, err
	}
	if t != nil && t.MaxIdleWindow > 0 && t.MaxIdleWindow < limit {
		limit = t.MaxIdleWindow
	}
	return limit, nil
}

// checkIdleWindow bounds a per-sandbox idle window by limit seconds. 0 is
// refused: it would mean "never" to a reader of the global settings, and no
// create may opt its sandbox out of the sweeper.
func checkIdleWindow(field string, seconds, limit int64) error {
	if limit == 0 {
		return fmt.Errorf("%s is not allowed: SANDBOX_API_MAX_IDLE_WINDOW is 0", field)
	}
	minimum := int64(config.MinIdleWindow / time.Second)
	if seconds < minimum || seconds > limit {
		return fmt.Errorf("%s must be between %d and %d seconds", field, minimum, limit)
	}
	return nil
}

// runnerOptions is what the create RPC carries. Idle windows stay in the API,
// whose sweeper applies them; the default template goes as empty so a runner
// from before templates confirms it. A proxy sandbox carries its owner, under
// whose name alone the runner reads secrets.
func (p *createParams) runnerOptions(tenantID string) runnerruntime.CreateOptions {
	opts := runnerruntime.CreateOptions{Egress: p.egress, EgressProxy: p.egressProxy}
	if p.egress == runnerruntime.EgressProxy {
		opts.Tenant = tenantID
	}
	if p.template != runnerruntime.DefaultTemplate {
		opts.Template = p.template
	}
	return opts
}

// conflict returns why an existing sandbox cannot answer this request and the
// reason to report, or "" when it can. A field the request names has to be the
// sandbox's: a caller asking for "none" must not get an open sandbox back and
// take it for sealed. A field left out matches anything.
func (p *createParams) conflict(existing *store.SandboxRecord) (msg, reason string) {
	if p.egressGiven && string(p.egress) != existing.Egress {
		return "sandbox exists with egress " + existing.Egress, reasonEgressMismatch
	}
	if p.egressGiven && p.egressProxyJSON != existing.EgressProxy {
		return "sandbox exists with a different egress_proxy", reasonEgressProxyMismatch
	}
	if p.templateGiven && p.template != recordTemplate(existing) {
		return "sandbox exists with template " + recordTemplate(existing), reasonTemplateMismatch
	}
	if p.idleStopGiven && p.idleStopAfter != existing.IdleStopAfter {
		return "sandbox exists with a different idle_stop_after", reasonIdleWindowMismatch
	}
	if p.idleDeleteGiven && p.idleDeleteAfter != existing.IdleDeleteAfter {
		return "sandbox exists with a different idle_delete_after", reasonIdleWindowMismatch
	}
	if p.idleArchiveGiven && p.idleArchiveAfter != existing.IdleArchiveAfter {
		return "sandbox exists with a different idle_archive_after", reasonIdleWindowMismatch
	}
	return "", ""
}

// recordTemplate is the template a row was created with; rows written without
// one (only by tests: the column defaults to it) are the default.
func recordTemplate(rec *store.SandboxRecord) string {
	if rec.Template == "" {
		return runnerruntime.DefaultTemplate
	}
	return rec.Template
}

// checkWindowOrder requires stop <= archive <= delete among the windows set
// (0 is unset).
func checkWindowOrder(stop, archive, del int64) error {
	set := []struct {
		name string
		v    int64
	}{{"idle_stop_after", stop}, {"idle_archive_after", archive}, {"idle_delete_after", del}}
	for i := range set {
		for j := i + 1; j < len(set); j++ {
			if set[i].v > 0 && set[j].v > 0 && set[i].v > set[j].v {
				return fmt.Errorf("%s must not exceed %s", set[i].name, set[j].name)
			}
		}
	}
	return nil
}
