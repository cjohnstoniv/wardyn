// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Agent providers: the org-level policy for WHICH coding agents this deployment
// offers and the model provider each uses by default. This file owns the write
// boundary — validation, the two endpoints, the audit row — and the predicates
// every later site reads:
//
//	agentProvidersConfigured(sc)       is this install in legacy open mode?
//	agentProviderFor(sc, agentID)      the row governing this agent, if any
//	(*Server) agentRosterRefusal(...)  the launch-path refusal, one spelling
//
// The predicates live here, next to the validation that shapes the rows they
// read, so there is exactly ONE spelling of "is this agent offered" — the call
// sites (run create, the record launcher, the setup roster) only ask.
//
// What this file does not do, so the seams are findable: it never chooses a
// run's model provider (chooseModelProvider reads the default from here), never
// captures a credential, and never renders a console surface.
package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// agent400 / agent422 — the refusal bodies this file writes.
//
// DRAFT (M2 canon pending): every string here is provisional until the owner's
// canon sitting freezes it. They are constants, and the tests assert THROUGH the
// constants, so the post-sitting swap is a one-file diff with no test churn.
const (
	agent400Unknown = "agents: %q names no agent this deployment can run — a catalog id or a WARDYN_AGENT_IMAGES key"
	agent400DupID   = "agents: id %q is not unique"
	agent412Stale   = "agents changed since you loaded them — reload and retry"

	// The default model provider. A turned-off provider is still a valid
	// default (it is the one-click incident switch); a missing one, or one not
	// enabled for the agent, is not.
	agent400DefaultUnknown    = "agents: %q: default_provider %q names no model provider — add it first, or choose another default"
	agent400DefaultNotServing = "agents: %q: default_provider %q is not enabled for %q — enable it for this agent, or choose another default"

	// AGENT_422 — the ONE launch-path refusal, and the one string in this file a
	// MEMBER ever reads. It names the agent and nothing else: no base URL, no
	// secret name, no mechanism, no start URL.
	agent422NotEnabled = "agent: %q is not an enabled agent on this deployment — ask an admin"
)

// errAgentNotEnabled marks a step-run launch refused by the agent roster, so the
// handler can answer 422 (the status run create answers for the identical cause)
// instead of the 500 every other launch failure maps to — the errRecordCeilingLimit
// precedent, and for the same reason: a policy decision must not read as a fault.
var errAgentNotEnabled = errors.New("agent not enabled")

// mountAgentProviderRoutes registers the two agent-roster endpoints. A mount
// rather than two lines in routes() for the reason every other route family is:
// routes() sits at its funlen ratchet and the file's own route-tier map lists
// mounts, not individual routes.
//
// operatorOnly for BOTH, the sibling /workspace-providers reasoning: the block
// names an org's model-provider choices. The member-safe projection already
// exists and is deliberately a DIFFERENT, narrower document: SetupHarnessTool
// (setup_integrations.go), which carries whether each agent is enabled. There is
// no DELETE: removing a row is a PUT without it.
func (s *Server) mountAgentProviderRoutes(operatorOnly chi.Router) {
	operatorOnly.Get("/agent-providers", s.handleGetAgentProviders)
	operatorOnly.Put("/agent-providers", s.handlePutAgentProviders)
}

// agentProviderRows is the stored agent rows, or nil. Every predicate starts
// here so the nil-block and empty-slice cases can never disagree.
func agentProviderRows(sc types.SiteConfig) []types.AgentProvider {
	if sc.AgentProviders == nil {
		return nil
	}
	return sc.AgentProviders.Agents
}

// agentProvidersConfigured reports whether this install has an agent roster at
// all — the ONE place "legacy open mode" is decided for agents.
//
// Configured == a non-nil block, not "has rows", and the two cannot diverge:
// normalizeAgentProviders turns an empty block back into nil on every write
// (the {} clear form), so a stored block always carries at least one row. A
// caller therefore never has to decide what a present-but-rowless block means —
// it cannot be stored.
func agentProvidersConfigured(sc types.SiteConfig) bool {
	return sc.AgentProviders != nil
}

// agentProviderFor returns the row governing agentID, and whether one exists.
// Lookup is exact on the --agent string: ids are never folded or normalized (the
// image map is keyed by the same literal), so a row and a run name the same agent
// or they do not.
func agentProviderFor(sc types.SiteConfig, agentID string) (types.AgentProvider, bool) {
	for _, row := range agentProviderRows(sc) {
		if row.ID == agentID {
			return row, true
		}
	}
	return types.AgentProvider{}, false
}

