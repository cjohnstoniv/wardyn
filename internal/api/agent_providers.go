// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Agent providers: the org-level policy for WHICH coding agents this deployment
// offers and how each one reaches its model. This file owns the write boundary —
// validation, the two endpoints, the audit row — and the predicates every later
// site reads:
//
//	agentProvidersConfigured(sc)       is this install in legacy open mode?
//	agentProviderFor(sc, agentID)      the row governing this agent, if any
//	(*Server) agentRosterRefusal(...)  the launch-path refusal, one spelling
//
// The predicates live here, next to the validation that shapes the rows they
// read, so there is exactly ONE spelling of "is this agent offered" — the call
// sites (run create, the record launcher, the setup roster) only ask.
//
// What this file does not do, so the seams are findable: it never enforces a
// row's Mechanism at dispatch (that is enforceConfiguredLLMMechanism's, which
// reads these rows), never captures a per-user credential, and never renders a
// console surface.
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
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
	agent400Unknown           = "agents: %q names no agent this deployment can run — a catalog id or a WARDYN_AGENT_IMAGES key"
	agent400CustomMechanism   = "agents: %q is not in the agent catalog, so its mechanism must be none — Wardyn wires no model credential into a custom image (%s would show a live credential that binds nothing)"
	agent400PerUser           = "agents: %q: credential_source per_user is available for %s only, not %s"
	agent400SSOStartURL       = "agents: %q: sso_start_url is required when mechanism is bedrock_sso and credential_source is per_user"
	agent400SSOStartURLUnused = "agents: %q: sso_start_url applies only when mechanism is bedrock_sso and credential_source is per_user"

	// The account/role PIN. Admin-owned beside the start URL
	// and for the same reason: the sign-in proposes, the roster disposes.
	agent400SSOPinUnused = "agents: %q: sso_account_id and sso_role_name apply only when mechanism is bedrock_sso and credential_source is per_user"
	agent400SSOPinPair   = "agents: %q: sso_account_id and sso_role_name are set together or not at all — pinning the account alone still leaves the role picked for whoever signs in"
	agent400SSOAccountID = "agents: %q: sso_account_id must be a 12-digit AWS account id"
	agent400SSORoleName  = "agents: %q: sso_role_name must be an IAM role name — letters, digits and +=,.@_- , at most 64 characters"
	// agentWarnSSOPinModelAccount is a WARNING now, not a refusal body:
	// the pin outranks the model's account, and this is how the disagreement is
	// spoken once rather than silently.
	agentWarnSSOPinModelAccount = "agents: %q: sso_account_id %s is not the account the configured Bedrock model lives in (%s) — the pin is taken as written, so runs will only work if that model is shared with the pinned account"
	agent400DupID               = "agents: id %q is not unique"
	agent400Mechanism           = "agents[%d].mechanism: %q is not a model-access mechanism — want one of: %s"
	agent400Source              = "agents[%d].credential_source: %q is not a credential source — want one of: %s"
	agent400NoManagedAuth       = "agents: %q is the bring-your-own-agent row, which Wardyn wires no model credential for, so its mechanism must be none"
	agent400NeedsLane           = "agents: %q is a catalog agent Wardyn can wire a model credential for, so mechanism none would leave every run of it without one — name the lane you configured"
	agent412Stale               = "agents changed since you loaded them — reload and retry"

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
// names an org's model-provider choices and, under per_user, its AWS access
// portal URL — corporate topology. The member-safe projection already exists and
// is deliberately a DIFFERENT, narrower document: the three per-row fields on
// SetupHarnessTool (setup_integrations.go), which carry enabled/mechanism/source
// and never the start URL. There is no DELETE: removing a row is a PUT without it.
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
// Only whitespace is trimmed. Ids, mechanisms and sources are NOT folded: their
// grammars are exact (an id must equal a catalog id or an image-map key
// verbatim), so an off-case value is a refusal at the write boundary, never a
// silent rewrite of what the admin wrote down.
func normalizeAgentProviders(p *types.AgentProviders) *types.AgentProviders {
	if p == nil {
		return nil
	}
	for i := range p.Agents {
		p.Agents[i].ID = strings.TrimSpace(p.Agents[i].ID)
		p.Agents[i].SSOStartURL = strings.TrimSpace(p.Agents[i].SSOStartURL)
		p.Agents[i].SSOAccountID = strings.TrimSpace(p.Agents[i].SSOAccountID)
		p.Agents[i].SSORoleName = strings.TrimSpace(p.Agents[i].SSORoleName)
	}
	if p.Empty() {
		return nil
	}
	return p
}

