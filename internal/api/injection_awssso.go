// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The captured-AWS-SSO injection resolve (PHASE B) — the control-plane half of
// the lane authorBedrockSSOInjection authors at dispatch.
//
// It is a SEPARATE FILE from handleInternalInjection deliberately: that handler
// is already near the funlen cap, and this arm is not a variation on the
// subscription sentinel — it re-derives an identity, compares it against a
// snapshot, can RAISE a human-visible request and can answer 423, none of which
// the generic sink does.
//
// The one rule this file exists to enforce (I3/I5): a recovery event may refresh
// credential MATERIAL; it must never re-authorize the operation against a
// different principal, credential source, mechanism, account, role, region or
// host. Everything below is that sentence, spelled out.

// DRAFT (M2 canon pending)

const (
	// credentialReauthRaisedSentence is what the person is told when a run's
	// captured AWS SSO session has lapsed mid-run and the request is being held.
	// It names the mechanism, says what is happening to the run, and closes the
	// substitution question before it is asked.
	credentialReauthRaisedSentence = "this run's model access is configured as Amazon Bedrock " +
		"(captured AWS SSO session), and that session can no longer be renewed — the run is HELD while " +
		"you sign in again. It resumes by itself when the sign-in lands. Wardyn does not substitute a " +
		"different model provider."
	// credentialReauthHostPinRefusal is the host pin (I4), the exact shape of
	// the subscription sentinel's own pin one file over: a grant could name any
	// egress-allowlisted host, and this sink hands back a LIVE session token.
	credentialReauthHostPinRefusal = "the AWS SSO access token may only be injected to this " +
		"credential's own sso portal host"
	// credentialReauthScopeChangedRefusal is the I3 drift refusal. It is
	// deliberately vague about WHICH field drifted: the caller is a sidecar, the
	// reader is an audit row, and the field names are identity.
	credentialReauthScopeChangedRefusal = "this run's model credential is no longer the one it was " +
		"dispatched with — the roster changed while the run was working, and Wardyn will not resolve a " +
		"different principal's credential for a run already in flight. Relaunch the run."
	// credentialReauthTooManyRefusal bounds the raise path. A run that has
	// already asked its owner to sign in eight times is not going to be fixed by
	// a ninth row.
	credentialReauthTooManyRefusal = "this run has already asked for an AWS sign-in too many times; " +
		"nothing was substituted and no further sign-in will be requested for it"
	// credentialReauthClosedBody is the 403 a resolve gets when this run's
	// sign-in request is no longer open. %s is the row's terminal state, which
	// is a closed Wardyn enum and never caller input.
	credentialReauthClosedBody = "this run's AWS sign-in request is closed: %s"
	// credentialReauthRunUnreadableBody / credentialReauthStoreErrorBody /
	// credentialReauthApprovalsUnreadableBody are the three fail-closed 503s.
	// Machine-facing (the caller is a sidecar), but named rather than inlined so
	// every sentence this lane puts on a wire has one definition and a canon row.
	credentialReauthRunUnreadableBody = "could not read this run"
	// DRAFT (M2 canon pending)
	credentialReauthStoreErrorBody = "could not read this run's AWS SSO credential"
	// DRAFT (M2 canon pending)
	credentialReauthApprovalsUnreadableBody = "could not read this run's approvals"
	// DRAFT (M2 canon pending)
	credentialReauthRaiseFailedBody = "could not raise the AWS sign-in request: "
	// credentialReauthNotDecidableBody is the 409 Server.decide answers for this
	// kind, on every tier.
	credentialReauthNotDecidableBody = "an AWS sign-in request is resolved by signing in, not by a " +
		"decision: approving or denying it would change nothing, because the next credential resolve " +
		"raises the request again"
)

// maxReauthHolds bounds how many re-auth WORKFLOWS one run may open — the
// control-plane half of the sidecar's own counter of the same name. Workflows,
// not requests: N concurrent GetRoleCredentials for one run share ONE row (the
// injector's per-host single flight plus RequestApproval's dedup), so this
// counts LIFECYCLES — lapse, ask, sign in, lapse again.
//
// It exists because the internal raise route's maxApprovalsPerRun guards
// handleInternalRequestApproval and nothing else; this path raises server-side
// and would otherwise be bounded by nothing but the run's own lifetime.
const maxReauthHolds = 8

