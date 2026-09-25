// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The Azure DevOps CAPABILITY ARM of the injection resolve: what the proxy asks
// when a request needs access its run was not dispatched with.
//
//	GET /internal/injection/{grant}?capability=C&first_use=M[&approval=A]…
//
// answers 200 (C is held — the dispatch grant, or approved for this run, or
// the named `once` approval, which this resolve spends), 403 (C is above the
// administrator's ceiling, the run's first-use mode is always_deny, a person
// denied it, or deny_with_review raised a request and refused this attempt) or
// 423 {state:"capability_pending", approval_id} under wait_for_review, which
// the proxy holds on.
//
// THE REQUEST IS RAISED HERE, never by the sidecar: the row is kind tool_call
// (migration-free) and carries the grant it belongs to in grant_id, which the
// sandbox's own approval route can never set. That column — not a key inside
// the scope — is what makes the row decidable by the run's owner
// (authorizeMemberDecision). kind `credential` is deliberately NOT used:
// selectGrantApprovalForUpdate adopts the newest credential row for a grant as
// that grant's own mint approval.
//
// THE SCOPE IS CANONICAL. The pending-uniqueness index and findPendingDup hash
// the whole requested_scope, so it carries identity only — the raw request path
// rides the audit row, and `cmd` is composed from the identity fields.
//
// CONSENT. An approval widens what Wardyn lets through; it cannot widen what the
// person consented to. If the capability's scopes are outside the person's
// consent (Entra refuses the whole redemption — AADSTS65001 — or grants less
// than asked), the answer is a credential_reauth 423 the proxy chains onto, and
// the person's next sign-in resolves it (reconcileADOReauthOnRead).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// maxADOCapabilityHoldsPerRun is the control plane's per-run cap on Azure
// DevOps escalation and consent requests — beside maxReauthHolds, and for its
// reason: this path raises server-side, so maxApprovalsPerRun (the sidecar
// route's cap) does not bound it. The proxy's maxCapabilityHolds is the other
// half; it alone is not a limit, since a restarted sidecar counts from zero.
const maxADOCapabilityHoldsPerRun = 16

const (
	adoApprovalLane           = "azure_devops"
	adoConsentMechanism       = "entra_consent"
	adoCapabilityPendingState = "capability_pending"
	adoRefClassProtected      = "protected"
	// adoMaxAuditPath bounds the sandbox-chosen path on the audit row.
	adoMaxAuditPath = 512
)

// The refusal sentences. They reach the SANDBOX (the proxy relays them in Azure
// DevOps' own error shape), and the proxy reads only a short slice of a
// refusal body, so each stays well under that.
//
// DRAFT (M2 canon pending)
const (
	adoCapNotGrantableRefusal  = "Wardyn refused this Azure DevOps request: no run can be granted that access."
	adoCapAboveCeilingRefusal  = "Wardyn refused this Azure DevOps request: it needs %q, which is outside what an administrator allows for this organisation."
	adoCapAlwaysDenyRefusal    = "Wardyn refused this Azure DevOps request: it needs %q, and this run's policy refuses more access without asking."
	adoCapReviewRefusal        = "Wardyn refused this Azure DevOps request and asked a person to approve %q (approval %s). Retry once it is approved."
	adoCapDeniedRefusal        = "Wardyn refused this Azure DevOps request: a person denied the access it needs."
	adoCapDeniedForRunRefusal  = "Wardyn refused this Azure DevOps request: a person denied this access for the rest of the run (approval %s)."
	adoCapClosedRefusal        = "Wardyn refused this Azure DevOps request: its approval request has closed."
	adoCapMismatchRefusal      = "Wardyn refused this Azure DevOps request: the approval it named is not this request's."
	adoCapTooManyRefusal       = "Wardyn refused this Azure DevOps request: this run has asked for more access too many times."
	adoCapApprovalsUnreadable  = "could not read this run's approvals, so no further access can be decided"
	adoCapRaiseFailedBody      = "could not raise the approval request for more Azure DevOps access"
	adoCapUnspendableBody      = "this deployment cannot spend a once-only approval, so none is honoured"
	adoCapabilityRequestedNote = "a run asked for Azure DevOps access it was not dispatched with, and a person was asked to decide"
)

