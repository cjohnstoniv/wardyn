// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The per-person Azure DevOps injection RESOLVE — the control-plane half of the
// lane authorADOEntraInjection authors at dispatch (runs_dispatch_ado_inject.go).
//
// A separate file from handleInternalInjection for resolveAWSSSOInjection's
// reason: this arm re-derives an identity, compares it against a snapshot and
// redeems a rotating credential, none of which the generic sink does.
//
// THE RULE IT ENFORCES is the AWS lane's, restated for a forge: a resolve may
// refresh credential MATERIAL; it must never re-authorize the run against a
// different person, provider row, organisation, tenant, application, token mode
// or capability set than the one it was dispatched with.
//
// WHAT IT DOES NOT CLAIM. The access token it returns carries every scope the
// person consented to, whatever this run was granted (measured; see
// RedeemADOEntraAccess). It is not narrowed and nothing here says it is. What
// holds the run to its capabilities is the proxy's capability check; what this
// file records is the GRANTED scope string the authority reported, because a
// token is opaque, must never be parsed, and that string is the only honest
// evidence of what the credential can do.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The refusal bodies. Machine-facing (the caller is the proxy sidecar, which
// fails its boot closed on any of them), but each names the one thing a person
// reading the run's failure could do about it.
//
// DRAFT (M2 canon pending)
const (
	adoResolveScopeChangedRefusal = "this run's Azure DevOps credential is no longer the one it was dispatched with — " +
		"the provider row changed while the run was working, and Wardyn will not resolve a different credential for a " +
		"run already in flight. Relaunch the run."
	adoResolveHostPinRefusal    = "the Azure DevOps access token may only be injected to this run's own organisation's hosts"
	adoResolveUnconfigured      = "this deployment offers no Azure DevOps sign-in, so no Azure DevOps credential can be resolved"
	adoResolveRosterUnreadable  = "could not read this deployment's Azure DevOps provider configuration"
	adoResolveNotCaptured       = "the person who launched this run has not connected Azure DevOps — sign in to Azure DevOps from the console, then relaunch"
	adoResolveDeadCredential    = "the Azure DevOps sign-in behind this run can no longer be renewed — sign in to Azure DevOps again, then relaunch"
	adoResolveConsentRequired   = "Azure DevOps has not been consented for the access this run was granted — an administrator or the person must grant consent, then relaunch"
	adoResolveInteractionNeeded = "Azure DevOps requires the person to sign in interactively (a Conditional Access policy) — sign in to Azure DevOps again, then relaunch"
	adoResolveUnavailable       = "renewing the Azure DevOps sign-in behind this run did not complete; nothing about the credential is known to be wrong"
	adoResolveTokenModeRefusal  = "this run's Azure DevOps token mode cannot be issued by Wardyn"
)

// adoEntraAccessReuseMargin is how long before expiry a minted access token
// stops being handed out again. It must EXCEED the proxy's own refresh margin
// (injectRefreshMargin, 5 min): a token returned inside that margin would send
// the sidecar straight back here on its next request, for the same token, until
// it expired.
const adoEntraAccessReuseMargin = 10 * time.Minute

// adoEntraAccessCache holds the last access token minted per person, row,
// application and requested scope set.
//
// It exists because one run's sidecar resolves ONE GRANT PER HOST (a dozen of
// them) at boot and again near every expiry, and every uncached resolve is a
// redemption that ROTATES the person's refresh token. A dozen rotations of one
// person's credential in a second is a dozen chances for a persist to fail
// after the authority has already spent the old token. The token is already
// masked process-wide by RedeemADOEntraAccess, so holding it here adds no new
// rendering to protect.
//
// Process-local, and correct for the same reason every other in-memory bound in
// this package is: more than one replica is refused by construction.
type adoEntraAccessCache struct {
	mu sync.Mutex
	m  map[string]ADOEntraAccess
	// refused is when a consent_required answer was last given per (owner,
	// row, needed scopes) — adoConsentRefusedRecently's negative cache.
	refused map[string]time.Time
}