// validateAgentProviders is the ONE write-boundary gate both doors run
// (PUT /agent-providers and PUT /site-config) — a nil block is valid (legacy open
// mode), so the common "not configured" case costs nothing.
//
// images is the boot agent-image map (Config.AgentImages) and bedrockModel the
// boot Bedrock model (Config.BedrockModel): they are parameters rather than
// package reads because these are the validations on this struct that need
// SERVER state, and passing them keeps the gate callable from a test with a
// two-entry map and a one-line model string. BOTH doors pass the same values —
// a rule that held on one door and not the other is a rule an admin can write
// their way around.
func validateAgentProviders(p *types.AgentProviders, images map[string]string, bedrockModel string) error {
	if p == nil {
		return nil
	}
	seen := map[string]bool{}
	for i, row := range p.Agents {
		if seen[row.ID] {
			return fmt.Errorf(agent400DupID, row.ID)
		}
		seen[row.ID] = true
		if !row.Mechanism.Valid() {
			return fmt.Errorf(agent400Mechanism, i, string(row.Mechanism),
				strings.Join(types.ClosedAgentMechanismList(), ", "))
		}
		if row.CredentialSource != "" && !row.CredentialSource.Valid() {
			return fmt.Errorf(agent400Source, i, string(row.CredentialSource),
				strings.Join(types.ClosedCredentialSourceList(), ", "))
		}
		if err := validateAgentMechanism(row, images); err != nil {
			return err
		}
		if err := validateAgentCredentialSource(row, bedrockModel); err != nil {
			return err
		}
	}
	return nil
}

// validateAgentMechanism decides whether THIS agent can actually be reached on
// THIS lane, and it is the check that keeps the Agents tab honest: a row the
// dispatch path cannot honour would render a "live" chip that binds nothing.
//
// Three cases, in the order they are decided:
//
//  1. Not in the catalog (a WARDYN_AGENT_IMAGES key) — the only valid mechanism
//     is none. No code path can honour another: resolveBedrockAuth is not-ready
//     for a non-claude-code agent, managedInjectReady is claude-code-only, and
//     hasAnthropicAPIKeyInjection is false without a catalog Gateway.
//  2. The BYOA catalog row (NoManagedAuth) — mechanism none, for the same reason
//     stated as the catalog's own flag rather than as an id literal.
//  3. A catalog row with lanes — the mechanism FOLDS to the coarse provider type
//     harnessDef.ProviderTypes is keyed by ("bedrock" for all four sub-lanes),
//     and an impossible pair is refused with that map's EXISTING verbatim reason,
//     which is reviewed user-facing copy (harness.go's reasonX* constants). Never
//     a second opinion about what Codex CLI can speak.
func validateAgentMechanism(row types.AgentProvider, images map[string]string) error {
	def, inCatalog := harnessByID(row.ID)
	if !inCatalog {
		if _, known := images[row.ID]; !known {
			return fmt.Errorf(agent400Unknown, row.ID)
		}
		if row.Mechanism != types.AgentMechanismNone {
			return fmt.Errorf(agent400CustomMechanism, row.ID, string(row.Mechanism))
		}
		return nil
	}
	if def.NoManagedAuth {
		if row.Mechanism != types.AgentMechanismNone {
			return fmt.Errorf(agent400NoManagedAuth, row.ID)
		}
		return nil
	}
	if row.Mechanism == types.AgentMechanismNone {
		return fmt.Errorf(agent400NeedsLane, row.ID)
	}
	if reason := def.ProviderTypes[row.Mechanism.ProviderType()]; reason != "" {
		return errors.New(reason)
	}
	return nil
}