// adoFail is resolveADOInjection's own refusal writer: an audited secret.read
// failure and the body. Always returns true (handled).
type adoFail func(status int, reason, body string, extra map[string]any) bool

// adoCapabilityScope is the CANONICAL requested_scope of an escalation row.
type adoCapabilityScope struct {
	Lane       string    `json:"lane"`
	ProviderID string    `json:"provider_id"`
	Org        string    `json:"org"`
	GrantID    uuid.UUID `json:"grant_id"`
	Capability string    `json:"capability"`
	Repo       string    `json:"repo"`
	RefClass   string    `json:"ref_class"`
	// Tool and Cmd are what an older console's generic tool_call card reads.
	// Composed from the fields above, never from the request.
	Tool string `json:"tool"`
	Cmd  string `json:"cmd"`
}

// adoConsentScopeBody is the requested_scope of a consent row (kind
// credential_reauth). No credential_source key, so the AWS sign-in resolver
// (reauthResolvableBy) can never match it.
type adoConsentScopeBody struct {
	Lane       string   `json:"lane"`
	Mechanism  string   `json:"mechanism"`
	Owner      string   `json:"owner"`
	ProviderID string   `json:"provider_id"`
	Scopes     []string `json:"scopes"`
}

// adoEscalationScope reports whether ap is a control-plane-raised Azure DevOps
// escalation, and its scope. STRUCTURAL FIRST: grant_id is set only by the
// control plane (the sandbox route cannot), and the lane key is a sanity check
// behind it, never the discriminator.
func adoEscalationScope(ap types.ApprovalRequest) (adoCapabilityScope, bool) {
	if ap.Kind != types.ApprovalToolCall || ap.GrantID == nil || *ap.GrantID == uuid.Nil {
		return adoCapabilityScope{}, false
	}
	var sc adoCapabilityScope
	if json.Unmarshal(ap.RequestedScope, &sc) != nil || sc.Lane != adoApprovalLane || sc.GrantID != *ap.GrantID {
		return adoCapabilityScope{}, false
	}
	return sc, true
}

// adoConsentScope reports whether ap is an Azure DevOps consent request.
func adoConsentScope(ap types.ApprovalRequest) (adoConsentScopeBody, bool) {
	if ap.Kind != types.ApprovalCredentialReauth {
		return adoConsentScopeBody{}, false
	}
	var sc adoConsentScopeBody
	if json.Unmarshal(ap.RequestedScope, &sc) != nil || sc.Lane != adoApprovalLane || sc.Mechanism != adoConsentMechanism {
		return adoConsentScopeBody{}, false
	}
	return sc, true
}

// adoCapabilityGrant is what the arm decided when it lets the resolve go on.
type adoCapabilityGrant struct {
	capability adoscope.Capability
	// standing is the dispatch grant plus every capability approved for this
	// run, each still inside the LIVE ceiling.
	standing []adoscope.Capability
	// once is the approved `once` row this resolve spends, if that is why it
	// goes on.
	once *types.ApprovalRequest
}