func (c *adoEntraAccessCache) get(key string, now time.Time) (ADOEntraAccess, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.m[key]
	if !ok || !a.ExpiresAt.After(now.Add(adoEntraAccessReuseMargin)) {
		return ADOEntraAccess{}, false
	}
	return a, true
}

func (c *adoEntraAccessCache) put(key string, a ADOEntraAccess) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]ADOEntraAccess{}
	}
	c.m[key] = a
}

// adoEntraAccessFor returns a live access token for owner, redeeming only when
// no cached one has comfortably more life left than the proxy's own margin.
//
// fresh skips the cache: a cached token predates any sign-in since, so a
// caller asking whether the person has NOW consented to something must redeem.
func (s *Server) adoEntraAccessFor(ctx context.Context, cfg ADOEntraConfig, owner string, scopes []string, fresh bool) (ADOEntraAccess, error) {
	key := strings.Join([]string{owner, cfg.RowID, cfg.TenantID, cfg.ClientID, strings.Join(scopes, " ")}, "\x00")
	if a, ok := s.adoEntraTokens.get(key, s.cfg.Now()); ok && !fresh {
		return a, nil
	}
	a, err := s.RedeemADOEntraAccess(ctx, cfg, owner, scopes)
	if err != nil {
		return ADOEntraAccess{}, err
	}
	s.adoEntraTokens.put(key, a)
	return a, nil
}

// adoRequestScopes is what a resolve asks the authority for: the row's
// ceiling narrowed to the scopes owner's stored sign-in was captured with.
// A ceiling an administrator widened after the capture must not fail the
// redemption whole (RedeemADOEntraAccess refuses a scope outside the capture);
// a capability that needs the new scope goes through the consent chain. With
// no stored sign-in, or no overlap, the ceiling is asked for unchanged and the
// redemption names the failure.
func (s *Server) adoRequestScopes(ctx context.Context, cfg ADOEntraConfig, owner string) []string {
	blob, found, err := s.readADOEntraBlob(ctx, owner, cfg.RowID)
	if err != nil || !found {
		return cfg.Scopes
	}
	var out []string
	for _, sc := range cfg.Scopes {
		if slices.Contains(blob.Scopes, sc) {
			out = append(out, sc)
		}
	}
	if len(out) == 0 {
		return cfg.Scopes
	}
	return out
}

