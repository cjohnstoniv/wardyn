// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The Azure DevOps SIGN-IN HOLD: the per-person Azure DevOps lane's answer to a
// refresh token that dies while a run is working — the AWS SSO lane's
// credential_reauth hold (injection_awssso.go), with Azure DevOps sentences.
//
// Mid-run, a renewal that meets a dead refresh token (invalid_grant) or a
// Conditional Access policy that wants the person present
// (interaction_required) raises a credential_reauth request for the run's
// owner and answers 423; the proxy holds the request on it (credhold.go). The
// person's next Azure DevOps capture — the console login or the dedicated
// sign-in — resolves the request (resolvePendingADOReauth, eagerly, and
// reconcileADOReauthOnRead on the proxy's own poll), and the held request is
// re-resolved and forwarded.
//
// At the sidecar's BOOT there is no request to hold: the proxy marks that
// resolve (phase=boot) and the run fails with a failure hint that names the
// remedy. The redemption has already deleted a dead sign-in, or recorded a
// Conditional Access end on it (noteEntraSignInEnded), so /me/scm-access and
// the launch gate say so before the next launch.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// adoResolvePhase / adoResolvePhaseBoot are the query the proxy puts on its
// boot-time resolves. Mirrors internal/egress/proxy's own pair.
const (
	adoResolvePhase     = "phase"
	adoResolvePhaseBoot = "boot"
)

const (
	adoSignInMechanism = "entra_signin"
	adoSignInReason    = "signin"
)

// DRAFT (M2 canon pending)
const (
	adoSignInRaisedNote = "This run's Azure DevOps sign-in can no longer be renewed — the request is held while its " +
		"owner signs in to Azure DevOps again, and resumes when the sign-in lands; nothing is substituted"
	adoSignInClosedRefusal   = "This run's Azure DevOps sign-in request is closed: %s"
	adoSignInTooManyRefusal  = "This run has already asked for an Azure DevOps sign-in too many times; no further sign-in will be requested for it"
	adoSignInRaiseFailedBody = "Could not raise the Azure DevOps sign-in request"
	// adoOwnPATRaisedNote is adoSignInRaisedNote for a person's own token that
	// reached the expiry they entered (reasonADOOwnPATExpired): a new token,
	// not a sign-in, answers it.
	adoOwnPATRaisedNote = "This run's Azure DevOps token has reached the expiry its owner entered — the request is held " +
		"until its owner adds a new token, and resumes when they do; nothing is substituted"
)

// adoSignInScopeBody is the requested_scope of a sign-in request (kind
// credential_reauth). It is the dedup key, so it carries identity only; the
// class that raised it rides the audit row. No credential_source key, so the
// AWS resolver (reauthResolvableBy) can never match it.
type adoSignInScopeBody struct {
	Lane       string `json:"lane"`
	Mechanism  string `json:"mechanism"`
	Reason     string `json:"reason"`
	Owner      string `json:"owner"`
	ProviderID string `json:"provider_id"`
}

// adoSignInEndedClass reports whether class is one only a new sign-in fixes.
func adoSignInEndedClass(class ADOEntraFailure) bool {
	return class == ADOEntraFailureDeadCredential || class == ADOEntraFailureInteractionRequired
}

// answerADOSignInEnded answers a renewal that only a new sign-in can fix: a
// hold mid-run, a failure hint and a refusal at boot. false means class is not
// one of those, and nothing has been written.
func (s *Server) answerADOSignInEnded(w http.ResponseWriter, r *http.Request, claims *identity.Claims,
	sn adoEntraScopeSnapshot, class ADOEntraFailure, fail adoFail,
) bool {
	if class == ADOEntraFailureNotCaptured {
		// A dead sign-in is deleted as the hold is raised (noteEntraSignInEnded),
		// so the resolves that follow find none: while that request is open they
		// are still the lapse it holds for. Without one, not connected is refused.
		return r.URL.Query().Get(adoResolvePhase) != adoResolvePhaseBoot && s.cfg.Approvals != nil &&
			s.stillHeldForADOSignIn(r.Context(), w, claims, sn)
	}
	if !adoSignInEndedClass(class) {
		return false
	}
	_, body := adoResolveFailureAnswer(class)
	extra := map[string]any{"owner": sn.OwnerSubject}
	boot := r.URL.Query().Get(adoResolvePhase) == adoResolvePhaseBoot
	if boot {
		s.noteADOBootFailure(r.Context(), claims.RunID, body)
		extra["phase"] = adoResolvePhaseBoot
	}
	// No approval FSM wired means no hold lane: refuse, as before this existed.
	if boot || s.cfg.Approvals == nil {
		return fail(http.StatusForbidden, string(class), body, extra)
	}
	return s.holdForADOSignIn(w, r, claims, sn, class, fail)
}