// answerADOCapability decides a capability ask. ok=false means a response has
// been written.
func (s *Server) answerADOCapability(w http.ResponseWriter, r *http.Request, claims *identity.Claims,
	sn adoEntraScopeSnapshot, sc types.SiteConfig, grantID uuid.UUID, fail adoFail,
) (adoCapabilityGrant, bool) {
	q := r.URL.Query()
	c := adoscope.Capability(q.Get("capability"))
	if !c.Grantable() {
		return adoCapabilityGrant{}, !fail(http.StatusForbidden, "capability_not_grantable", adoCapNotGrantableRefusal,
			map[string]any{"capability": c})
	}
	// driftFrom has proved the row exists, is live and carries Entra.
	row, _ := gitProviderRowByID(sc, sn.ProviderRowID)
	ceiling := row.Entra.CapabilityCeiling
	rows, err := s.runApprovals(r.Context(), claims.RunID, "")
	if err != nil {
		return adoCapabilityGrant{}, !fail(http.StatusServiceUnavailable, "approvals_unreadable", adoCapApprovalsUnreadable, nil)
	}
	g := adoCapabilityGrant{capability: c, standing: adoStanding(sn, ceiling, rows)}
	if slices.Contains(g.standing, c) {
		return g, true
	}
	// NEVER ABOVE THE CEILING, whoever approved what: checked against the live
	// row on every ask, before any approval is honoured or raised.
	if !slices.Contains(ceiling, c) {
		return adoCapabilityGrant{}, !fail(http.StatusForbidden, "capability_above_ceiling",
			fmt.Sprintf(adoCapAboveCeilingRefusal, adoscope.Label(c)), map[string]any{"capability": c})
	}
	if raw := q.Get("approval"); raw != "" {
		ap, ok := adoNamedApproval(rows, raw, grantID, c)
		if !ok {
			return adoCapabilityGrant{}, !fail(http.StatusForbidden, "approval_mismatch", adoCapMismatchRefusal,
				map[string]any{"capability": c})
		}
		switch ap.State {
		case types.ApprovalPending:
			writeJSON(w, http.StatusLocked, reauthPendingResponse{State: adoCapabilityPendingState, ApprovalID: ap.ID})
			return adoCapabilityGrant{}, false
		case types.ApprovalDenied:
			return adoCapabilityGrant{}, !fail(http.StatusForbidden, "capability_denied", adoCapDeniedRefusal,
				map[string]any{"capability": c, "approval_id": ap.ID})
		case types.ApprovalApproved:
			if ap.DecisionScope == types.ScopeOnce && ap.MintedJTI == "" {
				g.once = &ap
				return g, true
			}
			// Spent: this is a new attempt, and it asks again below.
		default:
			return adoCapabilityGrant{}, !fail(http.StatusForbidden, "capability_closed", adoCapClosedRefusal,
				map[string]any{"capability": c, "approval_id": ap.ID})
		}
	}
	// A DENY STICKS for the rest of the run, as an egress deny does: the same
	// canonical request is refused naming that decision, and nothing is raised.
	want := adoScopeFor(sn, grantID, c, q)
	for _, ap := range rows {
		if got, ok := adoEscalationScope(ap); ok && got == want && ap.State == types.ApprovalDenied {
			return adoCapabilityGrant{}, !fail(http.StatusForbidden, "capability_denied",
				fmt.Sprintf(adoCapDeniedForRunRefusal, ap.ID), map[string]any{"capability": c, "approval_id": ap.ID})
		}
	}
	// The same request, already approved once and not yet spent — its hold ran
	// out before the answer, and this is the retry. Point it at that approval;
	// the proxy's re-resolve spends it.
	for _, ap := range rows {
		if got, ok := adoEscalationScope(ap); ok && got == want && ap.State == types.ApprovalApproved &&
			ap.DecisionScope == types.ScopeOnce && ap.MintedJTI == "" {
			writeJSON(w, http.StatusLocked, reauthPendingResponse{State: adoCapabilityPendingState, ApprovalID: ap.ID})
			return adoCapabilityGrant{}, false
		}
	}
	s.raiseADOCapability(w, r, claims, sn, grantID, c, rows, fail)
	return adoCapabilityGrant{}, false
}

// adoScopeFor is the canonical scope of the request q asks about.
func adoScopeFor(sn adoEntraScopeSnapshot, grantID uuid.UUID, c adoscope.Capability, q url.Values) adoCapabilityScope {
	sc := adoCapabilityScope{
		Lane: adoApprovalLane, ProviderID: sn.ProviderRowID, Org: sn.Organisation, GrantID: grantID,
		Capability: string(c), Repo: adoCanonicalRepo(q.Get("repo")), Tool: "Azure DevOps",
	}
	if q.Get("ref_class") == adoRefClassProtected {
		sc.RefClass = adoRefClassProtected
	}
	sc.Cmd = adoCapabilityCmd(sc, c)
	return sc
}

