// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A custom endpoint that signs people in (types.ModelProvider.OIDC) at run time: the dispatch grant, the
// per-person resolve and the sign-in hold. The control-plane half of the sign-in endpoint_oidc.go captures.
//
// The run owner's own refresh token never leaves the sealed -oidc part. Each resolve redeems it for a
// short-lived access token (RedeemEndpointOIDCAccess) and hands that to the egress proxy alone, for the
// row's own host over TLS; the sandbox holds only the placeholder dispatch put in its environment.
//
// The order the checks MUST run in, as the Azure arm's (injection_azure.go):
//
//  1. the grant's dispatch-time SNAPSHOT (never the minted rule, which carries no identity) names this
//     provider, an owner and the address the row had at dispatch;
//  2. the snapshot's owner is the run token's own subject;
//  3. the live row is still the run's choice for the snapshot's harness, on, serving it, signing people in,
//     and at the address the snapshot recorded (a row edit after dispatch is a refusal, never a silent
//     substitution of one authority or endpoint for another);
//  4. the host, header and format the grant would inject with are the live row's own;
//  5. redeem the owner's sign-in, and classify any failure.

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/subscription"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// endpointOIDCApprovalLane is the lane an endpoint sign-in hold is raised and matched under. It is neither
// adoApprovalLane nor azureApprovalLane, so no other lane's capture resolves, or is resolved by, these rows.
const endpointOIDCApprovalLane = "custom_endpoint"

const (
	// mpRunNotSignedIn and mpRunSignInEnded are mpRunNoToken for an endpoint that signs people in.
	mpRunNotSignedIn = "you are not signed in to its endpoint"
	mpRunSignInEnded = "your sign-in to its endpoint has ended and needs a new one"

	providerAccessSignInEndpoint = "Sign in to %s"
	providerAccessEndpointEnded  = "Your sign-in to %s has ended — sign in again."
)

// The refusal bodies. Machine-facing (the caller is the proxy sidecar, which fails its boot closed on any of
// them), but each names the one thing a person reading the run's failure could do about it.
const (
	endpointOIDCResolveNotRecorded = "A model provider's sign-in is injected only through the grant Wardyn authors when a run launches on " +
		"that provider, which records whose sign-in the run uses; this grant carries no such record"
	endpointOIDCResolveChanged = "This run's model provider was removed, turned off, re-pointed or changed after the run started, " +
		"so its sign-in is no longer injected. Relaunch the run."
	endpointOIDCResolveHostPin     = "The sign-in's access token may only be injected to this run's own provider endpoint"
	endpointOIDCResolveUnreadable  = "Wardyn couldn't read this run's model provider just now, so its sign-in is not injected"
	endpointOIDCResolveNotCaptured = "The person who launched this run has not signed in to this model provider's endpoint — sign in from the console, then relaunch"
	endpointOIDCResolveDead        = "The sign-in to this model provider's endpoint can no longer be renewed — sign in again, then relaunch"
	endpointOIDCResolveConsent     = "This model provider's endpoint has not consented to the console's sign-in client for the scopes it asks for — " +
		"an administrator of the endpoint must grant them, then relaunch"
	endpointOIDCResolveInteraction = "This model provider's endpoint refused to renew the sign-in without the person present — sign in again. " +
		"Wardyn renews the sign-in from the control plane, which cannot satisfy a policy that needs the person's device or a fresh challenge."
	endpointOIDCResolveUnavailable = "Renewing the sign-in to this model provider's endpoint did not complete; nothing about the credential is known to be wrong"
	endpointOIDCResolveStoreRefuse = "The secret store refused the sign-in behind this run (it was moved or changed at the store, or Wardyn's " +
		"access to it was revoked) — sign in again, or ask an administrator to check the store"

	endpointOIDCSignInRaisedNote = "This run's sign-in to its model provider's endpoint can no longer be renewed — the request is held while its " +
		"owner signs in again, and resumes when the sign-in lands; nothing is substituted"
	endpointOIDCSignInClosedRefusal   = "This run's sign-in request to its model provider's endpoint is closed: %s"
	endpointOIDCSignInTooManyRefusal  = "This run has already asked for a sign-in to its model provider's endpoint too many times; no further sign-in will be requested for it"
	endpointOIDCSignInRaiseFailedBody = "Could not raise the sign-in request"
)