// validateAgentCredentialSource holds the per-user half: per_user is for the
// lanes whose credential a member can hold as their own (types.PerUserMechanisms
// — the captured AWS SSO session they sign in for, and the bedrock-api-key
// bearer they store under their own principal), and a per-user bedrock_sso row
// MUST carry the admin-owned start URL every principal signs in against —
// validated by the same gate the login request's own URL passes, so the two
// cannot disagree about what an access-portal URL is.
//
// The start URL and the pin stay bedrock_sso's ALONE, on a per-user row as much
// as on any other: a per-user BEARER row has no portal to sign in against and no
// sign-in identity to pin, so a value accepted there would be exactly the defect
// this forbids elsewhere — an admin believing they pinned a portal nothing reads.
func validateAgentCredentialSource(row types.AgentProvider, bedrockModel string) error {
	perUser := row.CredentialSource == types.CredentialSourcePerUser
	if perUser && !types.PerUserMechanisms[row.Mechanism] {
		return fmt.Errorf(agent400PerUser, row.ID,
			strings.Join(types.PerUserMechanismList(), " and "), string(row.Mechanism))
	}
	if !perUser || row.Mechanism != types.AgentMechanismBedrockSSO {
		if row.SSOStartURL != "" {
			return fmt.Errorf(agent400SSOStartURLUnused, row.ID)
		}
		if row.SSOAccountID != "" || row.SSORoleName != "" {
			return fmt.Errorf(agent400SSOPinUnused, row.ID)
		}
		return nil
	}
	if row.SSOStartURL == "" {
		return fmt.Errorf(agent400SSOStartURL, row.ID)
	}
	if err := validateSSOStartURL(row.SSOStartURL); err != nil {
		return fmt.Errorf("agents: %q: %w", row.ID, err)
	}
	return validateAgentSSOPin(row, bedrockModel)
}

// iamRoleName is the pinned role's real grammar; the account half reuses
// awssso_pin.go's awsAccountID rather than compiling `^\d{12}$` a second time
// for the same concept. Both fields are baked VERBATIM into every later Bedrock
// run's generated ~/.aws/config INI (awsSSOConfigFileContents, runs_bedrock.go),
// so the write boundary holds them to what AWS actually accepts rather than
// merely to "non-empty" — a newline in either would otherwise smuggle extra
// keys into that file, and a wrong-but-plausible value would be discovered as
// somebody's 403.
var iamRoleName = regexp.MustCompile(`^[A-Za-z0-9+=,.@_-]{1,64}$`)

