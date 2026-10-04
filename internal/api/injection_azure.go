// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The per-person Azure Foundry injection RESOLVE: the control-plane half of the lane
// authorAzureInjection authors at dispatch (provider_azure.go). Modelled on the Azure DevOps arm
// (injection_ado.go) and the captured-AWS-SSO arm (injection_awssso.go), and held to their rule: a
// resolve may refresh credential MATERIAL; it must never re-authorize the run against a different
// person, provider row, audience or host than the one it was dispatched with.
//
// The order the checks MUST run in:
//
//  1. the grant's own dispatch-time SNAPSHOT, read from the grant (never the minted rule, which carries
//     no identity), must name this provider and an owner;
//  2. the snapshot's owner must be the run token's own subject;
//  3. the live provider row must still be the run's choice, on, serving its agent, with the snapshot's
//     provider UID and audience (a row edit after dispatch is a refusal, never a silent substitution);
//  4. the host the grant would inject onto must be the live row's own endpoint host;
//  5. redeem the snapshot's audience, and classify any failure.
//
// WHAT IT DOES NOT CLAIM. The access token is the person's own for the audience's whole resource; the
// proxy's route gate (azure_gate.go), not this token, is what holds a run to inference calls on its
// pinned deployments.

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/subscription"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// azureApprovalLane is the lane a sign-in hold for this kind is raised and matched under. It is not
// adoApprovalLane, so neither Entra lane's capture resolves, or is resolved by, the other's rows.
const azureApprovalLane = "azure_foundry"

// The refusal bodies. Machine-facing (the caller is the proxy sidecar, which fails its boot closed on
// any of them), but each names the one thing a person reading the run's failure could do about it.
//
// DRAFT (M2 canon pending)
const (
	azureResolveNotRecorded = "A model provider's Azure sign-in is injected only through the grant Wardyn authors when a run launches on " +
		"that provider, which records whose sign-in the run uses; this grant carries no such record"
	azureResolveChanged = "This run's Azure model provider was removed, turned off, re-pointed or changed after the run started, " +
		"so its sign-in is no longer injected. Relaunch the run."
	azureResolveHostPin     = "The Azure access token may only be injected to this run's own provider endpoint"
	azureResolveUnreadable  = "Wardyn couldn't read this run's model provider just now, so its Azure sign-in is not injected"
	azureResolveUnconfig    = "This deployment has no console Entra sign-in configured, so no Azure sign-in can be resolved"
	azureResolveNotCaptured = "The person who launched this run has not signed in to Azure for this model provider — sign in from the console, then relaunch"
	azureResolveDead        = "The Azure sign-in behind this run can no longer be renewed — sign in to Azure again, then relaunch"
	azureResolveConsent     = "Azure has not been consented for this model provider's audience — an administrator must grant the console's " +
		"sign-in application the delegated permission for it, with admin consent, then relaunch"
	// azureResolveInteraction takes the policy class Entra reported (azureConditionalAccessClass).
	azureResolveInteraction = "Azure refused to renew this sign-in without the person present: %s. Wardyn renews the sign-in from the control " +
		"plane's address, which cannot satisfy that policy — sign in to Azure again, or ask an administrator to exempt this application. " +
		"Policies that need the person's device or a fresh challenge are not supported."
	azureResolveUnavailable = "Renewing the Azure sign-in behind this run did not complete; nothing about the credential is known to be wrong"
	azureResolveStoreRefuse = "The secret store refused the Azure sign-in behind this run (it was moved or changed at the store, or Wardyn's " +
		"access to it was revoked) — sign in to Azure again, or ask an administrator to check the store"

	azureSignInRaisedNote = "This run's Azure sign-in can no longer be renewed — the request is held while its owner signs in to Azure " +
		"again, and resumes when the sign-in lands; nothing is substituted"
	azureSignInClosedRefusal   = "This run's Azure sign-in request is closed: %s"
	azureSignInTooManyRefusal  = "This run has already asked for an Azure sign-in too many times; no further sign-in will be requested for it"
	azureSignInRaiseFailedBody = "Could not raise the Azure sign-in request"
)