// credentialReauthClosedRefusal composes the closed-request refusal for a
// terminal row.
func credentialReauthClosedRefusal(state types.ApprovalState) string {
	return fmt.Sprintf(credentialReauthClosedBody, state)
}

// reauthPendingResponse is the 423 body. state is spelled for a machine; the
// sidecar reads approval_id and polls THAT row (never this URL again — each
// resolve re-mints and writes a credential.mint audit row, so a poll here would
// be 300 rows per ten-minute hold on a hash-chained log).
type reauthPendingResponse struct {
	State      string    `json:"state"`
	ApprovalID uuid.UUID `json:"approval_id"`
}

const reauthPendingState = "reauth_pending"

// runApprovalLister is the optional DB-paged by-run read the raise path prefers,
// the approvalPageLister seam one file over: a test double that embeds the
// interface keeps compiling, and a real deployment does not scan every
// approval in the fleet to count one run's rows.
type runApprovalLister interface {
	ListApprovalsPageByRun(ctx context.Context, runID uuid.UUID, stateFilter types.ApprovalState, p store.Page) ([]types.ApprovalRequest, error)
}

// runApprovals reads one run's approvals in a state (empty = all), preferring
// the paged by-run read and falling back to the fleet-wide list the dedup scan
// in internal/approval already uses.
func (s *Server) runApprovals(ctx context.Context, runID uuid.UUID, state types.ApprovalState) ([]types.ApprovalRequest, error) {
	if pager, ok := s.cfg.Store.(runApprovalLister); ok {
		return pager.ListApprovalsPageByRun(ctx, runID, state, store.Page{})
	}
	all, err := s.cfg.Approvals.List(ctx, state)
	if err != nil {
		return nil, err
	}
	out := make([]types.ApprovalRequest, 0, len(all))
	for _, a := range all {
		if a.RunID == runID {
			out = append(out, a)
		}
	}
	return out, nil
}

