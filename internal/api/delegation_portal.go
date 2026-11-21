// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The exchange of an issuer portal: a portal that names its own identity provider (a row with an issuer,
// whose credential has the wdi_ prefix). It shares the endpoint, the portal authentication and the form with
// the console-issuer exchange in delegation_exchange.go and nothing after them: the person's token is
// verified by oidc.PortalIssuers against that portal's own registration, the person is always a user, and
// the grant is a portal_grants row the delegated lane reads (delegation.go).
//
// This file never calls VerifySubjectToken, PreviewRoleAgainst or ListRoleMappings, and reads no console
// role configuration: a portal person's role is a constant.

// PortalVerifier is what the exchange asks of the verifier set; *oidc.PortalIssuers is the production one.
type PortalVerifier interface {
	VerifyDetail(ctx context.Context, reg oidc.PortalRegistration, raw, declaredType string) (oidc.PortalSubject, oidc.PortalRefusal)
}

// portalIntroductionFinder is the store's read of one identity's own owed introduction, by the identity's id:
// the event with its fixed id and time, and false when none is owed. A store without it falls back to the
// oldest portalIntroductionBatch owed events across all identities, which misses the person's own behind a
// backlog of that size (the leaver sweep still delivers it).
type portalIntroductionFinder interface {
	PendingPortalIntroductionFor(ctx context.Context, identity uuid.UUID) (store.PortalIntroduction, bool, error)
}

// auditPresence is the read of the shared audit table that settles an owed event: which (time, id) pairs the
// table holds. store.PG has it; a store without it never settles an introduction, which stays owed.
type auditPresence interface {
	AuditEventsStored(ctx context.Context, want []store.AuditEventKey) (map[store.AuditEventKey]bool, error)
}

var _ auditPresence = store.PG{}

// portalIntroductionBatch bounds how many owed introduction events one exchange looks through for the
// identity it just touched. The portal leaver sweep delivers the rest.
const portalIntroductionBatch = 100

// PortalReservedCheck is the reserved-name predicate the portal verifier takes at boot: the same
// isReservedPrincipal every human door asks, for a server whose local operator is localOperator.
func PortalReservedCheck(localOperator string) func(string) bool {
	s := &Server{cfg: Config{LocalOperator: localOperator}}
	return s.isReservedPrincipal
}

// portalRegistration is the part of a portal row the verifier reads.
func portalRegistration(d types.Delegate) oidc.PortalRegistration {
	reg := oidc.PortalRegistration{
		Issuer: d.Issuer, InternalIssuer: d.InternalIssuer, TokenProfile: d.TokenProfile, ClientID: d.IdPClientID,
		Audience: d.Audience, RequiredScopes: d.RequiredScopes, Group: d.Group, GroupsClaim: d.GroupsClaim,
		EmailDomains: d.EmailDomains, MaxSubjectTokenAge: time.Duration(d.MaxSubjectTokenAgeSeconds) * time.Second,
	}
	if c := d.RequiredClaim; c != nil {
		reg.RequiredClaim, reg.RequiredClaimValue = c.Claim, c.Value
	}
	return reg
}

// portalRefusalStatus maps a verifier refusal's class to the exchange's answer. A request that is malformed
// is 400 invalid_request; a token that fails a check is 400 invalid_grant; a token the portal may not act for
// is 403 access_denied; an issuer outage is 503 temporarily_unavailable. An unknown class is a refusal of the
// token, never an admission.
func portalRefusalStatus(c oidc.PortalRefusalClass) (int, string) {
	switch c {
	case oidc.PortalRefusedRequest:
		return http.StatusBadRequest, "invalid_request"
	case oidc.PortalRefusedAdmission:
		return http.StatusForbidden, "access_denied"
	case oidc.PortalRefusedUnavailable:
		return http.StatusServiceUnavailable, "temporarily_unavailable"
	default:
		return http.StatusBadRequest, "invalid_grant"
	}
}