// resolveADOInjection is the per-person Azure DevOps arm of
// handleInternalInjection. handled=false means this grant is not ours and the
// generic path must run; handled=true means a response has been written.
//
// The order is the order the checks MUST run in:
//
//  1. the grant's own dispatch-time SNAPSHOT, read from the grant (never the
//     minted rule, which carries no identity);
//  2. the snapshot's owner must be the run token's own subject — a grant naming
//     anyone else, however it was authored, resolves nothing;
//  3. every snapshot field re-derived from the LIVE provider row and required
//     equal;
//  4. the host pinned to the snapshot's own organisation's host set;
//  5. the app registration the redemption would use required to be the one the
//     snapshot names;
//  6. redeem, classify any failure, and record the GRANTED scope string.
func (s *Server) resolveADOInjection(w http.ResponseWriter, r *http.Request,
	claims *identity.Claims, minted broker.Minted, grantID uuid.UUID,
) bool {
	if minted.Injection == nil || minted.Injection.SecretName != types.ADOEntraAccessTokenSecret {
		return false
	}
	ctx := r.Context()
	fail := func(status int, reason, body string, extra map[string]any) bool {
		data := map[string]any{"reason": reason, "grant_id": grantID}
		for k, v := range extra {
			data[k] = v
		}
		s.recordAudit(ctx, s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
			"secret.read", types.ADOEntraAccessTokenSecret, "failure", mustJSON(data)))
		// reason reaches the wire now (#204): the same machine class already
		// recorded on the audit row, so the proxy can branch on it instead of
		// string-matching the human sentence in body.
		writeErrorReason(w, status, reason, body)
		return true
	}

	snapshot, ok := s.adoEntraGrantSnapshot(ctx, claims.RunID, grantID)
	if !ok {
		// A grant naming this sentinel with no snapshot was hand-authored (a
		// policy, an inline grant). The snapshot IS the authorization.
		return fail(http.StatusForbidden, reasonMissingScopeSnapshot, adoResolveScopeChangedRefusal, nil)
	}
	if snapshot.OwnerSubject != claims.Sub {
		return fail(http.StatusForbidden, reasonOwnerNotCaller, adoResolveScopeChangedRefusal,
			map[string]any{"owner": snapshot.OwnerSubject})
	}
	siteCfg, scErr := s.cfg.Store.GetSiteConfig(ctx)
	if scErr != nil {
		// Never resolve against a read that failed: the zero SiteConfig is "no
		// rows", and a decision made from it is a decision about nothing.
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
	cfg, status, reason, body := s.adoEntraConfigFor(ctx, snapshot)
	if status != 0 {
		return fail(status, reason, body, nil)
	}
	if _, serr := adoscope.ScopesFor(snapshot.Capabilities); serr != nil {
		return fail(http.StatusForbidden, reasonCapabilityNotGrantable, adoResolveScopeChangedRefusal, nil)
	}

	// THE CAPABILITY ARM (injection_ado_capability.go): the proxy asking for
	// access this run was not dispatched with. It either answers here (403/423)
	// or names what this resolve grants; the consent that grant needs is checked
	// against the authority's answer below.
	responseCaps := snapshot.Capabilities
	var capAsk adoCapabilityGrant
	var need []string
	if r.URL.Query().Get("capability") != "" {
		var ok bool
		if capAsk, ok = s.answerADOCapability(w, r, claims, snapshot, siteCfg, grantID, fail); !ok {
			return true
		}
		// Cannot fail: answerADOCapability admits grantable capabilities only.
		need, _ = adoscope.ScopesFor([]adoscope.Capability{capAsk.capability})
		responseCaps = capAsk.standing
	}

	// REQUEST WHAT THE PERSON CONSENTED TO, inside the row's ceiling
	// (adoRequestScopes), not a subset computed from this run's capabilities.
	// Two measured facts decide it: Entra ignores a narrower request for this
	// resource and returns every consented scope anyway, so a subset buys
	// nothing; and a request naming even ONE scope the person has not
	// consented to fails whole (AADSTS65001, classified consent_required) with
	// no token at all — so asking for the whole ceiling after an administrator
	// widened it would fail every run of everyone who signed in before. The
	// run's capabilities bound it at the proxy, not here; a capability outside
	// the consent goes through the consent chain below.
	if capAsk.capability != "" && s.adoConsentRefusedRecently(ctx, cfg, snapshot.OwnerSubject, need) {
		return s.raiseADOConsent(w, r, claims, snapshot, need, fail)
	}
	scopes := s.adoRequestScopes(ctx, cfg, snapshot.OwnerSubject)
	access, err := s.adoEntraAccessFor(ctx, cfg, snapshot.OwnerSubject, scopes, false)
	if err == nil && capAsk.capability != "" && !adoScopesWithin(need, access.Scopes) {
		access, err = s.adoEntraAccessFor(ctx, cfg, snapshot.OwnerSubject, scopes, true)
	}
	if err != nil {
		class := ADOEntraClassify(err)
		if capAsk.capability != "" && class == ADOEntraFailureConsentRequired {
			s.adoConsentRefused(cfg, snapshot.OwnerSubject, need)
			return s.raiseADOConsent(w, r, claims, snapshot, need, fail)
		}
		if s.answerADOSignInEnded(w, r, claims, snapshot, class, fail) {
			return true
		}
		status, body := adoResolveFailureAnswer(class)
		return fail(status, string(class), body, map[string]any{"owner": snapshot.OwnerSubject})
	}
	if capAsk.capability != "" && s.settleADOCapability(w, r, claims, snapshot, cfg, grantID, minted.JTI, capAsk, need, access, fail) {
		return true
	}

	// The ONE wire shape, forced whatever the grant authored — the subscription
	// sentinel's rule: a crossed-wire grant must not put a live bearer in some
	// other header.
	value := formatInjectionValue(adoEntraInjectFormat, []byte(access.AccessToken))
	if s.cfg.MaskRegistry != nil {
		s.cfg.MaskRegistry.Add(claims.RunID, []byte(access.AccessToken))
		s.cfg.MaskRegistry.Add(claims.RunID, []byte(value))
	}
	data := map[string]any{
		"purpose": "proxy-injection-ado", "grant_id": grantID, "jti": minted.JTI,
		"owner": snapshot.OwnerSubject, "provider_row": snapshot.ProviderRowID,
		"organisation": snapshot.Organisation, "capabilities": responseCaps,
		// What the AUTHORITY said the token carries — not what was asked
		// for, and not a claim read out of the token.
		"granted_scope": strings.Join(access.Scopes, " "),
	}
	if capAsk.capability != "" {
		data["capability"] = capAsk.capability
		if capAsk.once != nil {
			// The join "approval X let request Z through": Z's jti is now
			// the approval row's minted_jti.
			data["approval_id"] = capAsk.once.ID
		}
	}
	s.recordAudit(ctx, s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
		"secret.read", types.ADOEntraAccessTokenSecret, "success", mustJSON(data)))
	writeJSON(w, http.StatusOK, injectionResponse{
		Host:      minted.Injection.Host,
		Header:    adoEntraInjectHeader,
		Value:     value,
		JTI:       minted.JTI,
		ExpiresAt: access.ExpiresAt.UnixMilli(),
		// Informational only: the proxy's gate pins the organisation from the
		// dispatch-time ADOGrants in its own configuration, not from this.
		Organisation: snapshot.Organisation,
		// Not the gate's input either (dispatch-time ADOGrants are); the hold
		// reads it only to confirm a capability ask came back granted.
		Capabilities: adoCapabilityStrings(responseCaps),
	})
	return true
}