// adoStanding is the run's standing capability set: the dispatch grant, plus
// every capability a person approved "for this run" on this provider row and
// organisation — held to the live ceiling at each control-plane ask. The proxy
// caches a run-scoped widening for the run, so narrowing the row stops the
// next ask, not a capability that proxy has already widened to.
func adoStanding(sn adoEntraScopeSnapshot, ceiling []adoscope.Capability, rows []types.ApprovalRequest) []adoscope.Capability {
	out := slices.Clone(sn.Capabilities)
	for _, ap := range rows {
		sc, ok := adoEscalationScope(ap)
		if !ok || ap.State != types.ApprovalApproved || ap.DecisionScope != types.ScopeRun ||
			sc.ProviderID != sn.ProviderRowID || sc.Org != sn.Organisation {
			continue
		}
		c := adoscope.Capability(sc.Capability)
		if slices.Contains(ceiling, c) && !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out
}

// adoNamedApproval finds the escalation the proxy's re-resolve names, held to
// this run, this grant and this capability.
func adoNamedApproval(rows []types.ApprovalRequest, raw string, grantID uuid.UUID, c adoscope.Capability) (types.ApprovalRequest, bool) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return types.ApprovalRequest{}, false
	}
	for _, ap := range rows {
		if ap.ID != id {
			continue
		}
		sc, ok := adoEscalationScope(ap)
		return ap, ok && sc.GrantID == grantID && sc.Capability == string(c)
	}
	return types.ApprovalRequest{}, false
}

// adoRequestCount is how many Azure DevOps escalation and consent requests the
// run has raised, in any state — the server-side cap's count.
func adoRequestCount(rows []types.ApprovalRequest) int {
	n := 0
	for _, ap := range rows {
		_, esc := adoEscalationScope(ap)
		_, con := adoConsentScope(ap)
		if esc || con {
			n++
		}
	}
	return n
}

// raiseADOCapability applies the run's first-use mode and the per-run cap, then
// raises (or finds) the escalation request and answers 423 or 403.
func (s *Server) raiseADOCapability(w http.ResponseWriter, r *http.Request, claims *identity.Claims,
	sn adoEntraScopeSnapshot, grantID uuid.UUID, c adoscope.Capability, rows []types.ApprovalRequest, fail adoFail,
) {
	ctx := r.Context()
	q := r.URL.Query()
	// The run's first-use mode, as its proxy holds it (the proxy is the
	// component that enforces the run's policy; the sandbox cannot reach this
	// route). Missing or unknown is always_deny.
	mode := types.FirstUseMode(q.Get("first_use")).Normalize()
	if mode == types.FirstUseAlwaysDeny {
		fail(http.StatusForbidden, "capability_always_deny", fmt.Sprintf(adoCapAlwaysDenyRefusal, adoscope.Label(c)),
			map[string]any{"capability": c})
		return
	}
	if adoRequestCount(rows) >= maxADOCapabilityHoldsPerRun {
		fail(http.StatusForbidden, "capability_holds_exhausted", adoCapTooManyRefusal, map[string]any{"capability": c})
		return
	}
	scope := adoScopeFor(sn, grantID, c, q)
	raw, _ := json.Marshal(scope)
	raisedID := uuid.New()
	created, err := s.cfg.Approvals.Request(ctx, types.ApprovalRequest{
		ID: raisedID, RunID: claims.RunID, GrantID: &grantID, Kind: types.ApprovalToolCall, RequestedScope: raw,
	})
	if err != nil {
		fail(http.StatusServiceUnavailable, "raise_failed", adoCapRaiseFailedBody, map[string]any{"capability": c})
		return
	}
	if created.ID == raisedID {
		path := q.Get("path")
		if len(path) > adoMaxAuditPath {
			path = path[:adoMaxAuditPath]
		}
		s.recordAudit(ctx, s.auditEvent(&claims.RunID, types.ActorSystem, "wardynd",
			"credential.capability.request", created.ID.String(), "success",
			mustJSON(map[string]any{
				"approval_id": created.ID, "capability": c, "organisation": sn.Organisation,
				"provider_row": sn.ProviderRowID, "owner": sn.OwnerSubject, "repo": scope.Repo,
				"ref_class": scope.RefClass, "first_use": mode, "method": q.Get("method"), "path": path,
				"detail": adoCapabilityRequestedNote,
			})))
	}
	if mode == types.FirstUseWaitForReview {
		writeJSON(w, http.StatusLocked, reauthPendingResponse{State: adoCapabilityPendingState, ApprovalID: created.ID})
		return
	}
	fail(http.StatusForbidden, "capability_review", fmt.Sprintf(adoCapReviewRefusal, adoscope.Label(c), created.ID),
		map[string]any{"capability": c, "approval_id": created.ID})
}