// portalGrantTTL is the longest a grant of d lives from its mint: grant_ttl_seconds, else the maximum token age.
func portalGrantTTL(d types.Delegate) time.Duration {
	secs := d.GrantTTLSeconds
	if secs <= 0 {
		secs = d.MaxSubjectTokenAgeSeconds
	}
	if secs <= 0 {
		secs = 300
	}
	return time.Duration(secs) * time.Second
}

// portalAdmissionAudit is the names-only account of how a portal admits people: whether it sets a group, the
// name of its claim match, and whether it admits on the token checks alone. It never carries a value.
func portalAdmissionAudit(d types.Delegate) map[string]any {
	var claim any
	if d.RequiredClaim != nil {
		claim = d.RequiredClaim.Claim
	}
	return map[string]any{"group": d.Group != "", "claim": claim, "portal_managed": d.PortalManagedAdmission}
}

// portalExchange is the exchange for an issuer portal d, which authenticateDelegate has just resolved live.
// Every refusal fails closed and mints nothing, and every decision past client authentication writes a
// delegation.exchange row naming the portal, the issuer and k, and never a token or a claim value.
func (s *Server) portalExchange(w http.ResponseWriter, r *http.Request, d types.Delegate, subjectToken, actorToken string, authorizedAt time.Time) {
	ctx := r.Context()
	// Every row of the exchange, denied or successful, names the settings that governed it: the exact issuer,
	// its key, the profile, the two freshness settings and the admission rule by names only (whether a group
	// is required, the portal-managed flag, the one claim's name), never a claim value or a group.
	base := map[string]any{
		"issuer": d.Issuer, "k": d.IssuerKey, "token_profile": d.TokenProfile,
		"max_subject_token_age_seconds": int(portalMaxAge(d) / time.Second), "grant_ttl_seconds": int(portalGrantTTL(d) / time.Second),
		"admission": portalAdmissionAudit(d),
	}
	deny := func(status int, code, target, reason string, extra map[string]any) {
		data := map[string]any{"reason": reason}
		for k, v := range base {
			data[k] = v
		}
		for k, v := range extra {
			data[k] = v
		}
		s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, delegateActor(d.ID), "delegation.exchange", target, "denied", mustJSON(data)))
		writeOAuthError(w, status, code, "the subject token was refused")
	}
	unavailable := func(reason string, extra map[string]any) {
		deny(http.StatusServiceUnavailable, "temporarily_unavailable", "", reason, extra)
	}
	pgs, okG := s.cfg.Store.(store.PortalGrantStore)
	pis, okI := s.cfg.Store.(store.PortalIdentityStore)
	if !okG || !okI || s.cfg.PortalIssuers == nil {
		slog.ErrorContext(ctx, "api: an issuer portal exchanged, but this server cannot verify issuer portals")
		unavailable(reasonPortalUnavailable, nil)
		return
	}
	if actorToken != "" {
		deny(http.StatusBadRequest, "invalid_request", "", reasonActorTokenUnsupported, nil)
		return
	}
	if strings.HasPrefix(subjectToken, delegatedTokenPrefix) {
		deny(http.StatusBadRequest, "invalid_grant", "", "subject_token_delegated", nil)
		return
	}
	// The issuer this process signs people in with is never also a portal's. Registration and boot refuse the
	// overlap; this closes the window between a replica's boot check and its first request.
	if s.cfg.OIDC != nil && types.NormIssuerURL(s.cfg.OIDC.Issuer()) == types.NormIssuerURL(d.Issuer) {
		unavailable(reasonPortalIssuerConflict, nil)
		return
	}
	subj, refusal := s.cfg.PortalIssuers.VerifyDetail(ctx, portalRegistration(d), subjectToken, r.PostForm.Get("subject_token_type"))
	if !refusal.OK() {
		status, code := portalRefusalStatus(refusal.Class)
		extra := map[string]any{}
		switch {
		case refusal.Cause != "":
			extra["cause"] = refusal.Cause
		case refusal.Reason == oidc.SubjectTokenClaimMismatch && d.RequiredClaim != nil:
			extra["claim"] = d.RequiredClaim.Claim
		}
		if refusal.Reason == oidc.PortalRegistrationInvalid {
			slog.ErrorContext(ctx, "api: an issuer portal's own registration cannot be verified against", "portal", d.ID, "reason", reasonPortalRegistrationInvalid)
		}
		if refusal.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int((refusal.RetryAfter+time.Second-1)/time.Second)))
		}
		deny(status, code, "", refusal.Reason, extra)
		return
	}
	// A person never takes the name of the local operator's seat, whatever shape the operator was given.
	if s.isLocalOperatorName(subj.Principal) {
		deny(http.StatusBadRequest, "invalid_grant", "", oidc.DenialReservedPrincipal, nil)
		return
	}
	// The revocation sequence is read before the person's cutoff is checked, as the console exchange does,
	// so a suspension that lands between the identity's commit and the grant's still ends the grant.
	issue, err := s.mintIssue(ctx, subj.Principal, authorizedAt)
	if err != nil {
		slog.ErrorContext(ctx, "api: revocation order lookup failed; portal exchange refused", "error", err)
		s.metrics.authStoreErrorInc()
		writeOAuthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "session revocation lookup failed")
		return
	}
	if s.cfg.SessionRevocations != nil {
		revoked, err := s.cfg.SessionRevocations.IsSessionRevoked(ctx, subj.Principal, "", subj.IssuedAt)
		if err != nil {
			slog.ErrorContext(ctx, "api: session-revocation lookup failed; portal exchange refused", "error", err)
			s.metrics.authStoreErrorInc()
			writeOAuthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "session revocation lookup failed")
			return
		}
		if revoked {
			deny(http.StatusBadRequest, "invalid_grant", subj.Principal, "session_revoked", nil)
			return
		}
	}
	// The user type is the portal's own, never a claim, and must still exist.
	exists, err := s.userTypeExists(ctx, d.UserType)
	if err != nil {
		slog.ErrorContext(ctx, "api: user type lookup failed; portal exchange refused", "error", err)
		s.metrics.authStoreErrorInc()
		writeOAuthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "user type lookup failed")
		return
	}
	if !exists {
		deny(http.StatusBadRequest, "invalid_grant", subj.Principal, "user_type_unknown", nil)
		return
	}
	// A display value for the People page only: the email claim of an ID token, the login name of an access
	// token. Nothing authorises by it and no audit row carries it.
	display := subj.Sub
	if d.TokenProfile == oidc.PortalProfileIDToken {
		display = subj.Email
	}
	identity, _, err := pis.IssuePortalIdentity(ctx, subj.Principal, subj.K, display, d.Issuer, d.ID, d.AdmissionRev)
	if s.portalWriteRefused(w, r, err, "issue portal identity", subj.Principal, deny) {
		return
	}
	s.deliverPortalIntroductions(ctx, pis, identity)
	// The person's record under this portal's id, kept empty: groups and email never reach authorization.
	if pw, ok := s.cfg.Store.(principalGroupsWriter); ok {
		if err := pw.PutPrincipalGroups(ctx, subj.Principal, d.ID.String(), "", nil, false, subj.IssuedAt); err != nil {
			slog.ErrorContext(ctx, "api: record the portal exchange failed; portal exchange refused", "error", err)
			s.metrics.authStoreErrorInc()
			writeOAuthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "sign-in record write failed")
			return
		}
	}
	raw := newBearer(delegatedTokenPrefix)
	grant, dbnow, err := pgs.MintPortalGrant(ctx, types.PortalGrant{
		ID: uuid.New(), DelegateID: d.ID, Principal: subj.Principal, UserType: d.UserType, SubjectIssuedAt: subj.IssuedAt, RevSeq: issue.Seq,
	}, raw, subj.GrantEnd, portalGrantTTL(d), d.AdmissionRev)
	if errors.Is(err, store.ErrSubjectTokenStale) {
		deny(http.StatusBadRequest, "invalid_grant", subj.Principal, oidc.SubjectTokenStale, nil)
		return
	}
	if s.portalWriteRefused(w, r, err, "mint portal grant", subj.Principal, deny) {
		return
	}
	// After the commit, so two people's mints never wait on each other: the portal's last use, at most
	// once a minute, and the sweep of ended grants. Either failing leaves the exchange as it is.
	if err := pgs.StampPortalUsed(ctx, d.ID, dbnow); err != nil {
		slog.WarnContext(ctx, "api: stamp the portal's last use failed", "error", err)
	}
	if _, err := pgs.SweepExpiredPortalGrants(ctx, dbnow); err != nil {
		slog.WarnContext(ctx, "api: sweep ended portal grants failed", "error", err)
	}
	data := map[string]any{
		"grant": grant.ID, "expires_at": grant.ExpiresAt, "user_type": grant.UserType, "role": oidc.RoleUser,
	}
	for k, v := range base {
		data[k] = v
	}
	s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, delegateActor(d.ID), "delegation.exchange", grant.Principal, "success", mustJSON(data)))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":      raw,
		"issued_token_type": tokenTypeAccessToken,
		"token_type":        "Bearer",
		// The grant's remaining life, never a constant: it ends by the token's own cutoff.
		"expires_in": int(grant.ExpiresAt.Sub(dbnow) / time.Second),
	})
}