// holdForADOSignIn is holdOrRefuseCredentialReauth for this lane: 423 while a
// sign-in request is open, 403 once one was refused, cancelled or aged out,
// and a new request otherwise — counted against the SAME per-run budget,
// maxReauthHolds, as the run's AWS re-auth workflows. Consent rows are
// credential_reauth too, but they have their own cap
// (maxADOCapabilityHoldsPerRun) and are not counted here.
func (s *Server) holdForADOSignIn(w http.ResponseWriter, r *http.Request, claims *identity.Claims,
	sn adoEntraScopeSnapshot, class ADOEntraFailure, fail adoFail,
) bool {
	// The body says what answers the request; the console reads it. Matching
	// (adoSignInScope) stays on lane and mechanism, so both causes resolve alike.
	reason, detail := adoSignInReason, adoSignInRaisedNote
	if class == ADOEntraFailure(reasonADOOwnPATExpired) {
		reason, detail = reasonADOOwnPATExpired, adoOwnPATRaisedNote
	}
	return s.holdForSignIn(w, r, claims, signInHold{
		lane: adoApprovalLane, owner: sn.OwnerSubject, providerID: sn.ProviderRowID, reason: reason, detail: detail,
		closed: adoSignInClosedRefusal, tooMany: adoSignInTooManyRefusal, raiseFailed: adoSignInRaiseFailedBody,
	}, class, fail)
}

// signInHold is one lane's sign-in hold: which approval lane its requests are
// raised and matched under, whose sign-in for which row answers them, and the
// sentences it speaks in. closed takes the terminal state.
type signInHold struct {
	lane, owner, providerID string
	reason, detail          string
	closed, tooMany         string
	raiseFailed             string
}

// holdForSignIn is the sign-in hold both Entra lanes share (Azure DevOps and
// azure_foundry); see holdForADOSignIn. The lane keeps the two lanes' rows
// apart: neither lane's capture resolves, or counts as a match for, the other's.
func (s *Server) holdForSignIn(w http.ResponseWriter, r *http.Request, claims *identity.Claims,
	h signInHold, class ADOEntraFailure, fail adoFail,
) bool {
	ctx := r.Context()
	rows, err := s.runApprovals(ctx, claims.RunID, "")
	if err != nil {
		return fail(http.StatusServiceUnavailable, reasonApprovalsUnreadable, adoCapApprovalsUnreadable, nil)
	}
	workflows := 0
	var terminal *types.ApprovalRequest
	for i := range rows {
		if rows[i].Kind != types.ApprovalCredentialReauth {
			continue
		}
		if _, consent := adoConsentScope(rows[i]); consent {
			continue
		}
		workflows++
		sc, ok := signInScopeFor(rows[i], h.lane)
		if !ok || sc.Owner != h.owner || sc.ProviderID != h.providerID {
			continue
		}
		switch rows[i].State {
		case types.ApprovalPending:
			writeJSON(w, http.StatusLocked, reauthPendingResponse{State: reauthPendingState, ApprovalID: rows[i].ID})
			return true
		case types.ApprovalCancelled, types.ApprovalExpired, types.ApprovalDenied:
			terminal = &rows[i]
		}
	}
	if terminal != nil {
		return fail(http.StatusForbidden, reasonSigninClosed, fmt.Sprintf(h.closed, terminal.State),
			map[string]any{"owner": h.owner, "approval_id": terminal.ID})
	}
	if workflows >= maxReauthHolds {
		return fail(http.StatusForbidden, reasonSigninHoldsExhausted, h.tooMany,
			map[string]any{"owner": h.owner})
	}
	raw, _ := json.Marshal(adoSignInScopeBody{
		Lane: h.lane, Mechanism: adoSignInMechanism, Reason: h.reason,
		Owner: h.owner, ProviderID: h.providerID,
	})
	raisedID := uuid.New()
	created, err := s.cfg.Approvals.Request(ctx, types.ApprovalRequest{
		ID: raisedID, RunID: claims.RunID, Kind: types.ApprovalCredentialReauth, RequestedScope: raw,
	})
	if err != nil {
		// Routed through fail (#204): every other refusal in this lane leaves a
		// secret.read failure row; this raise and the capability and consent
		// raises (injection_ado_capability.go) used to be the exceptions.
		return fail(http.StatusServiceUnavailable, reasonRaiseFailed, h.raiseFailed,
			map[string]any{"owner": h.owner})
	}
	if created.ID == raisedID {
		s.recordAudit(ctx, s.auditEvent(&claims.RunID, types.ActorSystem, "wardynd",
			"credential.reauth.request", created.ID.String(), "success",
			mustJSON(map[string]any{
				"approval_id": created.ID, "owner": h.owner, "provider": h.lane,
				"reason": string(class), "detail": h.detail,
			})))
		// No metrics.credentialReauthRecorded here (#971):
		// wardyn_credential_reauth_total's HELP promises the AWS SSO re-auth
		// population alone, and an Entra sign-in is credential_reauth too but
		// not that. The audit row above is the trail for these lanes.
	}
	writeJSON(w, http.StatusLocked, reauthPendingResponse{State: reauthPendingState, ApprovalID: created.ID})
	return true
}

