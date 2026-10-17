// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The Azure CLI injection RESOLVE: the control-plane half of the lane
// authorAzureCLI authors at dispatch (runs_dispatch_cli_azure.go). Held to the
// rule the other captured-sign-in arms keep (injection_azure.go,
// injection_awssso.go): a resolve may refresh credential MATERIAL; it must
// never re-authorize the run against a different person, row, tenant,
// audience, subscription set or host than the one it was dispatched with.
//
// The order the checks run in:
//
//  1. the grant's own dispatch-time SNAPSHOT must name this row and an owner;
//  2. the snapshot's owner must be the run token's own subject;
//  3. the host the grant would inject onto must be Resource Manager's;
//  4. the run must still carry the row, and the live row must still pin the
//     snapshot's subscriptions and access (an edit after dispatch is a refusal,
//     never a silent substitution: the run's proxy still gates the old set);
//  5. the console's sign-in application must still be in the snapshot's tenant;
//  6. redeem this lane's one audience, and classify any failure.
//
// WHAT IT DOES NOT CLAIM. The access token is the person's own for all of
// Resource Manager; the proxy's door, not this token, is what holds a run to
// its row's subscriptions and access.

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/armscope"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/subscription"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The refusal bodies. Machine-facing (the caller is the proxy sidecar), but
// each names the one thing a person reading the run's failure could do.
//
// DRAFT (canon pending)
const (
	azureCLIResolveNotRecorded = "An Azure CLI's sign-in is injected only through the grant Wardyn authors when a run launches with " +
		"that component, which records whose sign-in the run uses; this grant carries no such record"
	azureCLIResolveChanged = "This run's Azure CLI component was removed or changed after the run started, " +
		"so its sign-in is no longer injected. Relaunch the run."
	azureCLIResolveHostPin     = "The Azure access token may only be injected to Azure Resource Manager"
	azureCLIResolveUnreadable  = "Wardyn couldn't read this run's Azure CLI component just now, so its Azure sign-in is not injected"
	azureCLIResolveNotCaptured = "The person who launched this run has not signed in to Azure for its Azure CLI — sign in from the console, then relaunch"
	azureCLIResolveDead        = "The Azure sign-in behind this run's Azure CLI can no longer be renewed — sign in to Azure again, then relaunch"
	azureCLIResolveConsent     = "Azure Resource Manager has not been consented for this deployment — an administrator must grant the console's " +
		"sign-in application the delegated permission \"Azure Service Management · user_impersonation\", with admin consent, then relaunch"
)

// cliEntraComponentID returns the component id a wardyn-cli-<id>-entra name names.
func cliEntraComponentID(name string) (string, bool) {
	id, ok := strings.CutPrefix(name, cliSecretPrefix)
	if !ok {
		return "", false
	}
	id, ok = strings.CutSuffix(id, "-"+providerEntraPart)
	return id, ok && id != ""
}