// portalMaxAge is the portal's maximum subject-token age, 300 s when the row names none.
func portalMaxAge(d types.Delegate) time.Duration {
	if d.MaxSubjectTokenAgeSeconds <= 0 {
		return 300 * time.Second
	}
	return time.Duration(d.MaxSubjectTokenAgeSeconds) * time.Second
}

// portalWriteRefused answers the failure of one of the exchange's two writes and reports whether it did. A
// portal revoked or an issuer binding retired since the token was checked is the revoked portal's answer,
// 401 invalid_client, and writes nothing; an admission rule changed since is 503 portal_admission_changed
// and the portal retries under the new rule; a deactivated or purged person is refused as the console
// refuses one. Anything else is a failed write: 500, nothing minted.
func (s *Server) portalWriteRefused(w http.ResponseWriter, r *http.Request, err error, what, principal string, deny func(int, string, string, string, map[string]any)) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrPortalGone), errors.Is(err, store.ErrNotFound):
		s.auditAuthFailedAs(r, delegationAuthActor, "invalid_client_credential")
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "portal authentication failed")
	case errors.Is(err, store.ErrAdmissionChanged):
		deny(http.StatusServiceUnavailable, "temporarily_unavailable", principal, reasonPortalAdmissionChanged, nil)
	case errors.Is(err, store.ErrIdentityDeactivated), errors.Is(err, store.ErrPortalPersonPurged):
		s.auditAuthFailedAs(r, oidcCallbackActor, authFailedIdentityDeactivated)
		deny(http.StatusBadRequest, "invalid_grant", principal, oidc.DenialIdentityDeactivated, nil)
	default:
		slog.ErrorContext(r.Context(), "api: portal exchange write failed", "step", what, "error", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", what+" failed")
	}
	return true
}

