package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/n8n-io/sandbox-service/internal/api/config"
	"github.com/n8n-io/sandbox-service/internal/api/store"
	runnerruntime "github.com/n8n-io/sandbox-service/internal/runner/runtime"
)

// maxCreateBodyBytes bounds a create body. An egress_proxy spec is the only
// large part; its lists have limits of their own (runnerruntime.EgressProxySpec).
const maxCreateBodyBytes = 64 << 10

// The API serves only the default template: every runner has one image.
var knownTemplates = map[string]bool{runnerruntime.DefaultTemplate: true}

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
	Egress          *string         `json:"egress"`
	Template        *string         `json:"template"`
	IdleStopAfter   *int64          `json:"idle_stop_after"`
	IdleDeleteAfter *int64          `json:"idle_delete_after"`
	EgressProxy     json.RawMessage `json:"egress_proxy"`
}

// createParams is a validated create request.
type createParams struct {
	egress      runnerruntime.Egress
	egressGiven bool
	template    string // stored form; runnerruntime.DefaultTemplate when left out
	// templateGiven, idleStopGiven and idleDeleteGiven are true when the
	// request named the field, so a reconnect has to match it.
	templateGiven   bool
	idleStopAfter   int64 // seconds; 0 = global window
	idleStopGiven   bool
	idleDeleteAfter int64 // seconds; 0 = global window
	idleDeleteGiven bool
	egressProxy     *runnerruntime.EgressProxySpec
	egressProxyJSON string // canonical JSON of egressProxy, stored and compared on reconnect
}

// parseCreateRequest validates a create body. Every error is a 400.
func parseCreateRequest(req *createSandboxRequest, cfg *config.APIConfig) (*createParams, error) {
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
		if !knownTemplates[p.template] {
			return nil, &createError{msg: fmt.Sprintf("unknown template %q", p.template), reason: reasonUnknownTemplate}
		}
	}

	if req.IdleStopAfter != nil {
		if err := checkIdleWindow("idle_stop_after", *req.IdleStopAfter, cfg); err != nil {
			return nil, err
		}
		p.idleStopAfter, p.idleStopGiven = *req.IdleStopAfter, true
	}
	if req.IdleDeleteAfter != nil {
		if err := checkIdleWindow("idle_delete_after", *req.IdleDeleteAfter, cfg); err != nil {
			return nil, err
		}
		p.idleDeleteAfter, p.idleDeleteGiven = *req.IdleDeleteAfter, true
	}
	if p.idleStopGiven && p.idleDeleteGiven && p.idleStopAfter > p.idleDeleteAfter {
		return nil, errors.New("idle_stop_after must not exceed idle_delete_after")
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

// checkIdleWindow bounds a per-sandbox idle window. 0 is refused: it would
// mean "never" to a reader of the global settings, and no create may opt its
// sandbox out of the sweeper.
func checkIdleWindow(field string, seconds int64, cfg *config.APIConfig) error {
	limit := int64(cfg.MaxIdleWindow / time.Second)
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
// from before templates confirms it.
func (p *createParams) runnerOptions() runnerruntime.CreateOptions {
	opts := runnerruntime.CreateOptions{Egress: p.egress, EgressProxy: p.egressProxy}
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