// agentEnabled reports whether a run naming agentID may launch. Legacy open mode
// (no block) admits everything, which is what makes an upgraded install
// byte-identical to what it was; with a block, an agent is offered only if it has
// a row and that row is on.
func agentEnabled(sc types.SiteConfig, agentID string) bool {
	if !agentProvidersConfigured(sc) {
		return true
	}
	row, ok := agentProviderFor(sc, agentID)
	return ok && !row.Disabled
}

// agentRosterRefusal is the ONE spelling of the launch-path check: it returns the
// member-safe refusal sentence when agent has no enabled row, "" when the launch
// may proceed, and an error only when the site config could not be read.
//
// An EMPTY agent is always "" — a task_mode=exec run naming an image and no agent
// is a thing agentRequirementError deliberately still admits, and a roster of
// agents has nothing to say about a run that names none.
//
// A read failure is returned as itself rather than swallowed in either direction:
// admitting on a database hiccup would silently reopen the roster, and refusing
// would blame the caller for an outage.
func (s *Server) agentRosterRefusal(ctx context.Context, agent string) (string, error) {
	if agent == "" || s.cfg.Store == nil {
		return "", nil
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return "", err
	}
	if agentEnabled(sc, agent) {
		return "", nil
	}
	return fmt.Sprintf(agent422NotEnabled, agent), nil
}

// recordRosterRefusal is agentRosterRefusal in the shape a step-run launcher
// needs: one error, already wrapped in errAgentNotEnabled so the handler can
// answer 422 rather than 500 (record.go maps it).
func (s *Server) recordRosterRefusal(ctx context.Context, agent string) error {
	msg, err := s.agentRosterRefusal(ctx, agent)
	if err != nil {
		return fmt.Errorf("read agent roster: %w", err)
	}
	if msg != "" {
		return fmt.Errorf("%w: %s", errAgentNotEnabled, msg)
	}
	return nil
}

// storedAgentProviders is the stored block as a VALUE — the shape
// GET /agent-providers returns and the shape both doors' ETag is computed over,
// so a nil pointer and a present-but-empty block are one document.
func storedAgentProviders(sc types.SiteConfig) types.AgentProviders {
	if sc.AgentProviders == nil {
		return types.AgentProviders{}
	}
	return *sc.AgentProviders
}

// normalizeAgentProviders canonicalizes a block on its way to storage and returns
// nil for an EMPTY one, which is what makes {} the clear form on both doors:
// without it, a caller who cleared their last row would leave
// "agent_providers":{} rendered on every GET /site-config forever.
//
// Only whitespace is trimmed. Ids are NOT folded: their grammar is exact (an id
// must equal a catalog id or an image-map key verbatim), so an off-case value is
// a refusal at the write boundary, never a silent rewrite of what the admin
// wrote down.
func normalizeAgentProviders(p *types.AgentProviders) *types.AgentProviders {
	if p == nil {
		return nil
	}
	for i := range p.Agents {
		p.Agents[i].ID = strings.TrimSpace(p.Agents[i].ID)
		p.Agents[i].DefaultProvider = strings.TrimSpace(p.Agents[i].DefaultProvider)
	}
	if p.Empty() {
		return nil
	}
	return p
}

// validateAgentProviders is the ONE write-boundary gate both doors run
// (PUT /agent-providers and PUT /site-config) — a nil block is valid (legacy open
// mode), so the common "not configured" case costs nothing. Each row names an
// agent this deployment can run: a harness-catalog id or a key of images, the
// boot agent-image map (Config.AgentImages). Its default is checked against the
// model providers by validateDefaultProviders, which needs both blocks.
func validateAgentProviders(p *types.AgentProviders, images map[string]string) error {
	if p == nil {
		return nil
	}
	seen := map[string]bool{}
	for _, row := range p.Agents {
		if seen[row.ID] {
			return fmt.Errorf(agent400DupID, row.ID)
		}
		seen[row.ID] = true
		if _, inCatalog := harnessByID(row.ID); !inCatalog {
			if _, known := images[row.ID]; !known {
				return fmt.Errorf(agent400Unknown, row.ID)
			}
		}
	}
	return nil
}

// validateDefaultProviders cross-checks the roster's defaults against the
// model-provider block they name. It needs BOTH blocks, so each door runs it
// once it holds the pair it is about to store: PUT /agent-providers against the
// stored providers, PUT /model-providers against the stored roster, and PUT
// /site-config against the document after its carry-forward.
func validateDefaultProviders(roster *types.AgentProviders, providers *types.ModelProviders) error {
	if roster == nil {
		return nil
	}
	for _, row := range roster.Agents {
		if row.DefaultProvider == "" {
			continue
		}
		mp, ok := modelProviderByID(providers, row.DefaultProvider)
		if !ok {
			return fmt.Errorf(agent400DefaultUnknown, row.ID, row.DefaultProvider)
		}
		if !mp.Serves(row.ID) {
			return fmt.Errorf(agent400DefaultNotServing, row.ID, row.DefaultProvider, row.ID)
		}
	}
	return nil
}