// azureConditionalAccessClasses names the policy an Entra interaction refusal came from, by the AADSTS
// number it carries. Only the classes the number itself states: a refresh redeemed from the control plane
// cannot satisfy any of them, and none is promised to be supportable.
var azureConditionalAccessClasses = map[string]string{
	"AADSTS50076":  "a Conditional Access policy requires multi-factor authentication",
	"AADSTS50079":  "a Conditional Access policy requires multi-factor authentication",
	"AADSTS50158":  "a Conditional Access policy requires an external security challenge",
	"AADSTS53000":  "a Conditional Access policy requires a compliant device",
	"AADSTS53001":  "a Conditional Access policy requires a domain-joined device",
	"AADSTS53002":  "a Conditional Access policy requires an approved client application",
	"AADSTS53003":  "a Conditional Access policy blocks token issuance",
	"AADSTS530032": "a Conditional Access policy blocks token issuance",
}

// azureConditionalAccessClass is the sentence naming which policy class blocked a renewal, from the
// AADSTS number in err, or a generic one when Entra named none.
func azureConditionalAccessClass(err error) string {
	if code := aadstsPattern.FindString(err.Error()); code != "" {
		if class, ok := azureConditionalAccessClasses[code]; ok {
			return class + " (" + code + ")"
		}
		return "a Conditional Access policy refused the renewal (" + code + ")"
	}
	return "a Conditional Access policy refused the renewal"
}

// resolveAzureFoundryInjection is the per-person Azure Foundry arm of handleInternalInjection.
// handled=false means this grant is not ours and the generic path must run; handled=true means a
// response has been written.
func (s *Server) resolveAzureFoundryInjection(w http.ResponseWriter, r *http.Request,
	claims *identity.Claims, minted broker.Minted, grantID uuid.UUID,
) bool {
	name := minted.Injection.SecretName
	uid, ok := providerEntraUID(name)
	if !ok {
		return false
	}
	ctx := secretstore.WithPurpose(r.Context(), secretstore.PurposeADORefresh)
	fail := func(status int, reason, body string, extra map[string]any) bool {
		data := map[string]any{"reason": reason, "grant_id": grantID, "source": "provider"}
		for k, v := range extra {
			data[k] = v
		}
		s.recordAudit(ctx, s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
			"secret.read", name, "failure", mustJSON(data)))
		writeErrorReason(w, status, reason, body)
		return true
	}
	var sn azureGrantSnapshot
	if !s.grantSnapshot(ctx, claims.RunID, grantID, &sn) || !sn.authored() || sn.ProviderUID != uid {
		return fail(http.StatusForbidden, reasonMissingScopeSnapshot, azureResolveNotRecorded, nil)
	}
	// The run token, not the grant, is authority for whose run this is.
	if sn.OwnerSubject != claims.Sub {
		return fail(http.StatusForbidden, reasonOwnerNotCaller, azureResolveChanged, map[string]any{"owner": sn.OwnerSubject})
	}
	run, err := s.cfg.Store.GetRun(ctx, claims.RunID)
	if err != nil {
		return fail(http.StatusServiceUnavailable, reasonRunUnreadable, azureResolveUnreadable, nil)
	}
	siteCfg, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		// Never resolve against a read that failed: the zero SiteConfig is "no rows".
		return fail(http.StatusServiceUnavailable, reasonRosterUnreadable, azureResolveUnreadable, nil)
	}
	p, found := modelProviderByID(siteCfg.ModelProviders, run.ModelProviderID)
	lane, laneOK := providerAzureLaneFor(p, run.Agent)
	if drift := azureDrift(sn, p, found, lane, laneOK); drift != "" {
		return fail(http.StatusForbidden, reasonScopeChanged, azureResolveChanged, map[string]any{"drift": drift, "owner": sn.OwnerSubject})
	}
	if !hostEqual(minted.Injection.Host, lane.host) {
		return fail(http.StatusForbidden, reasonHostNotEndpoint, azureResolveHostPin, map[string]any{"host": minted.Injection.Host})
	}
	if s.cfg.AzureFoundryEntra == nil {
		return fail(http.StatusForbidden, reasonSigninUnconfigured, azureResolveUnconfig, nil)
	}
	cfg, cfgFound, err := s.cfg.AzureFoundryEntra(ctx, uid)
	switch {
	case err != nil:
		return fail(http.StatusServiceUnavailable, reasonSigninUnreadable, azureResolveUnreadable, nil)
	case !cfgFound:
		return fail(http.StatusForbidden, reasonSigninUnconfigured, azureResolveUnconfig, nil)
	}
	access, err := s.RedeemAzureFoundryAccess(ctx, cfg, sn.OwnerSubject, uid, sn.Audience)
	if err != nil {
		return s.answerAzureRedeemFailure(w, r, claims, sn, err, fail)
	}
	// The ONE wire shape, forced whatever the grant authored.
	value := formatInjectionValue("Bearer %s", []byte(access.AccessToken))
	if s.cfg.MaskRegistry != nil {
		s.cfg.MaskRegistry.Add(claims.RunID, []byte(access.AccessToken))
		s.cfg.MaskRegistry.Add(claims.RunID, []byte(value))
	}
	s.recordAudit(ctx, s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
		"secret.read", name, "success", mustJSON(map[string]any{
			"purpose": "proxy-injection-azure", "grant_id": grantID, "jti": minted.JTI, "source": "provider",
			"owner": sn.OwnerSubject, "provider": p.ID, "provider_uid": uid, "audience": sn.Audience,
			// What the AUTHORITY said the token carries: a token is opaque and never parsed.
			"granted_scope": strings.Join(access.Scopes, " "),
		})))
	s.stampCredentialUse(ctx, sn.OwnerSubject, name)
	writeJSON(w, http.StatusOK, injectionResponse{
		Host: minted.Injection.Host, Header: "Authorization", Value: value, JTI: minted.JTI,
		// The token's own expiry or the stored-key lease, whichever is sooner.
		ExpiresAt: s.subscriptionLease(minted, subscription.Token{ExpiresAt: access.ExpiresAt}),
	})
	return true
}