// adoCanonicalRepo holds the proxy-reported repository to a short, printable
// identifier; anything else is dropped rather than stored.
func adoCanonicalRepo(raw string) string {
	repo := strings.ToLower(strings.TrimSpace(raw))
	if len(repo) > 128 || strings.ContainsFunc(repo, func(r rune) bool {
		return r == '/' || r == '?' || r == '#' || !unicode.IsPrint(r)
	}) {
		return ""
	}
	return repo
}

// adoCapabilityCmd is the card's one-line description, from identity only.
func adoCapabilityCmd(sc adoCapabilityScope, c adoscope.Capability) string {
	where := sc.Org
	if sc.Repo != "" {
		where += " / " + sc.Repo
	}
	cmd := fmt.Sprintf("%s (%s) in %s", adoscope.Label(c), c, where)
	if sc.RefClass == adoRefClassProtected {
		cmd += ", on a protected branch"
	}
	return cmd
}

// raiseADOConsent answers a capability the person has not consented to: a
// credential_reauth request their next sign-in resolves, and a 423 the proxy's
// hold chains onto.
func (s *Server) raiseADOConsent(w http.ResponseWriter, r *http.Request, claims *identity.Claims,
	sn adoEntraScopeSnapshot, need []string, fail adoFail,
) bool {
	ctx := r.Context()
	rows, err := s.runApprovals(ctx, claims.RunID, "")
	if err != nil {
		return fail(http.StatusServiceUnavailable, "approvals_unreadable", adoCapApprovalsUnreadable, nil)
	}
	if adoRequestCount(rows) >= maxADOCapabilityHoldsPerRun {
		return fail(http.StatusForbidden, "capability_holds_exhausted", adoCapTooManyRefusal, nil)
	}
	raw, _ := json.Marshal(adoConsentScopeBody{
		Lane: adoApprovalLane, Mechanism: adoConsentMechanism, Owner: sn.OwnerSubject,
		ProviderID: sn.ProviderRowID, Scopes: need,
	})
	raisedID := uuid.New()
	created, err := s.cfg.Approvals.Request(ctx, types.ApprovalRequest{
		ID: raisedID, RunID: claims.RunID, Kind: types.ApprovalCredentialReauth, RequestedScope: raw,
	})
	if err != nil {
		return fail(http.StatusServiceUnavailable, "raise_failed", adoCapRaiseFailedBody, nil)
	}
	if created.ID == raisedID {
		s.recordAudit(ctx, s.auditEvent(&claims.RunID, types.ActorSystem, "wardynd",
			"credential.reauth.request", created.ID.String(), "success",
			mustJSON(map[string]any{
				"approval_id": created.ID, "owner": sn.OwnerSubject, "provider": adoApprovalLane,
				"reason": string(ADOEntraFailureConsentRequired), "detail": adoResolveConsentRequired,
			})))
	}
	writeJSON(w, http.StatusLocked, reauthPendingResponse{State: reauthPendingState, ApprovalID: created.ID})
	return true
}