// deliverPortalIntroductions delivers the introduction event of identity, if it is still owed. The event's
// id and time were fixed when the identity row was inserted, so every delivery, by this exchange, a later one
// through any portal, or the leaver sweep, is a replay of one event and the audit table keeps one row.
func (s *Server) deliverPortalIntroductions(ctx context.Context, ps store.PortalIdentityStore, identity uuid.UUID) {
	if f, ok := ps.(portalIntroductionFinder); ok {
		p, owed, err := f.PendingPortalIntroductionFor(ctx, identity)
		if err != nil {
			slog.WarnContext(ctx, "api: read this person's owed portal introduction failed", "error", err)
			return
		}
		if owed {
			s.deliverPortalIntroduction(ctx, ps, p)
		}
		return
	}
	owed, err := ps.PendingPortalIntroductions(ctx, portalIntroductionBatch)
	if err != nil {
		slog.WarnContext(ctx, "api: read owed portal introductions failed", "error", err)
		return
	}
	for _, p := range owed {
		if p.IdentityID == identity {
			s.deliverPortalIntroduction(ctx, ps, p)
		}
	}
}

// deliverPortalIntroduction appends one owed introduction as person.create and marks it delivered once the
// shared audit table holds it. The data is read from the identity row alone, never recomputed, and carries no
// email in any form. A recorder's acceptance is not enough: an event that only reached this replica's local
// spool is lost with the replica, so the mark waits until a read of the shared table finds the event's id with
// its recorded time. Until then it stays owed, and every later delivery replays it; the replay is keyed by
// that id and time, so the table keeps one row.
func (s *Server) deliverPortalIntroduction(ctx context.Context, ps store.PortalIdentityStore, p store.PortalIntroduction) {
	ev := s.auditEvent(nil, types.ActorSystem, delegateActor(p.PortalID), "person.create", p.Principal, "success", mustJSON(map[string]any{
		"source": "portal", "portal": p.PortalID, "issuer": p.Issuer, "k": p.IssuerKey, "identity": p.IdentityID,
	}))
	ev.ID, ev.Time = p.EventID, p.EventAt
	if !s.recordAuditDurably(store.WithAuditReplay(ctx), ev) {
		return
	}
	ap, ok := ps.(auditPresence)
	if !ok {
		return
	}
	key := store.AuditEventKey{ID: p.EventID, At: p.EventAt.UTC().Truncate(time.Microsecond)}
	stored, err := ap.AuditEventsStored(ctx, []store.AuditEventKey{key})
	if err != nil {
		slog.WarnContext(ctx, "api: read whether a portal introduction is stored failed; it stays owed", "error", err)
		return
	}
	if !stored[key] {
		slog.WarnContext(ctx, "api: a portal introduction is not in the shared audit table yet; it stays owed", "identity", p.IdentityID)
		return
	}
	if _, err := ps.MarkPortalIntroductionAudited(ctx, p.IdentityID); err != nil {
		slog.WarnContext(ctx, "api: mark a portal introduction delivered failed", "error", err)
	}
}