// azureDrift names the first thing that moved between the dispatch-time snapshot and the live row ("" =
// nothing). The name rides the audit row only: the caller's body is deliberately vague.
func azureDrift(sn azureGrantSnapshot, p types.ModelProvider, found bool, lane providerAzureLane, laneOK bool) string {
	switch {
	case !found || p.UID != sn.ProviderUID:
		return "row_withdrawn"
	case p.Disabled:
		return "row_disabled"
	case !laneOK:
		return "lane_withdrawn"
	case lane.audience != sn.Audience:
		return "audience"
	}
	return ""
}

// answerAzureRedeemFailure answers a redemption failure by class. Each class has a different remedy:
// the two a new sign-in fixes are a hold mid-run (423) and a failure hint at boot, the rest a refusal.
func (s *Server) answerAzureRedeemFailure(w http.ResponseWriter, r *http.Request, claims *identity.Claims,
	sn azureGrantSnapshot, err error, fail func(int, string, string, map[string]any) bool,
) bool {
	class := ADOEntraClassify(err)
	extra := map[string]any{"owner": sn.OwnerSubject}
	boot := r.URL.Query().Get(adoResolvePhase) == adoResolvePhaseBoot
	held := func() bool {
		return s.stillHeldForSignIn(r.Context(), w, claims, azureApprovalLane, sn.OwnerSubject, sn.ProviderUID)
	}
	switch class {
	case ADOEntraFailureNotCaptured:
		// A dead sign-in is deleted as the hold is raised, so the resolves that follow find none: while that
		// request is open they are still the lapse it holds for.
		if !boot && s.cfg.Approvals != nil && held() {
			return true
		}
		return fail(http.StatusForbidden, string(class), azureResolveNotCaptured, extra)
	case ADOEntraFailureDeadCredential, ADOEntraFailureInteractionRequired:
		body, note := azureResolveDead, azureSignInRaisedNote
		if class == ADOEntraFailureInteractionRequired {
			body = fmt.Sprintf(azureResolveInteraction, azureConditionalAccessClass(err))
			note = body
		}
		if boot {
			s.noteADOBootFailure(r.Context(), claims.RunID, body)
			extra["phase"] = adoResolvePhaseBoot
		}
		if boot || s.cfg.Approvals == nil {
			return fail(http.StatusForbidden, string(class), body, extra)
		}
		return s.holdForSignIn(w, r, claims, signInHold{
			lane: azureApprovalLane, owner: sn.OwnerSubject, providerID: sn.ProviderUID, reason: adoSignInReason, detail: note,
			closed: azureSignInClosedRefusal, tooMany: azureSignInTooManyRefusal, raiseFailed: azureSignInRaiseFailedBody,
		}, class, fail)
	case ADOEntraFailureConsentRequired:
		return fail(http.StatusForbidden, string(class), azureResolveConsent, extra)
	case ADOEntraFailureStoreRefused:
		return fail(http.StatusForbidden, string(class), azureResolveStoreRefuse, extra)
	}
	return fail(http.StatusServiceUnavailable, string(class), azureResolveUnavailable, extra)
}