// reconcileADOReauthOnRead resolves a PENDING Azure DevOps consent or sign-in
// request once the person has signed in again, after it was raised, with every
// scope it needs — on the read the proxy's hold is already making, and from
// the capture itself (resolvePendingADOReauth). The capture is the resolution;
// this writes it down, through the same one-transaction seam the AWS lane uses.
func (s *Server) reconcileADOReauthOnRead(ctx context.Context, ap types.ApprovalRequest) types.ApprovalRequest {
	sc, ok := adoReauthScope(ap)
	if !ok || ap.State != types.ApprovalPending || s.cfg.Approvals == nil {
		return ap
	}
	resolver, ok := s.cfg.Store.(reauthResolver)
	if !ok {
		return ap
	}
	// Generation: only a sign-in captured AFTER the raise answers it, and only
	// one no renewal has since found ended.
	blob, found, err := s.readADOEntraBlob(secretstore.WithPurpose(ctx, secretstore.PurposeStatus), sc.Owner, sc.ProviderID)
	if err != nil || !found || !blob.CapturedAt.After(ap.RequestedAt) || blob.signInEnded() {
		return ap
	}
	if !subsetOf(sc.Scopes, blob.Scopes) {
		return ap
	}
	ev := s.auditEvent(&ap.RunID, types.ActorHuman, sc.Owner, "credential.reauth.resolve", ap.ID.String(), "success",
		mustJSON(map[string]any{
			"approval_id": ap.ID, "owner": sc.Owner, "resolved_by": sc.Owner, "provider": adoApprovalLane,
		}))
	if _, err := resolver.ResolveReauthApproval(ctx, ap.ID, types.ApprovalDecision{
		State: types.ApprovalApproved, DecidedBy: sc.Owner, Reason: "signed in again",
	}, ev); err != nil {
		return ap
	}
	s.metrics.credentialReauthResolved(s.cfg.Now().Sub(ap.RequestedAt))
	if fresh, gerr := s.cfg.Approvals.Get(ctx, ap.ID); gerr == nil {
		return fresh
	}
	return ap
}

// approvalOnceSpender is the store seam that spends a `once` approval exactly
// once (store.PG.SpendApprovalOnce). Optional, like reauthResolver; a store
// without it honours no `once` approval at all.
type approvalOnceSpender interface {
	SpendApprovalOnce(ctx context.Context, id uuid.UUID, jti string) (bool, error)
}

// adoEscalationWithinCeiling reports whether the escalation's capability is
// inside its provider row's LIVE ceiling. A read that fails is outside.
func (s *Server) adoEscalationWithinCeiling(ctx context.Context, sc adoCapabilityScope) bool {
	if s.cfg.Store == nil {
		return false
	}
	site, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return false
	}
	row, found := gitProviderRowByID(site, sc.ProviderID)
	return found && !row.Disabled && row.Entra != nil &&
		slices.Contains(row.Entra.CapabilityCeiling, adoscope.Capability(sc.Capability))
}

// adoDecisionRule is decide()'s rule 4 for an Azure DevOps escalation — the
// one tool_call a scope means something on: `once` lets the held request
// through and nothing after it, `run` adds the capability to the run's standing
// set. No scope is `once`, the narrower reading of a bodyless click. until and
// always are refused: the lane has no clock and no workspace to write back to.
// And never above the ceiling: an approval outside what the administrator
// allows today would grant nothing (the resolve re-checks it), so it is refused
// rather than recorded as if it had.
//
// isADO=false leaves scope untouched for the other kinds' rules; ok=false means
// a response has been written.
func (s *Server) adoDecisionRule(w http.ResponseWriter, r *http.Request, ap types.ApprovalRequest,
	scope types.ApprovalScope, approve bool,
) (types.ApprovalScope, bool, bool) {
	sc, isADO := adoEscalationScope(ap)
	if !isADO {
		return scope, false, true
	}
	switch scope {
	case "":
		scope = types.ScopeOnce
	case types.ScopeOnce, types.ScopeRun:
	default:
		writeError(w, http.StatusBadRequest, `an Azure DevOps access request is decided "once" or for this "run"`)
		return scope, true, false
	}
	if approve && !s.adoEscalationWithinCeiling(r.Context(), sc) {
		writeError(w, http.StatusForbidden, "this Azure DevOps access is outside what an administrator allows for the organisation")
		return scope, true, false
	}
	return scope, true, true
}

