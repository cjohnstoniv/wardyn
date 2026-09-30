// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The run-time half of token_mode own_pat: a run on such a row is DISPATCHED by
// the per-person Azure DevOps lane unchanged (authorADOEntraLane: the same
// organisation-pinned hosts, REST gate, git broker and inert placeholder; its
// snapshot names token_mode own_pat and no tenant or client), and RESOLVED
// here, ahead of the Entra arm, from the run owner's own stored token.
//
// The resolve keeps the Entra arm's rule: it may hand the proxy fresh MATERIAL,
// never a different person's, row's, organisation's or capability set's. What
// differs is only where the material comes from and how it is presented —
// Basic with the token as the password, the one shape a personal access token
// has — and that it stops at the expiry the person entered: past it the run is
// held on the existing sign-in request until the person adds a new token.

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/subscription"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The resolve's own refusal bodies, beside the Entra arm's. Machine-facing
// first (the proxy fails its boot closed on any of them), and each names what
// the run's owner can do.
//
// DRAFT (M2 canon pending)
const (
	adoOwnPATNotAddedRefusal = "The person who launched this run has not added their own Azure DevOps token — add it under Settings, then relaunch"
	adoOwnPATExpiredRefusal  = "The Azure DevOps token behind this run has reached the expiry its owner entered — add a new token under Settings, then relaunch"
	adoOwnPATOtherOrgRefusal = "The Azure DevOps token its owner added is for a different organisation than this run's — add one for this organisation, then relaunch"
)

// adoOwnPATGrantSnapshot reads an own-token grant's dispatch-time snapshot
// through the run's own grant list (adoEntraGrantSnapshot's rule). ok=false
// for any grant that is not own_pat's, which the Entra arm then answers.
func (s *Server) adoOwnPATGrantSnapshot(r *http.Request, runID, grantID uuid.UUID) (adoEntraScopeSnapshot, bool) {
	grants, err := s.cfg.Store.ListGrantsByRun(r.Context(), runID)
	if err != nil {
		return adoEntraScopeSnapshot{}, false
	}
	i := slices.IndexFunc(grants, func(g types.CredentialGrant) bool { return g.ID == grantID })
	if i < 0 {
		return adoEntraScopeSnapshot{}, false
	}
	var sc struct {
		Snapshot adoEntraScopeSnapshot `json:"snapshot"`
	}
	if json.Unmarshal(grants[i].Spec.Scope, &sc) != nil {
		return adoEntraScopeSnapshot{}, false
	}
	sn := sc.Snapshot
	if types.ADOTokenMode(sn.TokenMode) != types.ADOTokenModeOwnPAT || sn.ProviderRowID == "" ||
		sn.Organisation == "" || sn.OwnerSubject == "" || len(sn.Capabilities) == 0 {
		return adoEntraScopeSnapshot{}, false
	}
	return sn, true
}