// handleGetAgentProviders returns the stored agent roster.
//
// The response carries an ETag so a caller that means to base a later PUT on
// exactly this read can send it back as If-Match; a never-configured install gets
// the zero-value document with 200, never a 404.
//
//	GET /api/v1/agent-providers
func (s *Server) handleGetAgentProviders(w http.ResponseWriter, r *http.Request) {
	sc, err := s.cfg.Store.GetSiteConfig(r.Context())
	if err != nil {
		writeServerError(w, r, "get site config", err)
		return
	}
	block := storedAgentProviders(sc)
	w.Header().Set("ETag", computeETag(block))
	writeJSON(w, http.StatusOK, block)
}

// handlePutAgentProviders replaces the WHOLE agent roster: there is no delete
// route because removing a row is PUTting the document without it, and {} is the
// clear form (normalizeAgentProviders turns it back into an absent key rather
// than an empty object rendered forever).
//
// If-Match (etag.go) is optional optimistic concurrency, exactly as the sibling
// /workspace-providers PUT: absent behaves as last-writer-wins, a present-but-stale
// value is refused with 412 before the write reaches the store.
//
//	PUT /api/v1/agent-providers
func (s *Server) handlePutAgentProviders(w http.ResponseWriter, r *http.Request) {
	var body types.AgentProviders
	if !decodeStrict(w, r, &body) {
		return
	}
	block := normalizeAgentProviders(&body)
	if err := validateAgentProviders(block, s.cfg.AgentImages); err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonSiteConfigInvalid, "invalid agent providers: "+err.Error())
		return
	}
	// SEAM-1: serializes this read-modify-write against the site config's other
	// writers (handlePutSiteConfig, handlePutWorkspaceProviders and the two
	// integration handlers), which read and rewrite the SAME singleton document —
	// see handlePutIntegration's SEAM-1 comment for why an unguarded RMW here
	// silently erases a concurrent one.
	s.siteConfigMu.Lock()
	defer s.siteConfigMu.Unlock()
	ctx := r.Context()
	existing, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		writeServerError(w, r, "get site config", err)
		return
	}
	if !ifMatchSatisfied(r, computeETag(storedAgentProviders(existing))) {
		writeErrorReason(w, http.StatusPreconditionFailed, reasonSiteConfigStale, agent412Stale)
		return
	}
	if err := validateDefaultProviders(block, existing.ModelProviders); err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonSiteConfigInvalid, "invalid agent providers: "+err.Error())
		return
	}
	candidate := existing
	candidate.AgentProviders = block
	// EffectiveScmHosts is projected on read and never stored — a value that rode
	// in on a GET-spread body would otherwise be persisted into the JSONB.
	candidate.EffectiveScmHosts = nil
	candidate.WithheldScmHosts = nil
	saved, err := s.cfg.Store.PutSiteConfig(ctx, candidate)
	if err != nil {
		writeServerError(w, r, "put site config", err)
		return
	}
	savedBlock := storedAgentProviders(saved)
	s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"agent_provider.write", "agent_providers", "success",
		mustJSON(agentProviderAuditData(savedBlock))))
	w.Header().Set("ETag", computeETag(savedBlock))
	writeJSON(w, http.StatusOK, savedBlock)
}

// agentProviderAuditData is agent_provider.write's datum: what an incident review
// actually needs — how many rows, which agents, which are off, and which model
// provider each one defaults to.
func agentProviderAuditData(block types.AgentProviders) map[string]any {
	ids, disabled := []string{}, []string{}
	var defaults []string
	for _, row := range block.Agents {
		ids = append(ids, row.ID)
		if row.DefaultProvider != "" {
			defaults = append(defaults, row.ID+":"+row.DefaultProvider)
		}
		if row.Disabled {
			disabled = append(disabled, row.ID)
		}
	}
	slices.Sort(ids)
	slices.Sort(disabled)
	datum := map[string]any{"agent_count": len(block.Agents), "ids": ids, "disabled": disabled}
	// Only when a row names one, so a roster with no defaults writes the row it
	// always wrote.
	if len(defaults) > 0 {
		slices.Sort(defaults)
		datum["defaults"] = defaults
	}
	return datum
}

// enabledAgentProviderCount is site_config.write's count of ENABLED rows — the
// number that says how much this document narrows, which a disabled row does not
// contribute to.
func enabledAgentProviderCount(sc types.SiteConfig) int {
	n := 0
	for _, row := range agentProviderRows(sc) {
		if !row.Disabled {
			n++
		}
	}
	return n
}