// portalGrantPrincipalOK is the reverse check: a portal grant's principal must be one of its own portal's
// people, delegate:person:<issuer key>:<e>. A grant row that pairs a portal with any other principal (a console
// sub, another portal's person, an empty string) can only come from a corrupt or attacked store, and is
// refused as if the grant were unknown.
func portalGrantPrincipalOK(g types.PortalGrant) bool {
	if g.IssuerKey == "" {
		return false
	}
	prefix := types.PortalPersonPrefix + g.IssuerKey + ":"
	return len(g.Principal) > len(prefix) && strings.HasPrefix(g.Principal, prefix)
}

// PortalRefreshFailures counts failed refreshes of a portal issuer's discovery or keys, by issuer key, the portals registered on
// it, source and coarse cause. It is the portal verifier's OnRefreshFailure target and renders on /metrics.
type PortalRefreshFailures struct {
	// Portals names the live portals registered on an issuer at the moment of a failure, by delegate id: the
	// attribution two portals on one issuer share. nil, or an empty answer, attributes to none. A delegate
	// id is a bounded, non-person label; nothing about a person, a token or a credential is ever one.
	Portals func(issuer string) []string

	mu     sync.Mutex
	counts map[[4]string]int64
}

// Record counts one failed refresh. An issuer is labelled by its key, which is bounded and reveals no address.
func (p *PortalRefreshFailures) Record(f oidc.PortalRefreshFailure) {
	if p == nil {
		return
	}
	var portals []string
	if p.Portals != nil {
		portals = p.Portals(f.Issuer)
	}
	if len(portals) == 0 {
		portals = []string{""}
	}
	key := oidc.PortalIssuerKey(f.Issuer)
	slog.Warn("api: issuer portal refresh failed", "issuer_key", key, "portals", portals, "source", f.Source, "cause", f.Cause)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.counts == nil {
		p.counts = map[[4]string]int64{}
	}
	for _, id := range portals {
		p.counts[[4]string{key, id, f.Source, f.Cause}]++
	}
}