// providerKeyGrantScope is the api_key grant scope dispatch authors for a key or endpoint lane. A row that
// signs people in names the owner's sealed -oidc sign-in instead of a key, rides TLS only, and records the
// address its digest covers so the resolve can refuse a row that moved.
func providerKeyGrantScope(run types.AgentRun, lane providerKeyLane) map[string]any {
	snap := providerGrantSnapshot{OwnerSubject: lane.owner, ProviderUID: lane.provider.UID, Harness: run.Agent}
	scope := map[string]any{
		"host": lane.host, "header": lane.header, "format": lane.format,
		"secret_name": providerCredentialName(lane.provider),
	}
	if lane.provider.OIDC != nil {
		snap.AddressDigest = providerAddressDigest(lane.provider)
		scope["require_tls"] = true
	}
	scope["snapshot"] = snap
	return scope
}

// providerEndpointOIDCRefusal is the liveness of an endpoint that signs people in: the zero denial when
// owner has a sign-in at its authority that no renewal has found ended. Read strictly from owner's own
// namespace and recorded as a status read; nothing is redeemed (a dry check never rotates a refresh token).
func (s *Server) providerEndpointOIDCRefusal(ctx context.Context, owner string, p types.ModelProvider) (providerDenial, error) {
	blob, found, err := s.endpointOIDCBlob(secretstore.WithPurpose(ctx, secretstore.PurposeStatus), owner, p)
	switch {
	case err != nil:
		return providerDenial{}, err
	case !found:
		return connectDenial(p.ID, mpRunNotSignedIn), nil
	case blob.signInEnded():
		return connectDenial(p.ID, mpRunSignInEnded), nil
	}
	return providerDenial{}, nil
}

// gradeProviderEndpointOIDC grades a person's sign-in to an endpoint's authority from their own sealed -oidc
// part: absent -> not_configured (also what a sign-in deleted as dead reads), present with a recorded end
// (adoEntraBlob.DeadAt) -> expired_signin, else live. There is no expiring state: authorities publish no
// refresh-token lifetime, so any date shown would be a guess.
func (s *Server) gradeProviderEndpointOIDC(ctx context.Context, row *SetupProviderAccess, p types.ModelProvider, owner string) {
	blob, found, err := s.endpointOIDCBlob(ctx, owner, p)
	if err != nil {
		unreadableProvider(row)
		return
	}
	name := p.Name
	if name == "" {
		name = p.ID
	}
	if found && !blob.CapturedAt.IsZero() {
		at := blob.CapturedAt.UTC()
		row.SignedInAt = &at
	}
	switch {
	case !found:
		row.State = modelAccessNotConfigured
		row.Action = fmt.Sprintf(providerAccessSignInEndpoint, name)
	case blob.signInEnded():
		row.State = modelAccessExpiredSignin
		row.Action = fmt.Sprintf(providerAccessEndpointEnded, name)
	default:
		row.State = modelAccessLive
	}
}

// endpointOIDCReauthCapture is the capture that answers an endpoint sign-in request: the live row's, read
// from the site configuration. ok=false when the row is gone, off or no longer signs people in.
func (s *Server) endpointOIDCReauthCapture(ctx context.Context, sc adoSignInScopeBody) (adoSignInScopeBody, entraCapture, string, bool) {
	site, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return adoSignInScopeBody{}, entraCapture{}, "", false
	}
	row, found := endpointOIDCRowFor(site, sc.ProviderID)
	if !found {
		return adoSignInScopeBody{}, entraCapture{}, "", false
	}
	ec, err := endpointOIDCCapture(row, nil, nil)
	return sc, ec, endpointOIDCApprovalLane, err == nil
}

// providerOIDCUID is the provider uid in a wardyn-provider-<uid>-oidc secret name.
func providerOIDCUID(name string) (string, bool) {
	uid, ok := strings.CutPrefix(name, providerSecretPrefix)
	if !ok {
		return "", false
	}
	uid, ok = strings.CutSuffix(uid, "-"+providerOIDCPart)
	return uid, ok && uid != ""
}