// adoConsentRefusalTTL is how long a consent_required answer is reused before
// the authority is asked again. Without it, a stored sign-in listing a scope
// Entra no longer grants would cost a fresh redemption — a refresh-token
// rotation — on every refused request.
const adoConsentRefusalTTL = 60 * time.Second

func adoConsentKey(cfg ADOEntraConfig, owner string, need []string) string {
	return strings.Join([]string{owner, cfg.RowID, cfg.TenantID, cfg.ClientID, strings.Join(need, " ")}, "\x00")
}

// adoConsentRefused records a consent_required answer for (owner, row, need).
func (s *Server) adoConsentRefused(cfg ADOEntraConfig, owner string, need []string) {
	c := &s.adoEntraTokens
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.refused == nil {
		c.refused = map[string]time.Time{}
	}
	c.refused[adoConsentKey(cfg, owner, need)] = s.cfg.Now()
}

// adoConsentRefusedRecently reports whether the same consent was refused within
// adoConsentRefusalTTL AND the person has not signed in since — a sign-in is
// the thing that can change the answer, so it always earns a fresh redemption.
func (s *Server) adoConsentRefusedRecently(ctx context.Context, cfg ADOEntraConfig, owner string, need []string) bool {
	c := &s.adoEntraTokens
	c.mu.Lock()
	at, ok := c.refused[adoConsentKey(cfg, owner, need)]
	c.mu.Unlock()
	if !ok || s.cfg.Now().Sub(at) >= adoConsentRefusalTTL {
		return false
	}
	blob, found, err := s.readADOEntraBlob(ctx, owner, cfg.RowID)
	return err == nil && found && !blob.CapturedAt.After(at)
}

// settleADOCapability finishes a capability resolve once a token is in hand:
// the consent the capability needs, then the `once` spend. handled=true means a
// response has been written and the resolve must stop.
func (s *Server) settleADOCapability(w http.ResponseWriter, r *http.Request, claims *identity.Claims,
	snapshot adoEntraScopeSnapshot, cfg ADOEntraConfig, grantID uuid.UUID, jti string,
	capAsk adoCapabilityGrant, need []string, access ADOEntraAccess, fail adoFail,
) bool {
	ctx := r.Context()
	// The authority's GRANTED set is the person's whole consent for the
	// resource (measured), so a capability whose scope is missing from it is
	// one they have not consented to — whoever approved it here.
	if !subsetOf(need, access.Scopes) {
		s.adoConsentRefused(cfg, snapshot.OwnerSubject, need)
		return s.raiseADOConsent(w, r, claims, snapshot, need, fail)
	}
	if capAsk.once == nil {
		return false
	}
	spender, ok := s.cfg.Store.(approvalOnceSpender)
	if !ok {
		return fail(http.StatusServiceUnavailable, "once_unspendable", adoCapUnspendableBody, nil)
	}
	spent, serr := spender.SpendApprovalOnce(ctx, capAsk.once.ID, jti)
	if serr != nil {
		return fail(http.StatusServiceUnavailable, "once_unspendable", adoCapUnspendableBody, nil)
	}
	if spent {
		return false
	}
	// Another request spent it first: this one is a new attempt.
	rows, rerr := s.runApprovals(ctx, claims.RunID, "")
	if rerr != nil {
		return fail(http.StatusServiceUnavailable, "approvals_unreadable", adoCapApprovalsUnreadable, nil)
	}
	s.raiseADOCapability(w, r, claims, snapshot, grantID, capAsk.capability, rows, fail)
	return true
}