// write appends the counter in the exposition format. A counter nothing has recorded renders only its header.
func (p *PortalRefreshFailures) write(w io.Writer) {
	fmt.Fprint(w, "# HELP wardyn_portal_issuer_refresh_failures_total Failed refreshes of an issuer portal's discovery document or keys, by issuer key, portal (delegate id), source (scheduled or unknown_kid) and coarse cause. While one is failing the issuer's exchanges answer 503.\n"+
		"# TYPE wardyn_portal_issuer_refresh_failures_total counter\n")
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	keys := make([][4]string, 0, len(p.counts))
	for k := range p.counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		for n := range keys[i] {
			if keys[i][n] != keys[j][n] {
				return keys[i][n] < keys[j][n]
			}
		}
		return false
	})
	for _, k := range keys {
		fmt.Fprintf(w, "wardyn_portal_issuer_refresh_failures_total{issuer_key=%q,portal=%q,source=%q,cause=%q} %d\n", k[0], k[1], k[2], k[3], p.counts[k])
	}
}

// writePortalRulingError answers 422 for the two rulings of the store's writes of admin-written person lists
// (a portal person on a list that confers a role, or on one whose issuer is not bound) and reports whether it
// did. writeServerError, the one presenter every store failure of a handler goes through, calls it, so the
// answer is one: 422 delegate_person_not_allowed or 422 delegate_person_unknown_portal. A presenter of a
// held change's decision that does not go through writeServerError calls it directly.
func writePortalRulingError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, store.ErrDelegatePersonNotAllowed):
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonDelegatePersonNotAllowed, "a portal person cannot hold this role")
	case errors.Is(err, store.ErrDelegatePersonUnknownPortal):
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonDelegatePersonUnknownPortal, "no issuer portal is registered for this person's issuer")
	default:
		return false
	}
	return true
}

// isLocalOperatorName reports whether name is the configured local operator's seat, compared as
// isReservedPrincipal compares it. The operator's name is protected whatever its spelling looks like, a
// portal person's included.
func (s *Server) isLocalOperatorName(name string) bool {
	op := strings.ToLower(strings.TrimSpace(s.cfg.LocalOperator))
	return op != "" && strings.ToLower(strings.TrimSpace(name)) == op
}

// isPortalPersonName reports whether name is a well-formed portal person, delegate:person:<k>:<e>, with k
// the 26 characters of an issuer key and e a non-empty lowercase base32 person id, after the fold the user
// path applies. Anything else that starts delegate: stays reserved.
func isPortalPersonName(name string) bool {
	p := strings.ToLower(strings.TrimSpace(name))
	rest, ok := strings.CutPrefix(p, types.PortalPersonPrefix)
	if !ok || len(p) > 248 {
		return false
	}
	k, e, ok := strings.Cut(rest, ":")
	if !ok || len(k) != 26 || e == "" {
		return false
	}
	for _, part := range []string{k, e} {
		for i := 0; i < len(part); i++ {
			if c := part[i]; !(c >= 'a' && c <= 'z') && !(c >= '2' && c <= '7') {
				return false
			}
		}
	}
	return true
}