// resolveEndpointOIDCInjection is the per-person endpoint sign-in arm of handleInternalInjection.
// handled=false means this grant is not ours and the next arm must run; handled=true means a response has
// been written. run is the one the withheld check read on this delivery; nil is a run it could not read.
func (s *Server) resolveEndpointOIDCInjection(w http.ResponseWriter, r *http.Request,
	claims *identity.Claims, minted broker.Minted, grantID uuid.UUID, run *types.AgentRun,
) bool {
	name := minted.Injection.SecretName
	uid, ok := providerOIDCUID(name)
	if !ok {
		return false
	}
	ctx := secretstore.WithPurpose(r.Context(), secretstore.PurposeProviderSignIn)
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
	var rec providerGrantSnapshot
	if !s.grantSnapshot(ctx, claims.RunID, grantID, &rec) || rec.ProviderUID != uid || rec.OwnerSubject == "" || rec.AddressDigest == "" {
		return fail(http.StatusForbidden, reasonMissingScopeSnapshot, endpointOIDCResolveNotRecorded, nil)
	}
	// The run token, not the grant, is authority for whose run this is.
	if rec.OwnerSubject != claims.Sub {
		return fail(http.StatusForbidden, reasonOwnerNotCaller, endpointOIDCResolveChanged, map[string]any{"owner": rec.OwnerSubject})
	}
	if run == nil || s.cfg.Store == nil {
		return fail(http.StatusServiceUnavailable, reasonRunUnreadable, endpointOIDCResolveUnreadable, nil)
	}
	siteCfg, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		// Never resolve against a read that failed: the zero SiteConfig is "no rows".
		return fail(http.StatusServiceUnavailable, reasonRosterUnreadable, endpointOIDCResolveUnreadable, nil)
	}
	harness, providerID, launched, err := s.sinkHarnessProvider(ctx, *run, rec.Harness)
	if err != nil {
		return fail(http.StatusServiceUnavailable, reasonRunUnreadable, endpointOIDCResolveUnreadable, nil)
	}
	p, found := modelProviderByID(siteCfg.ModelProviders, providerID)
	drift := endpointOIDCDrift(rec, p, found && launched, harness)
	if drift != "" {
		return fail(http.StatusForbidden, reasonScopeChanged, endpointOIDCResolveChanged, map[string]any{"drift": drift, "owner": rec.OwnerSubject})
	}
	lane, ok := providerKeyLaneFor(p, harness)
	if !ok || minted.Injection.Header != lane.header || minted.Injection.Format != lane.format {
		return fail(http.StatusForbidden, reasonScopeChanged, endpointOIDCResolveChanged,
			map[string]any{"drift": "lane_withdrawn", "owner": rec.OwnerSubject})
	}
	if !hostEqual(minted.Injection.Host, lane.host) {
		return fail(http.StatusForbidden, reasonHostNotEndpoint, endpointOIDCResolveHostPin, map[string]any{"host": minted.Injection.Host})
	}
	access, err := s.RedeemEndpointOIDCAccess(ctx, rec.OwnerSubject, p)
	if err != nil {
		return s.answerEntraRedeemFailure(w, r, claims, entraRedeemLane{
			lane: endpointOIDCApprovalLane, owner: rec.OwnerSubject, rowID: uid,
			notCaptured: endpointOIDCResolveNotCaptured, dead: endpointOIDCResolveDead, consent: endpointOIDCResolveConsent,
			raisedNote: endpointOIDCSignInRaisedNote, closed: endpointOIDCSignInClosedRefusal,
			tooMany: endpointOIDCSignInTooManyRefusal, raiseFailed: endpointOIDCSignInRaiseFailedBody,
			interaction: endpointOIDCResolveInteraction, storeRefused: endpointOIDCResolveStoreRefuse,
			unavailable: endpointOIDCResolveUnavailable,
		}, err, fail)
	}
	value := formatInjectionValue(lane.format, []byte(access.AccessToken))
	if s.refuseUnmasked(w, r, claims, "injection.resolve", []byte(access.AccessToken), []byte(value)) {
		return true
	}
	s.recordAudit(ctx, s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
		"secret.read", name, "success", mustJSON(map[string]any{
			"purpose": "proxy-injection-oidc", "grant_id": grantID, "jti": minted.JTI, "source": "provider",
			"owner": rec.OwnerSubject, "provider": p.ID, "provider_uid": uid,
			// What the AUTHORITY said the token carries: a token is opaque and never parsed.
			"granted_scope": strings.Join(access.Scopes, " "),
		})))
	s.stampCredentialUse(ctx, rec.OwnerSubject, name)
	writeJSON(w, http.StatusOK, injectionResponse{
		Host: minted.Injection.Host, Header: lane.header, Value: value, JTI: minted.JTI,
		// The token's own expiry or the stored-key lease, whichever is sooner.
		ExpiresAt: s.subscriptionLease(minted, subscription.Token{ExpiresAt: access.ExpiresAt}),
	})
	return true
}

// endpointOIDCDrift names the first thing that moved between the dispatch-time snapshot and the live row
// ("" = nothing). The name rides the audit row only: the caller's body is deliberately vague. launched is
// whether the run still has p for the snapshot's harness.
func endpointOIDCDrift(rec providerGrantSnapshot, p types.ModelProvider, launched bool, harness string) string {
	switch {
	case !launched || p.UID != rec.ProviderUID:
		return "row_withdrawn"
	case p.Disabled:
		return "row_disabled"
	case p.Kind != types.ModelProviderCustomEndpoint || p.OIDC == nil || !p.Serves(harness):
		return "lane_withdrawn"
	case providerAddressDigest(p) != rec.AddressDigest:
		return "address"
	}
	return ""
}