// resolveAWSSSOInjection is the captured-AWS-SSO arm of handleInternalInjection.
// handled=false means this grant is not ours and the generic path must run;
// handled=true means a response has been written.
//
// The order below is the order the checks MUST run in, and each line is a
// refusal somebody could otherwise walk through:
//
//  1. the grant's own dispatch-time SNAPSHOT is read from the grant, not from
//     the minted rule — mintAPIKey drops it, and an injection rule has no
//     business carrying identity;
//  2. the scope is RE-DERIVED from the live roster and required equal (I3);
//  3. on the per_user lane the snapshot's owner must be the run token's own
//     subject (I2), so a policy-authored grant cannot name another owner;
//  4. the blob is read through that scope (never the operator's row for a
//     per-user principal), renewed through the same single-flight dispatch uses;
//  5. the host is pinned to the credential's OWN portal host (I4);
//  6. live -> 200 + per-run mask + secret.read success;
//     dead -> a visible request and 423, or a terminal 403 if one was already
//     refused, cancelled or aged out.
func (s *Server) resolveAWSSSOInjection(w http.ResponseWriter, r *http.Request,
	claims *identity.Claims, minted broker.Minted, grantID uuid.UUID,
) bool {
	if minted.Injection == nil || minted.Injection.SecretName != types.AWSSSOAccessTokenSecret {
		return false
	}
	ctx := r.Context()
	fail := func(status int, reason, body string, extra map[string]any) bool {
		data := map[string]any{"reason": reason, "grant_id": grantID}
		for k, v := range extra {
			data[k] = v
		}
		s.recordAudit(ctx, s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
			"secret.read", types.AWSSSOAccessTokenSecret, "failure", mustJSON(data)))
		writeError(w, status, body)
		return true
	}

	snapshot, ok := s.awsSSOGrantSnapshot(ctx, claims.RunID, grantID)
	if !ok {
		// The grant exists (the broker just minted it) but its scope carries no
		// snapshot: a 0.7.5-authored grant re-resolved by a 0.7.6 daemon, or a
		// hand-authored one. Fail closed — the snapshot IS the authorization.
		return fail(http.StatusForbidden, "missing_scope_snapshot", credentialReauthScopeChangedRefusal, nil)
	}

	// (2) + (3): re-derive from the LIVE roster and require equality.
	run, rerr := s.cfg.Store.GetRun(ctx, claims.RunID)
	if rerr != nil {
		return fail(http.StatusServiceUnavailable, "run_unreadable", credentialReauthRunUnreadableBody, nil)
	}
	siteCfg, scErr := s.cfg.Store.GetSiteConfig(ctx)
	if scErr != nil {
		// NEVER resolve a scope from a read that failed: the zero SiteConfig is
		// indistinguishable from "a roster with no row", whose fallback is the
		// OPERATOR namespace — the exact substitution this arm refuses.
		return fail(http.StatusServiceUnavailable, "roster_unreadable", credentialReauthScopeChangedRefusal, nil)
	}
	scope := awsSSOScopeFor(siteCfg, run.Agent, claims.Sub)
	if drift := snapshot.driftFrom(siteCfg, run.Agent, scope, claims.Sub); drift != "" {
		return fail(http.StatusForbidden, "scope_changed", credentialReauthScopeChangedRefusal,
			map[string]any{"drift": drift, "owner": snapshot.OwnerSubject})
	}

	// (4) read + renew through that scope — the SAME single-flight, skew, spent
	// map and harness.credential.refresh audit dispatch uses.
	blob, found, berr := s.readAWSSSOBlob(ctx, scope)
	if berr != nil {
		return fail(http.StatusServiceUnavailable, "store_error", credentialReauthStoreErrorBody, nil)
	}
	reason := awsSSOReauthReasonNotFound
	if found {
		var refreshFailure string
		blob, refreshFailure = s.refreshAWSSSOBlob(ctx, scope, blob)
		switch {
		// The COMPOSED refusal, not the bare constant: the run-credential-door
		// lane turned awsSSORefreshSpentSentence into a format string whose %s
		// is the audience's own remedy (awsSSORefreshSpentRefusal), and
		// refreshAWSSSOBlob returns the composed form. Compared against the
		// constant this arm is silently always false, and every spent session
		// is audited "unavailable" — a reason that names the wrong fix.
		case refreshFailure == awsSSORefreshSpentRefusal(scope.perUser):
			reason = awsSSOReauthReasonSpent
		case refreshFailure != "":
			reason = awsSSOReauthReasonUnavailable
		case blob.expired(s.cfg.Now()):
			reason = awsSSOReauthReasonUnavailable
		default:
			reason = ""
		}
	}

	// (5) the host pin, checked for the LIVE and the DEAD path alike: a grant
	// pointing somewhere else must be refused, never held.
	if !hostEqual(minted.Injection.Host, ssoPortalHost(snapshot.Region, s.cfg.AWSSSOEndpointOverride)) {
		return fail(http.StatusForbidden, "sso-host-not-portal", credentialReauthHostPinRefusal,
			map[string]any{"host": minted.Injection.Host})
	}

	if reason != "" {
		return s.holdOrRefuseCredentialReauth(w, r, claims, snapshot, reason)
	}

	// (6) LIVE. Per-run masking is ADDITIVE to the global registration
	// refreshAWSSSOBlob already makes on every successful rotation: the global
	// set is what masks this one shared credential from every OTHER run's
	// streams, and the per-run set is what the terminal-run sweeper can evict.
	// Neither replaces the other.
	value := formatInjectionValue(minted.Injection.Format, []byte(blob.AccessToken))
	if s.cfg.MaskRegistry != nil {
		s.cfg.MaskRegistry.Add(claims.RunID, []byte(blob.AccessToken))
		if value != blob.AccessToken {
			s.cfg.MaskRegistry.Add(claims.RunID, []byte(value))
		}
	}
	s.recordAudit(ctx, s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
		"secret.read", types.AWSSSOAccessTokenSecret, "success",
		mustJSON(map[string]any{
			"purpose": "proxy-injection-sso", "grant_id": grantID, "jti": minted.JTI,
			"owner": snapshot.OwnerSubject, "credential_source": snapshot.CredentialSource,
		})))
	writeJSON(w, http.StatusOK, injectionResponse{
		Host:      minted.Injection.Host,
		Header:    minted.Injection.Header,
		Value:     value,
		JTI:       minted.JTI,
		ExpiresAt: blob.ExpiresAt.UnixMilli(),
	})
	return true
}

// The reasons a re-auth is raised for. A closed vocabulary because it rides an
// audit row and a metric label, and because it is deliberately NOT part of the
// approval's requested_scope: the scope is the DEDUP key, so a spent->unavailable
// flip between two resolves would otherwise raise a second row for one lapse.
const (
	awsSSOReauthReasonSpent       = "spent"
	awsSSOReauthReasonUnavailable = "unavailable"
	awsSSOReauthReasonNotFound    = "not_found"
)