// adoResolveFailureAnswer maps a redemption failure class onto its answer. Each
// class answers differently because each has a different remedy. The two a new
// sign-in fixes are answered first by answerADOSignInEnded (a hold mid-run, a
// failure hint at boot); their rows here are the sentences that answer uses.
func adoResolveFailureAnswer(class ADOEntraFailure) (int, string) {
	switch class {
	case ADOEntraFailureNotCaptured:
		return http.StatusForbidden, adoResolveNotCaptured
	case ADOEntraFailureDeadCredential:
		return http.StatusForbidden, adoResolveDeadCredential
	case ADOEntraFailureConsentRequired:
		return http.StatusForbidden, adoResolveConsentRequired
	case ADOEntraFailureInteractionRequired:
		return http.StatusForbidden, adoResolveInteractionNeeded
	default:
		return http.StatusServiceUnavailable, adoResolveUnavailable
	}
}

// adoEntraConfigFor resolves the app registration a redemption would use and
// holds it to the snapshot. status==0 means cfg is usable; otherwise the three
// strings are the refusal.
func (s *Server) adoEntraConfigFor(ctx context.Context, sn adoEntraScopeSnapshot) (ADOEntraConfig, int, string, string) {
	if types.ADOTokenMode(sn.TokenMode) != types.ADOTokenModeBearer {
		return ADOEntraConfig{}, http.StatusForbidden, reasonTokenMode, adoResolveTokenModeRefusal
	}
	if s.cfg.ADOEntra == nil {
		return ADOEntraConfig{}, http.StatusForbidden, reasonSigninUnconfigured, adoResolveUnconfigured
	}
	cfg, found, err := s.cfg.ADOEntra(ctx)
	switch {
	case err != nil:
		return ADOEntraConfig{}, http.StatusServiceUnavailable, reasonSigninUnreadable, adoResolveRosterUnreadable
	case !found:
		return ADOEntraConfig{}, http.StatusForbidden, reasonSigninUnconfigured, adoResolveUnconfigured
	case cfg.RowID != sn.ProviderRowID || !strings.EqualFold(cfg.TenantID, sn.TenantID) ||
		!strings.EqualFold(cfg.ClientID, sn.ClientID):
		return ADOEntraConfig{}, http.StatusForbidden, reasonScopeChanged, adoResolveScopeChangedRefusal
	}
	return cfg, 0, "", ""
}