// resolveADOOwnPATInjection is handleInternalInjection's own-token arm, tried
// before resolveADOInjection. handled=false means the grant is not an own-token
// grant and the Entra arm must run.
//
// The order is the Entra arm's: the snapshot; its owner is the run token's own
// subject; every field equal to the live row; the host inside the snapshot's
// organisation; the capabilities grantable; the capability ask; then the
// owner's own token, which must be for this organisation and not expired.
func (s *Server) resolveADOOwnPATInjection(w http.ResponseWriter, r *http.Request,
	claims *identity.Claims, minted broker.Minted, grantID uuid.UUID,
) bool {
	if minted.Injection == nil || minted.Injection.SecretName != types.ADOEntraAccessTokenSecret {
		return false
	}
	snapshot, ok := s.adoOwnPATGrantSnapshot(r, claims.RunID, grantID)
	if !ok {
		return false
	}
	ctx := r.Context()
	fail := func(status int, reason, body string, extra map[string]any) bool {
		data := map[string]any{"reason": reason, "grant_id": grantID, "token_mode": types.ADOTokenModeOwnPAT}
		for k, v := range extra {
			data[k] = v
		}
		s.recordAudit(ctx, s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
			"secret.read", types.ADOEntraAccessTokenSecret, "failure", mustJSON(data)))
		writeErrorReason(w, status, reason, body)
		return true
	}
	if snapshot.OwnerSubject != claims.Sub {
		return fail(http.StatusForbidden, reasonOwnerNotCaller, adoResolveScopeChangedRefusal,
			map[string]any{"owner": snapshot.OwnerSubject})
	}
	siteCfg, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return fail(http.StatusServiceUnavailable, reasonRosterUnreadable, adoResolveRosterUnreadable, nil)
	}
	if drift := snapshot.driftFrom(siteCfg); drift != "" {
		return fail(http.StatusForbidden, reasonScopeChanged, adoResolveScopeChangedRefusal,
			map[string]any{"drift": drift, "owner": snapshot.OwnerSubject})
	}
	if !slices.ContainsFunc(adoEntraHosts(snapshot.Organisation), func(h string) bool { return hostEqual(h, minted.Injection.Host) }) {
		return fail(http.StatusForbidden, reasonHostNotOrganisation, adoResolveHostPinRefusal,
			map[string]any{"host": minted.Injection.Host})
	}
	if _, serr := adoscope.ScopesFor(snapshot.Capabilities); serr != nil {
		return fail(http.StatusForbidden, reasonCapabilityNotGrantable, adoResolveScopeChangedRefusal, nil)
	}

	// THE CAPABILITY ARM, unchanged: above the ceiling, always_deny and denied
	// refuse; a grant, an approval or a spent `once` lets it through.
	responseCaps := snapshot.Capabilities
	var capAsk adoCapabilityGrant
	if r.URL.Query().Get("capability") != "" {
		if capAsk, ok = s.answerADOCapability(w, r, claims, snapshot, siteCfg, grantID, fail); !ok {
			return true
		}
		responseCaps = capAsk.standing
	}

	blob, found, err := s.readADOOwnPAT(secretstore.WithPurpose(ctx, secretstore.PurposeDispatch), snapshot.OwnerSubject, snapshot.ProviderRowID)
	boot := r.URL.Query().Get(adoResolvePhase) == adoResolvePhaseBoot
	switch {
	case err != nil:
		return fail(http.StatusForbidden, string(ADOEntraFailureStoreRefused), adoResolveStoreRefused,
			map[string]any{"owner": snapshot.OwnerSubject})
	case !found:
		// Removed mid-run while its hold is open: still the lapse that hold is for.
		if !boot && s.cfg.Approvals != nil && s.stillHeldForADOSignIn(ctx, w, claims, snapshot) {
			return true
		}
		return fail(http.StatusForbidden, reasonADOOwnPATNotAdded, adoOwnPATNotAddedRefusal,
			map[string]any{"owner": snapshot.OwnerSubject})
	case blob.Org != snapshot.Organisation:
		return fail(http.StatusForbidden, reasonADOOwnPATOtherOrg, adoOwnPATOtherOrgRefusal,
			map[string]any{"owner": snapshot.OwnerSubject})
	case blob.expired(s.cfg.Now()):
		return s.answerADOOwnPATExpired(w, r, claims, snapshot, boot, fail)
	}
	// Wardyn cannot read a pasted token's scopes, so there is no consent to
	// check: the capability arm's decision is the whole gate. The ask is
	// settled against its own scopes so a `once` approval is spent exactly as
	// on the Entra arm.
	if capAsk.capability != "" {
		need, _ := adoscope.ScopesFor([]adoscope.Capability{capAsk.capability})
		if s.settleADOCapability(w, r, claims, snapshot, ADOEntraConfig{}, grantID, minted.JTI, capAsk, need,
			ADOEntraAccess{Scopes: need}, fail) {
			return true
		}
	}

	value := adoOwnPATHeaderValue(blob.Token)
	if s.cfg.MaskRegistry != nil {
		s.cfg.MaskRegistry.Add(claims.RunID, []byte(blob.Token))
		s.cfg.MaskRegistry.Add(claims.RunID, []byte(base64.StdEncoding.EncodeToString([]byte(":"+blob.Token))))
		s.cfg.MaskRegistry.Add(claims.RunID, []byte(value))
	}
	data := map[string]any{
		"purpose": "proxy-injection-ado", "grant_id": grantID, "jti": minted.JTI,
		"token_mode": types.ADOTokenModeOwnPAT, "owner": snapshot.OwnerSubject,
		"provider_row": snapshot.ProviderRowID, "organisation": snapshot.Organisation,
		"capabilities": responseCaps, "expires_on": blob.ExpiresOn,
	}
	if capAsk.capability != "" {
		data["capability"] = capAsk.capability
		if capAsk.once != nil {
			data["approval_id"] = capAsk.once.ID
		}
	}
	s.recordAudit(ctx, s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
		"secret.read", types.ADOEntraAccessTokenSecret, "success", mustJSON(data)))
	writeJSON(w, http.StatusOK, injectionResponse{
		Host:   minted.Injection.Host,
		Header: adoEntraInjectHeader,
		Value:  value,
		JTI:    minted.JTI,
		// The stored-key lease, or the expiry the person entered when sooner:
		// the proxy re-resolves by then, and a removed or expired token stops.
		ExpiresAt:    s.subscriptionLease(minted, subscription.Token{ExpiresAt: blob.ExpiresOn}),
		Organisation: snapshot.Organisation,
		Capabilities: adoCapabilityStrings(responseCaps),
	})
	return true
}

// answerADOOwnPATExpired answers a token past the expiry its owner entered: at
// the sidecar's boot a refusal and the run's failure hint; mid-run the
// existing Azure DevOps sign-in hold, which the owner's next token answers
// (resolvePendingADOOwnPATHolds).
func (s *Server) answerADOOwnPATExpired(w http.ResponseWriter, r *http.Request, claims *identity.Claims,
	sn adoEntraScopeSnapshot, boot bool, fail adoFail,
) bool {
	extra := map[string]any{"owner": sn.OwnerSubject}
	if boot {
		s.noteADOBootFailure(r.Context(), claims.RunID, adoOwnPATExpiredRefusal)
		extra["phase"] = adoResolvePhaseBoot
	}
	if boot || s.cfg.Approvals == nil {
		return fail(http.StatusForbidden, reasonADOOwnPATExpired, adoOwnPATExpiredRefusal, extra)
	}
	return s.holdForADOSignIn(w, r, claims, sn, ADOEntraFailure(reasonADOOwnPATExpired), fail)
}