// holdOrRefuseCredentialReauth answers the DEAD-credential path: 423 while a
// request is open, 403 once one has been refused, cancelled or aged out.
//
// The row state gates the answer, and that is what stops the sidecar holding on
// a dead request: a CANCELLED row (the run ended) or an EXPIRED one (the 24 h
// sweeper) answers TERMINAL at once, so the hold ends within one poll instead of
// running its whole budget against a question nobody can answer.
func (s *Server) holdOrRefuseCredentialReauth(w http.ResponseWriter, r *http.Request,
	claims *identity.Claims, snapshot awsSSOScopeSnapshot, reason string,
) bool {
	ctx := r.Context()
	rows, lerr := s.runApprovals(ctx, claims.RunID, "")
	if lerr != nil {
		// Fail closed: an unbounded raise path is the thing being bounded, so
		// "we could not tell how many this run has" must not read as "raise
		// another one".
		writeError(w, http.StatusServiceUnavailable, credentialReauthApprovalsUnreadableBody)
		return true
	}
	workflows := 0
	var terminal *types.ApprovalRequest
	for i := range rows {
		if rows[i].Kind != types.ApprovalCredentialReauth {
			continue
		}
		// An Azure DevOps consent row is credential_reauth too, but it has its
		// own cap (maxADOCapabilityHoldsPerRun) and never spends this budget.
		if _, consent := adoConsentScope(rows[i]); consent {
			continue
		}
		workflows++
		switch rows[i].State {
		case types.ApprovalPending:
			// Already open: the sidecar is (or will be) holding on this id.
			writeJSON(w, http.StatusLocked, reauthPendingResponse{State: reauthPendingState, ApprovalID: rows[i].ID})
			return true
		case types.ApprovalCancelled, types.ApprovalExpired, types.ApprovalDenied:
			terminal = &rows[i]
		case types.ApprovalApproved:
			// A sign-in landed and the credential has lapsed AGAIN. That is a new
			// lapse, not the old one: it may raise a fresh row (below), bounded by
			// the workflow cap.
		}
	}
	if terminal != nil {
		// No metric here. This is a RESOLVE meeting a row that
		// was already terminal, not the transition that made it terminal: with
		// the measured ~30 s SDK cadence one cancelled row would score dozens of
		// "outcomes". expired and cancelled are counted where the state changes
		// — the sweeper and cancelRunApprovals.
		writeError(w, http.StatusForbidden, credentialReauthClosedRefusal(terminal.State))
		return true
	}
	if workflows >= maxReauthHolds {
		writeError(w, http.StatusForbidden, credentialReauthTooManyRefusal)
		return true
	}

	// The scope is the DEDUP KEY, so it carries identity and nothing else: the
	// reason lives on the audit row, not here.
	reqScope, _ := json.Marshal(map[string]string{
		"mechanism":         snapshot.Mechanism,
		"credential_source": snapshot.CredentialSource,
		"owner":             snapshot.OwnerSubject,
	})
	// The id is minted HERE so this caller can tell whether it RAISED the request
	// or merely found one. RequestApproval's dedup — the pre-insert
	// scan and the partial unique index's loser alike — answers with the WINNER'S
	// row, and it answers silently by design; a caller that cannot tell the two
	// apart audits `credential.reauth.requested` and counts outcome=requested for
	// a request somebody else raised. With N concurrent resolvers for one lapse
	// that is one row per resolver against a hash-chained log, all naming the same
	// approval id, and a `requested` count that no longer means "requests raised".
	//
	// RequestApproval honours a supplied ID and only mints one when it is nil.
	raisedID := uuid.New()
	created, aerr := s.cfg.Approvals.Request(ctx, types.ApprovalRequest{
		ID: raisedID, RunID: claims.RunID, Kind: types.ApprovalCredentialReauth, RequestedScope: reqScope,
	})
	if aerr != nil {
		writeError(w, http.StatusServiceUnavailable, credentialReauthRaiseFailedBody+aerr.Error())
		return true
	}
	if created.ID != raisedID {
		// We lost the race. The row is real, PENDING and ours to wait on — the
		// 423 below is unchanged, and the hold joins the same workflow by id —
		// but the trail and the counter belong to whoever raised it.
		writeJSON(w, http.StatusLocked, reauthPendingResponse{State: reauthPendingState, ApprovalID: created.ID})
		return true
	}
	// Audited at the raise, with the reason the scope deliberately omits.
	// RequestApproval itself emits nothing (its dedup is silent by design), so
	// without this row "whose credential, held how long, resolved by whom" is
	// unanswerable from the trail.
	s.recordAudit(ctx, s.auditEvent(&claims.RunID, types.ActorSystem, "wardynd",
		"credential.reauth.requested", created.ID.String(), "success",
		mustJSON(map[string]any{
			"approval_id": created.ID, "owner": snapshot.OwnerSubject,
			"credential_source": snapshot.CredentialSource, "provider": awsSSOProvider,
			"reason": reason, "detail": credentialReauthRaisedSentence,
		})))
	s.metrics.credentialReauthRecorded(credentialReauthOutcomeRequested)
	writeJSON(w, http.StatusLocked, reauthPendingResponse{State: reauthPendingState, ApprovalID: created.ID})
	return true
}