// servePortalGrant is the delegated lane's branch for a grant held in portal_grants. It reports whether it
// answered the request (admitted it to next, refused it, or failed closed on a store error); false means the
// bearer is no portal grant and the lane goes on to the legacy table. An unknown, ended or cut-off grant, one
// whose portal was revoked, and one whose principal is not its portal's person fall through to fallback as an
// unrecognised bearer does, so the single 401 gives no oracle. The person is published at the user role
// whatever any console configuration says, with no email and no groups.
func (s *Server) servePortalGrant(w http.ResponseWriter, r *http.Request, tok string, next, fallback http.Handler) bool {
	pgs, ok := s.cfg.Store.(store.PortalGrantStore)
	if !ok {
		return false
	}
	g, err := pgs.GetPortalGrantByRaw(r.Context(), tok, s.cfg.Now().UTC())
	if errors.Is(err, store.ErrNotFound) {
		return false
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "api: portal grant lookup failed; this request could not be authenticated",
			"error", err, "path", r.URL.Path)
		s.metrics.authStoreErrorInc()
		writeErrorReason(w, http.StatusServiceUnavailable, reasonTokenLookupUnavailable, "delegated token lookup failed")
		return true
	}
	if !portalGrantPrincipalOK(g) {
		slog.ErrorContext(r.Context(), "api: a portal grant names a principal outside its portal's key; refused",
			"grant", g.ID, "portal", g.DelegateID)
		fallback.ServeHTTP(w, r)
		return true
	}
	// The person's own cutoff and identity, at epoch -1 as every credential lane asks: a suspended, purged
	// or revoked person is refused at the next request.
	if s.cfg.SessionRevocations != nil {
		status, rerr := oidc.CheckSession(r.Context(), s.cfg.SessionRevocations, g.Principal, "", oidc.Issue{At: g.CreatedAt, Seq: g.RevSeq}, -1)
		if rerr != nil {
			slog.ErrorContext(r.Context(), "api: session-revocation lookup failed; this portal grant could not be authenticated",
				"error", rerr, "path", r.URL.Path)
			s.metrics.authStoreErrorInc()
			writeErrorReason(w, http.StatusServiceUnavailable, reasonTokenLookupUnavailable, "delegated token lookup failed")
			return true
		}
		if status != oidc.SessionLive {
			fallback.ServeHTTP(w, r)
			return true
		}
	}
	ctx := withHumanIdentity(oidc.WithRevocationSeq(r.Context(), g.Principal, g.RevSeq), g.Principal, "", oidc.RoleUser, g.UserType, []string{}, false)
	r = r.WithContext(audit.WithDelegation(ctx, types.DelegationVia{Delegate: g.DelegateID, Grant: g.ID}))
	if rctx := chi.RouteContext(r.Context()); rctx == nil || !delegationAllowed[r.Method+" "+rctx.RoutePattern()] {
		d := authz.Deny(authz.ReasonDelegationScope, r.URL.Path, delegationRefusal)
		writeErrorReason(w, d.Status, string(d.Reason), d.Sentence)
		s.recordRefusal(r.Context(), r, d)
		return true
	}
	next.ServeHTTP(w, r)
	return true
}

// portalGrantLive is delegationLive's branch for a portal grant: the grant resolved by id, unexpired and
// through a live portal that is the session's own, and whose principal is that portal's person (the reverse
// check). found is false when the id names no portal grant, and the caller reads the legacy table; a grant
// that is refused is found, with verdictDelegationEnded, so a portal grant never falls through to a lookup
// that could answer for it.
func (s *Server) portalGrantLive(ctx context.Context, via types.DelegationVia) (expires time.Time, v entryVerdict, found bool) {
	pgs, ok := s.cfg.Store.(store.PortalGrantStore)
	if !ok {
		return time.Time{}, verdictOK, false
	}
	g, err := pgs.GetPortalGrantByID(ctx, via.Grant, s.cfg.Now().UTC())
	if errors.Is(err, store.ErrNotFound) {
		return time.Time{}, verdictOK, false
	}
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: portal grant lookup failed", "grant", via.Grant, "err", err)
		return time.Time{}, verdictDelegationUnavailable, true
	}
	if g.DelegateID != via.Delegate {
		return time.Time{}, verdictDelegationEnded, true
	}
	if !portalGrantPrincipalOK(g) {
		slog.ErrorContext(ctx, "wardynd: a portal grant names a principal outside its portal's key; refused", "grant", g.ID, "portal", g.DelegateID)
		return time.Time{}, verdictDelegationEnded, true
	}
	return g.ExpiresAt, verdictOK, true
}