// resolveAzureARMInjection is the Azure CLI arm of handleInternalInjection.
// handled=false means this grant is not ours and the generic path must run;
// handled=true means a response has been written.
func (s *Server) resolveAzureARMInjection(w http.ResponseWriter, r *http.Request,
	claims *identity.Claims, minted broker.Minted, grantID uuid.UUID,
) bool {
	name := minted.Injection.SecretName
	id, ok := cliEntraComponentID(name)
	if !ok {
		return false
	}
	ctx := secretstore.WithPurpose(r.Context(), secretstore.PurposeADORefresh)
	fail := func(status int, reason, body string, extra map[string]any) bool {
		data := map[string]any{"reason": reason, "grant_id": grantID, "source": "cli"}
		for k, v := range extra {
			data[k] = v
		}
		s.recordAudit(ctx, s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
			"secret.read", name, "failure", mustJSON(data)))
		writeErrorReason(w, status, reason, body)
		return true
	}
	var sn azureARMGrantSnapshot
	if !s.grantSnapshot(ctx, claims.RunID, grantID, &sn) || !sn.authored() || sn.ComponentID != id {
		return fail(http.StatusForbidden, reasonMissingScopeSnapshot, azureCLIResolveNotRecorded, nil)
	}
	owner := map[string]any{"owner": sn.OwnerSubject}
	// The run token, not the grant, is authority for whose run this is.
	if sn.OwnerSubject != claims.Sub {
		return fail(http.StatusForbidden, reasonOwnerNotCaller, azureCLIResolveChanged, owner)
	}
	if !hostEqual(minted.Injection.Host, armscope.Host) {
		return fail(http.StatusForbidden, reasonHostNotEndpoint, azureCLIResolveHostPin, map[string]any{"host": minted.Injection.Host})
	}
	drift, err := s.azureCLIDrift(ctx, claims.RunID, sn)
	if err != nil {
		// Never resolve against a read that failed.
		return fail(http.StatusServiceUnavailable, reasonRunUnreadable, azureCLIResolveUnreadable, nil)
	}
	cfg, configured := s.azureCLIEntra(id)
	switch {
	case drift == "" && !configured:
		return fail(http.StatusForbidden, reasonSigninUnconfigured, azureResolveUnconfig, nil)
	case drift == "" && !strings.EqualFold(cfg.TenantID, sn.TenantID):
		drift = "tenant"
	}
	if drift != "" {
		return fail(http.StatusForbidden, reasonScopeChanged, azureCLIResolveChanged, map[string]any{"drift": drift, "owner": sn.OwnerSubject})
	}
	access, err := s.RedeemAzureARMAccess(ctx, cfg, sn.OwnerSubject, id)
	if err != nil {
		return s.answerEntraRedeemFailure(w, r, claims, entraRedeemLane{
			lane: azureCLIMechanism, owner: sn.OwnerSubject, rowID: id,
			notCaptured: azureCLIResolveNotCaptured, dead: azureCLIResolveDead, consent: azureCLIResolveConsent, raisedNote: azureCLIReauthRaisedNote,
			closed: azureCLISignInClosedRefusal, tooMany: azureCLITooManyRefusal, raiseFailed: azureCLIRaiseFailedBody,
		}, err, fail)
	}
	// The ONE wire shape, forced whatever the grant authored.
	value := formatInjectionValue("Bearer %s", []byte(access.AccessToken))
	if s.refuseUnmasked(w, r, claims, "injection.resolve", []byte(access.AccessToken), []byte(value)) {
		return true
	}
	s.recordAudit(ctx, s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
		"secret.read", name, "success", mustJSON(map[string]any{
			"purpose": "proxy-injection-azure-cli", "grant_id": grantID, "jti": minted.JTI, "source": "cli",
			"owner": sn.OwnerSubject, "component_id": id, "tenant_id": sn.TenantID, "audience": sn.Audience,
			// What the AUTHORITY said the token carries: a token is opaque and never parsed.
			"granted_scope": strings.Join(access.Scopes, " "),
		})))
	s.stampCredentialUse(ctx, sn.OwnerSubject, name)
	writeJSON(w, http.StatusOK, injectionResponse{
		Host: minted.Injection.Host, Header: "Authorization", Value: value, JTI: minted.JTI,
		// The token's own expiry or the grant's lease, whichever is sooner.
		ExpiresAt: s.subscriptionLease(minted, subscription.Token{ExpiresAt: access.ExpiresAt}),
	})
	return true
}

// azureCLIDrift names the first thing that moved between the dispatch-time
// snapshot and now ("" = nothing): the run no longer carries the row, the row
// is gone or no longer an Azure CLI row, or it pins other subscriptions or
// another access than the run's proxy was configured with. The name rides the
// audit row only. A store that cannot answer is an error, never a drift.
func (s *Server) azureCLIDrift(ctx context.Context, runID uuid.UUID, sn azureARMGrantSnapshot) (string, error) {
	cs, ok := s.cfg.Store.(store.ComponentStore)
	id, perr := uuid.Parse(sn.ComponentID)
	if !ok || perr != nil {
		return "component", nil
	}
	attached, err := cs.ListRunComponents(ctx, runID)
	if err != nil {
		return "", err
	}
	if !slices.ContainsFunc(attached, func(rc types.RunComponent) bool {
		return !rc.SelfDefined && rc.ComponentID != nil && *rc.ComponentID == id
	}) {
		return "component", nil
	}
	row, err := cs.GetComponent(ctx, id, "")
	switch {
	case errors.Is(err, store.ErrNotFound):
		return "row_withdrawn", nil
	case err != nil:
		return "", err
	}
	az := attachedAzureCLI(attachedComponent{snapshot: types.RunComponent{ComponentID: &id, Definition: row.Definition}})
	switch {
	case az == nil:
		return "row_withdrawn", nil
	case !slices.Equal(azureCLISubscriptions(*az), sn.Subscriptions):
		return "subscriptions", nil
	case az.Access != sn.Access:
		return "access", nil
	}
	return "", nil
}