// awsSSOGrantSnapshot reads the IMMUTABLE dispatch-time credential scope off
// the grant itself (grantSnapshot).
func (s *Server) awsSSOGrantSnapshot(ctx context.Context, runID, grantID uuid.UUID) (awsSSOScopeSnapshot, bool) {
	var sn awsSSOScopeSnapshot
	if !s.grantSnapshot(ctx, runID, grantID, &sn) || !sn.authored() {
		return awsSSOScopeSnapshot{}, false
	}
	return sn, true
}

// grantSnapshot decodes the dispatch-time "snapshot" a grant's scope carries
// into dst, false when the grant or its snapshot is absent. It looks the grant
// up through the RUN's own grant list, which re-proves run binding (I1)
// independently of the broker's own check.
func (s *Server) grantSnapshot(ctx context.Context, runID, grantID uuid.UUID, dst any) bool {
	if s.cfg.Store == nil {
		return false
	}
	grants, err := s.cfg.Store.ListGrantsByRun(ctx, runID)
	if err != nil {
		return false
	}
	for _, g := range grants {
		if g.ID != grantID {
			continue
		}
		var sc struct {
			Snapshot json.RawMessage `json:"snapshot"`
		}
		return json.Unmarshal(g.Spec.Scope, &sc) == nil && len(sc.Snapshot) > 0 &&
			json.Unmarshal(sc.Snapshot, dst) == nil
	}
	return false
}

// authored reports whether a snapshot is present at all. OwnerSubject is
// deliberately NOT part of the test: the shared lane's owner is legitimately the
// empty string, so reading emptiness there as "absent" would refuse every
// shared-lane run.
func (sn awsSSOScopeSnapshot) authored() bool {
	return sn.Mechanism != "" && sn.CredentialSource != "" && sn.Region != ""
}

// driftFrom compares the snapshot against the LIVE roster, returning the name
// of the first field that moved ("" = equal).
//
// It compares three different KINDS of fact, and each matters for its own
// reason. The roster scope (owner, credential source) is the substitution hole
// itself. The mechanism is the promise enforceConfiguredLLMMechanism makes at
// dispatch, restated here because a resolve is a second dispatch in every way
// that matters. The account/role PINS are admin-asserted identity: a row
// re-pinned mid-run must not have a held run silently resolve against the new
// pair.
func (sn awsSSOScopeSnapshot) driftFrom(sc types.SiteConfig, agentID string, scope awsSSOScope, subject string) string {
	if sn.OwnerSubject != scope.owner {
		return "owner_subject"
	}
	if sn.CredentialSource != awsSSOCredentialSourceLabel(scope) {
		return "credential_source"
	}
	// I2: on the per-user lane the credential's owner is the run's own subject,
	// which the run token — not the roster, and not the grant — is authority for.
	// A policy-authored grant naming another owner cannot pass this.
	if scope.perUser && sn.OwnerSubject != subject {
		return "owner_not_caller"
	}
	// Legacy open mode has no roster to drift from. When no roster
	// governs this deployment at all, the three roster-derived arms below have
	// nothing to compare against, and the two above them ARE the whole equality:
	// awsSSOScopeFor answers the shared scope for that shape, so
	// owner=="" + credential_source=="shared" is exactly what dispatch authored.
	//
	// Reading a MISSING roster as a WITHDRAWN row would refuse every no-roster
	// deployment: the refusal's sentence ("the roster changed") would be false
	// for it — nothing changed, because nothing was ever there.
	//
	// agentProvidersConfigured is the ONE place legacy open mode is decided
	// (agent_providers.go), so this asks it rather than inventing a second rule.
	if !agentProvidersConfigured(sc) {
		return ""
	}
	row, declared := agentProviderFor(sc, agentID)
	if !declared || row.Disabled {
		return "row_withdrawn"
	}
	if sn.Mechanism != string(row.Mechanism) {
		return "mechanism"
	}
	if row.SSOAccountID != "" && row.SSOAccountID != sn.SSOAccountID {
		return "sso_account_id"
	}
	if row.SSORoleName != "" && row.SSORoleName != sn.SSORoleName {
		return "sso_role_name"
	}
	return ""
}