// validateAgentSSOPin is the SAVE-time door, and it is the EARLIEST of
// the three places a wrong identity is refused (roster save, sign-in, capture).
//
// The pin is OPTIONAL — a single-account deployment never had this problem and
// is not made to answer a question it does not have — but optional as a PAIR:
// pinning the account alone leaves the role picked for whoever signs in, which
// is the same defect one level down.
//
// The model check is a WARNING, not a refusal. Wardyn holds both halves
// already, so an admin pinning an account the configured model does not live in
// hears about it here rather than from a member three sign-ins later — but a
// resource-shared application inference profile owned by another account is a
// real, supported AWS shape, and refusing left that deployment with no
// configuration that worked at all. The pin is the admin's deliberate answer to
// "which account signs in", so it wins and the disagreement is spoken once, with
// both accounts named. It is SILENT when the configured model names no account —
// a bare cross-region profile id is the common case, and there is nothing to
// compare.
func validateAgentSSOPin(row types.AgentProvider, bedrockModel string) error {
	if row.SSOAccountID == "" && row.SSORoleName == "" {
		return nil
	}
	if row.SSOAccountID == "" || row.SSORoleName == "" {
		return fmt.Errorf(agent400SSOPinPair, row.ID)
	}
	if !awsAccountID.MatchString(row.SSOAccountID) {
		return fmt.Errorf(agent400SSOAccountID, row.ID)
	}
	if !iamRoleName.MatchString(row.SSORoleName) {
		return fmt.Errorf(agent400SSORoleName, row.ID)
	}
	if modelAccount := bedrockModelAccount(bedrockModel); modelAccount != "" && modelAccount != row.SSOAccountID {
		slog.Warn("wardynd: "+fmt.Sprintf(agentWarnSSOPinModelAccount, row.ID, row.SSOAccountID, modelAccount),
			slog.String("agent", row.ID), slog.String("sso_account_id", row.SSOAccountID),
			slog.String("model_account_id", modelAccount))
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
		writeError(w, http.StatusInternalServerError, "get site config: "+err.Error())
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
	if err := validateAgentProviders(block, s.cfg.AgentImages, s.cfg.BedrockModel); err != nil {
		writeError(w, http.StatusBadRequest, "invalid agent providers: "+err.Error())
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
		writeError(w, http.StatusInternalServerError, "get site config: "+err.Error())
		return
	}
	if !ifMatchSatisfied(r, computeETag(storedAgentProviders(existing))) {
		writeError(w, http.StatusPreconditionFailed, agent412Stale)
		return
	}
	candidate := existing
	candidate.AgentProviders = block
	// EffectiveScmHosts is projected on read and never stored — a value that rode
	// in on a GET-spread body would otherwise be persisted into the JSONB.
	candidate.EffectiveScmHosts = nil
	saved, err := s.cfg.Store.PutSiteConfig(ctx, candidate)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "put site config: "+err.Error())
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
// actually needs — how many rows, which agents, which lanes, whose credential,
// and which rows are off.
//
// The start URL is never here. It is the one field of this block that names an
// organisation's identity provider, the audit log is read by more people than the
// providers page is, and no review question needs it: "which agents, on which
// lane, whose credential" is answered without it.
//
// The account/role PIN is here, and the difference is deliberate: an AWS account
// id and an IAM role name are the identity this deployment signs with, not the
// directory it authenticates against — and a review of a refused capture
// (harness.credential.refused) has no other way to learn what the pin was.
func agentProviderAuditData(block types.AgentProviders) map[string]any {
	ids, mechanisms, sources, disabled, pins := []string{}, []string{}, []string{}, []string{}, []string{}
	for _, row := range block.Agents {
		ids = append(ids, row.ID)
		if row.SSOAccountID != "" {
			pins = append(pins, row.SSOAccountID+"/"+row.SSORoleName)
		}
		if m := string(row.Mechanism); !slices.Contains(mechanisms, m) {
			mechanisms = append(mechanisms, m)
		}
		src := string(row.CredentialSource)
		if src == "" {
			src = string(types.CredentialSourceShared)
		}
		if !slices.Contains(sources, src) {
			sources = append(sources, src)
		}
		if row.Disabled {
			disabled = append(disabled, row.ID)
		}
	}
	slices.Sort(ids)
	slices.Sort(mechanisms)
	slices.Sort(sources)
	slices.Sort(disabled)
	slices.Sort(pins)
	return map[string]any{
		"agent_count": len(block.Agents), "ids": ids,
		"mechanisms": mechanisms, "credential_sources": sources, "disabled": disabled,
		// Unlike the start URL: an account id and a role name are not an
		// organisation's identity provider, and "which identity was this
		// deployment pinned to when that capture was refused" has no other
		// answer in the trail.
		"pins": pins,
	}
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