// adoEntraGrantSnapshot reads the dispatch-time snapshot off the grant itself,
// through the RUN's own grant list — which re-proves run binding independently
// of the broker's own check (awsSSOGrantSnapshot's rule).
func (s *Server) adoEntraGrantSnapshot(ctx context.Context, runID, grantID uuid.UUID) (adoEntraScopeSnapshot, bool) {
	grants, err := s.cfg.Store.ListGrantsByRun(ctx, runID)
	if err != nil {
		return adoEntraScopeSnapshot{}, false
	}
	for _, g := range grants {
		if g.ID != grantID {
			continue
		}
		var sc struct {
			Snapshot adoEntraScopeSnapshot `json:"snapshot"`
		}
		if json.Unmarshal(g.Spec.Scope, &sc) != nil || !sc.Snapshot.authored() {
			return adoEntraScopeSnapshot{}, false
		}
		return sc.Snapshot, true
	}
	return adoEntraScopeSnapshot{}, false
}

// driftFrom compares the snapshot against the LIVE provider row, returning the
// name of the first field that moved ("" = equal). The name rides the audit row
// only; the caller's body is deliberately vague, because the reader of the body
// is a sidecar and the field names are identity.
func (sn adoEntraScopeSnapshot) driftFrom(sc types.SiteConfig) string {
	row, found := gitProviderRowByID(sc, sn.ProviderRowID)
	switch {
	case !found || row.Disabled:
		return "row_withdrawn"
	case row.Kind != types.GitProviderAzureDevOps:
		return "provider_kind"
	case !laneAllowed(row, types.GitLaneEntra) || row.Entra == nil:
		return "lane_withdrawn"
	case string(row.CredentialSource) != sn.CredentialSource:
		return "credential_source"
	case row.Entra.TenantID != sn.TenantID:
		return "tenant_id"
	case row.Entra.ClientID != sn.ClientID:
		return "client_id"
	case string(cmpTokenMode(row.Entra.TokenMode)) != sn.TokenMode:
		// A field-drift label for the audit row's "drift" key, not the wire
		// reason enum — reasonTokenMode is a different concept that happens
		// to share this exact spelling; driftFrom names WHICH field drifted,
		// never why the resolve was refused.
		return "token_mode"
	case !rowServesOrganisation(row, sn.Organisation):
		return "organisation"
	case !adoCapabilitiesWithin(sn.Capabilities, row.Entra.CapabilityCeiling):
		return "capability_ceiling"
	}
	return ""
}

// cmpTokenMode is the ONE reading of an unset token mode: bearer. It is the
// same reading dispatch applied when it wrote the snapshot, so an unset field
// on both sides compares equal.
func cmpTokenMode(m types.ADOTokenMode) types.ADOTokenMode {
	if m == "" {
		return types.ADOTokenModeBearer
	}
	return m
}

// gitProviderRowByID finds a git provider row by its operator-chosen id.
func gitProviderRowByID(sc types.SiteConfig, id string) (types.GitProvider, bool) {
	for _, row := range gitProviderRows(sc) {
		if row.ID == id {
			return row, true
		}
	}
	return types.GitProvider{}, false
}

// rowServesOrganisation reports whether one of the row's base URLs still
// admits org — a bare dev.azure.com base URL admits every organisation on it,
// and anything else must name org itself.
func rowServesOrganisation(row types.GitProvider, org string) bool {
	for _, raw := range row.BaseURLs {
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		if strings.EqualFold(u.Hostname(), "dev.azure.com") && strings.Trim(u.Path, "/") == "" {
			return true
		}
		if got, ok := adoOrganisationOf(raw); ok && got == org {
			return true
		}
	}
	return false
}

// adoCapabilityStrings renders capabilities for the wire.
func adoCapabilityStrings(caps []adoscope.Capability) []string {
	out := make([]string, len(caps))
	for i, c := range caps {
		out[i] = string(c)
	}
	return out
}