// the resolution side: a sign-in answers the request

// resolvePendingReauth moves every PENDING credential_reauth this capture
// satisfies to APPROVED. Best-effort and never fatal to the capture: the
// credential is already stored, and a resolution that does not land is repaired
// by the reconcile-on-read below rather than by asking the person to sign in
// twice.
//
// I6 (generation): a capture resolves a request only if its LOGIN RUN was
// CREATED AFTER the request was raised. An older sign-in cannot answer a newer
// question — without this, a login sandbox that has been sitting idle since
// before the lapse would resolve a hold with the very session that lapsed.
//
// I2: only rows whose scope owner is this capture's own owner.
//
// ORDERING (I7): the caller runs this AFTER its own harness.credential.captured
// emit, never before. The chain is captured -> resolved -> retry, and a resolve
// that preceded its own capture row would be a credential-bearing retry with no
// auditable predecessor.
func (s *Server) resolvePendingReauth(ctx context.Context, scope awsSSOScope, capturedBy string, loginRun types.AgentRun) {
	// A deployment with no approval FSM wired (and every test double that does
	// not need one) has nothing to resolve. Nil-safe for the same reason the
	// mask-registry calls around it are: this is a best-effort courtesy on a
	// path whose real work is already done.
	if s.cfg.Approvals == nil {
		return
	}
	rows, err := s.cfg.Approvals.List(ctx, types.ApprovalPending)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: could not list pending AWS sign-in requests after a capture; the next poll will resolve them",
			slog.Any("err", err))
		return
	}
	for _, ap := range rows {
		if !reauthResolvableBy(ap, scope, loginRun) {
			continue
		}
		if err := s.resolveReauth(ctx, ap, capturedBy, loginRun.ID); err != nil {
			slog.WarnContext(ctx, "wardynd: could not resolve an AWS sign-in request after a capture; the next poll will",
				slog.String("approval_id", ap.ID.String()), slog.Any("err", err))
		}
	}
}

// reauthResolvableBy is the WHOLE admission test, in one predicate, so the
// eager path and the reconcile-on-read cannot drift apart.
func reauthResolvableBy(ap types.ApprovalRequest, scope awsSSOScope, loginRun types.AgentRun) bool {
	if ap.Kind != types.ApprovalCredentialReauth || ap.State != types.ApprovalPending {
		return false
	}
	var sc struct {
		Owner            string `json:"owner"`
		CredentialSource string `json:"credential_source"`
	}
	if json.Unmarshal(ap.RequestedScope, &sc) != nil {
		return false
	}
	// I2 — whose credential this capture became, decided at the login run's
	// LAUNCH (loginRunScope), never from the live roster.
	if sc.Owner != scope.owner || sc.CredentialSource != awsSSOCredentialSourceLabel(scope) {
		return false
	}
	// I6 — generation. Strictly after: a login run created in the same instant
	// as the raise is a coincidence nobody should resolve a credential on.
	return loginRun.CreatedAt.After(ap.RequestedAt)
}

// reauthResolver is the optional store seam that writes the APPROVED state and
// the credential.reauth.resolved row in ONE transaction — the broker's own mint
// precedent (state change and its durable record commit together, or neither).
//
// Optional, the approvalPageLister/store.Pager seam, so every test double that
// embeds store.Store keeps compiling; a store without it simply does not
// resolve eagerly, and the row stays PENDING until a poll finds it.
type reauthResolver interface {
	ResolveReauthApproval(ctx context.Context, id uuid.UUID, decision types.ApprovalDecision, ev types.AuditEvent) (types.ApprovalRequest, error)
}

// errReauthResolverUnavailable is what a store without the transactional seam
// returns. It is not an error the operator can act on — the reconcile-on-read
// repairs it — so it is logged, never surfaced.
var errReauthResolverUnavailable = errors.New("this store cannot resolve a re-auth approval transactionally")