// reconcileAzureReauthOnRead resolves a PENDING Azure sign-in request once the person has signed in again
// after it was raised — on the read the proxy's hold is already making, and from the capture itself
// (resolvePendingAzureReauth). The capture is the resolution; this writes it down, through the same
// one-transaction seam the other lanes use.
func (s *Server) reconcileAzureReauthOnRead(ctx context.Context, ap types.ApprovalRequest) types.ApprovalRequest {
	sc, ok := signInScopeFor(ap, azureApprovalLane)
	if !ok || ap.State != types.ApprovalPending || s.cfg.Approvals == nil || s.cfg.Store == nil {
		return ap
	}
	resolver, ok := s.cfg.Store.(reauthResolver)
	if !ok {
		return ap
	}
	site, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return ap
	}
	facts, ok := azureFoundryRowFor(site, sc.ProviderID)
	if !ok {
		return ap
	}
	ec, err := azureFoundryCapture(sc.ProviderID, facts.audience)
	if err != nil {
		return ap
	}
	// Generation: only a sign-in captured AFTER the raise answers it, and only one no renewal has since found ended.
	blob, found, err := s.readEntraBlob(secretstore.WithPurpose(ctx, secretstore.PurposeStatus), sc.Owner, ec)
	if err != nil || !found || !blob.CapturedAt.After(ap.RequestedAt) || blob.signInEnded() {
		return ap
	}
	ev := s.auditEvent(&ap.RunID, types.ActorHuman, sc.Owner, "credential.reauth.resolve", ap.ID.String(), "success",
		mustJSON(map[string]any{
			"approval_id": ap.ID, "owner": sc.Owner, "resolved_by": sc.Owner, "provider": azureApprovalLane,
		}))
	if _, err := resolver.ResolveReauthApproval(ctx, ap.ID, types.ApprovalDecision{
		State: types.ApprovalApproved, DecidedBy: sc.Owner, Reason: "signed in again",
	}, ev); err != nil {
		return ap
	}
	s.approvalClosed(ctx, ap.RunID)
	if fresh, gerr := s.cfg.Approvals.Get(ctx, ap.ID); gerr == nil {
		return fresh
	}
	return ap
}

// resolvePendingAzureReauth resolves every PENDING Azure sign-in request this capture answers: the
// eager path, called by the capture callback AFTER its own audit row (captured -> resolved -> retry).
// Best-effort: a resolution that does not land here is written by reconcileAzureReauthOnRead on the
// proxy's next poll, through the same checks.
func (s *Server) resolvePendingAzureReauth(ctx context.Context, owner, providerUID string) {
	if s.cfg.Approvals == nil {
		return
	}
	rows, err := s.cfg.Approvals.List(ctx, types.ApprovalPending)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: could not list pending Azure sign-in requests after a capture; the next poll will resolve them",
			slog.Any("err", err))
		return
	}
	for _, ap := range rows {
		if sc, ok := signInScopeFor(ap, azureApprovalLane); ok && sc.Owner == owner && sc.ProviderID == providerUID {
			s.reconcileAzureReauthOnRead(ctx, ap)
		}
	}
}