// stillHeldForADOSignIn answers 423 when this run holds an open sign-in
// request for sn's owner and provider row, and reports whether it did. A read
// failure answers nothing, so the caller refuses.
func (s *Server) stillHeldForADOSignIn(ctx context.Context, w http.ResponseWriter, claims *identity.Claims, sn adoEntraScopeSnapshot) bool {
	return s.stillHeldForSignIn(ctx, w, claims, adoApprovalLane, sn.OwnerSubject, sn.ProviderRowID)
}

// stillHeldForSignIn is stillHeldForADOSignIn for either Entra lane.
func (s *Server) stillHeldForSignIn(ctx context.Context, w http.ResponseWriter, claims *identity.Claims, lane, owner, providerID string) bool {
	rows, err := s.runApprovals(ctx, claims.RunID, "")
	if err != nil {
		return false
	}
	for _, ap := range rows {
		sc, ok := signInScopeFor(ap, lane)
		if ok && ap.State == types.ApprovalPending && sc.Owner == owner && sc.ProviderID == providerID {
			writeJSON(w, http.StatusLocked, reauthPendingResponse{State: reauthPendingState, ApprovalID: ap.ID})
			return true
		}
	}
	return false
}

// adoSignInScope reports whether ap is an Azure DevOps sign-in request.
func adoSignInScope(ap types.ApprovalRequest) (adoSignInScopeBody, bool) {
	return signInScopeFor(ap, adoApprovalLane)
}

// signInScopeFor reports whether ap is a sign-in request raised under lane.
func signInScopeFor(ap types.ApprovalRequest, lane string) (adoSignInScopeBody, bool) {
	if ap.Kind != types.ApprovalCredentialReauth {
		return adoSignInScopeBody{}, false
	}
	var sc adoSignInScopeBody
	if json.Unmarshal(ap.RequestedScope, &sc) != nil || sc.Lane != lane || sc.Mechanism != adoSignInMechanism {
		return adoSignInScopeBody{}, false
	}
	return sc, true
}

// noteADOBootFailure writes hint to the run's failure_hint as the refusal
// happens — noteBedrockDataPlaneFault's precedent: a sidecar that fails its
// boot leaves the run to fail later with no reason of its own, and this is the
// one durable line the person reads. projectFailureHint keeps it off any run
// that does not end FAILED. Best-effort.
func (s *Server) noteADOBootFailure(ctx context.Context, runID uuid.UUID, hint string) {
	setter, ok := s.cfg.Store.(runFailureHintSetter)
	if !ok {
		return
	}
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil || (isTerminalRunState(run.State) && !(run.State == types.RunFailed && run.FailureHint == "")) {
		return
	}
	if err := setter.SetRunFailureHint(ctx, runID, hint); err != nil {
		slog.WarnContext(ctx, "wardynd: could not persist the azure devops sign-in failure hint",
			slog.String("run_id", runID.String()), slog.Any("err", err))
	}
}

// resolvePendingADOReauth resolves every PENDING Azure DevOps sign-in or
// consent request this capture answers — resolvePendingReauth's eager path for
// this lane. Called by both capture doors AFTER their own capture audit row
// (the captured -> resolved -> retry order). Best-effort: a resolution that
// does not land here is written by reconcileADOReauthOnRead on the proxy's
// next poll, through the same checks.
func (s *Server) resolvePendingADOReauth(ctx context.Context, owner, rowID string) {
	if s.cfg.Approvals == nil {
		return
	}
	rows, err := s.cfg.Approvals.List(ctx, types.ApprovalPending)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: could not list pending Azure DevOps sign-in requests after a capture; the next poll will resolve them",
			slog.Any("err", err))
		return
	}
	for _, ap := range rows {
		if sc, ok := adoReauthScope(ap); ok && sc.Owner == owner && sc.ProviderID == rowID {
			s.reconcileADOReauthOnRead(ctx, ap)
		}
	}
}

// adoReauthRow is what the two Azure DevOps credential_reauth shapes have in
// common: whose sign-in answers it, for which row, and the scopes it must
// carry (none for a sign-in request).
type adoReauthRow struct {
	Owner, ProviderID string
	Scopes            []string
}

// adoReauthScope reports whether ap is an Azure DevOps sign-in or consent
// request.
func adoReauthScope(ap types.ApprovalRequest) (adoReauthRow, bool) {
	if sc, ok := adoConsentScope(ap); ok {
		return adoReauthRow{Owner: sc.Owner, ProviderID: sc.ProviderID, Scopes: sc.Scopes}, true
	}
	if sc, ok := adoSignInScope(ap); ok {
		return adoReauthRow{Owner: sc.Owner, ProviderID: sc.ProviderID}, true
	}
	return adoReauthRow{}, false
}