// resolveReauth moves ONE row to APPROVED together with its audit row.
//
// NOT approval.Decide (O-6, third-party review): nobody clicked anything.
// Writing approval.decide here would put a decision in the trail that no human
// made, on a kind Server.decide refuses to decide at all. The row still moves to
// APPROVED so every existing list, count and sweeper reads it unchanged.
func (s *Server) resolveReauth(ctx context.Context, ap types.ApprovalRequest, resolvedBy string, captureRunID uuid.UUID) error {
	resolver, ok := s.cfg.Store.(reauthResolver)
	if !ok {
		return errReauthResolverUnavailable
	}
	var owner string
	var sc struct {
		Owner string `json:"owner"`
	}
	if json.Unmarshal(ap.RequestedScope, &sc) == nil {
		owner = sc.Owner
	}
	ev := s.auditEvent(&ap.RunID, types.ActorHuman, resolvedBy,
		"credential.reauth.resolved", ap.ID.String(), "success",
		mustJSON(map[string]any{
			"approval_id": ap.ID, "owner": owner, "resolved_by": resolvedBy,
			"capture_run_id": captureRunID, "provider": awsSSOProvider,
		}))
	if _, err := resolver.ResolveReauthApproval(ctx, ap.ID, types.ApprovalDecision{
		State: types.ApprovalApproved, DecidedBy: resolvedBy, Reason: "signed in again",
	}, ev); err != nil {
		return err
	}
	s.approvalClosed(ctx, ap.RunID)
	s.metrics.credentialReauthResolved(s.cfg.Now().Sub(ap.RequestedAt))
	return nil
}

// reconcileReauthOnRead repairs a resolution that did not land.
//
// The capture and the resolution are two writes. A daemon crash between them —
// or a failed list, or a store blip — would strand a VALID credential behind a
// PENDING row until the hold's budget ended, and replaying the upload cannot
// repair it (handleUploadSSOToken refuses an already-captured SourceRunID). So
// the resolution is ALSO derivable from state, idempotently, on the read the
// sidecar is already making: a PENDING row whose owner's stored blob was
// captured by a login run created after the raise IS a resolved row that has
// not been written down yet.
//
// Everything it re-checks is what the eager path checks (reauthResolvableBy),
// because it resolves through the same ResolveReauthApproval transaction and
// must not be a second, weaker door.
func (s *Server) reconcileReauthOnRead(ctx context.Context, ap types.ApprovalRequest) types.ApprovalRequest {
	if ap.Kind != types.ApprovalCredentialReauth || ap.State != types.ApprovalPending || s.cfg.Approvals == nil {
		return ap
	}
	run, err := s.cfg.Store.GetRun(ctx, ap.RunID)
	if err != nil {
		return ap
	}
	siteCfg, scErr := s.cfg.Store.GetSiteConfig(ctx)
	if scErr != nil {
		return ap // never derive a scope from a read that failed
	}
	scope := awsSSOScopeFor(siteCfg, run.Agent, runIdentitySubject(ctx, run.CreatedBy))
	blob, found, berr := s.readAWSSSOBlob(ctx, scope)
	if berr != nil || !found || blob.SourceRunID == "" {
		return ap
	}
	captureRunID, perr := uuid.Parse(blob.SourceRunID)
	if perr != nil {
		return ap
	}
	loginRun, lerr := s.cfg.Store.GetRun(ctx, captureRunID)
	if lerr != nil {
		return ap
	}
	if !reauthResolvableBy(ap, scope, loginRun) {
		return ap
	}
	// The credential must actually be usable: a blob captured after the raise
	// but already lapsed resolves nothing.
	if blob.expired(s.cfg.Now()) && !blob.renewable(s.cfg.Now()) {
		return ap
	}
	if err := s.resolveReauth(ctx, ap, loginRun.CreatedBy, captureRunID); err != nil {
		if !errors.Is(err, errReauthResolverUnavailable) {
			slog.WarnContext(ctx, "wardynd: could not reconcile an AWS sign-in request on read",
				slog.String("approval_id", ap.ID.String()), slog.Any("err", err))
		}
		return ap
	}
	fresh, gerr := s.cfg.Approvals.Get(ctx, ap.ID)
	if gerr != nil {
		return ap
	}
	return fresh
}
