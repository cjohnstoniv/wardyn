// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// inboundSubjectTokenInvalidMsg is the one body of every refused subject token, whatever failed (the audit row says
// which), so a caller learns nothing about which trust or check refused it.
const inboundSubjectTokenInvalidMsg = "The subject token was not accepted"

// inboundFederationOffMsg answers the exchange while the switch is off, the store keeps no trusts, or the server runs
// in local mode: one body for all three.
const inboundFederationOffMsg = "Inbound federation is off"

// inboundFederationNeedsPersonMsg refuses a widening write that no signed-in person made.
const inboundFederationNeedsPersonMsg = "Turning inbound federation on, adding a trust, confirming one and approving either need a signed-in person, never an API token"

// inboundTrustUnconfirmedMsg refuses an exchange under a trust whose sponsor was revoked after its last confirm.
const inboundTrustUnconfirmedMsg = "The sponsor's sessions were revoked after this trust was last confirmed; the sponsor must confirm it again"

// inboundTrustNotFoundMsg is the one body of a missing trust and of one on an identity the caller does not sponsor.
const inboundTrustNotFoundMsg = "Inbound trust not found"

// inboundSharedAudiences are audiences a provider hands to many services. A second check: the own-address rule
// (ownAudience) already refuses them unless this deployment's own address is one of them.
var inboundSharedAudiences = []string{
	"sts.amazonaws.com", "api://AzureADTokenExchange",
	"https://kubernetes.default.svc", "https://kubernetes.default.svc.cluster.local",
}

// inboundSharedAudiencePrefix is GitHub's default audience, the owner's URL.
const inboundSharedAudiencePrefix = "https://github.com/"

// inboundTrustTextMax bounds an audience, a subject and a claim value, in bytes; inboundTrustClaimMax a claim name.
// The table's checks hold the same bounds, so a trust that passes here never fails its INSERT.
const (
	inboundTrustTextMax  = 512
	inboundTrustClaimMax = 256
)

// inboundRegisteredClaims are the claims a trust's extra claim may never name: the verifier checks them itself.
var inboundRegisteredClaims = []string{"iss", "aud", "sub", "exp", "nbf", "iat", "jti"}

// mountInboundFederationRoutes registers inbound federation: the switch and the trusts on the security tier (an
// admin or a security admin), and the sponsor's own view, veto and confirm of an identity's trusts beside
// /me/service-identities. The anonymous exchange is mountInboundExchangeRoute.
func (s *Server) mountInboundFederationRoutes(r chi.Router, securityOps chi.Router) {
	securityOps.Get("/inbound-federation", s.handleGetInboundFederation)
	securityOps.Put("/inbound-federation", s.handlePutInboundFederation)
	securityOps.Post("/inbound-trusts", s.handleCreateInboundTrust)
	securityOps.Get("/inbound-trusts", s.handleListInboundTrusts)
	securityOps.Get("/inbound-trusts/{id}", s.handleGetInboundTrust)
	securityOps.Delete("/inbound-trusts/{id}", s.handleDeleteInboundTrust)
	r.Get("/me/service-identities/{name}/inbound-trusts", s.handleListSponsoredInboundTrusts)
	r.Delete("/me/service-identities/{name}/inbound-trusts/{id}", s.handleDeleteSponsoredInboundTrust)
	r.Post("/me/service-identities/{name}/inbound-trusts/{id}/confirm", s.handleConfirmInboundTrust)
}

// urlOrigin is raw's origin: the lower-cased scheme and host, an IPv6 host bracketed, and the port unless it is the
// scheme's default. Path, query and fragment are dropped. False for an empty or unparsable value, no host, or a
// scheme other than http or https.
func urlOrigin(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := u.Port(); port != "" && !(scheme == "https" && port == "443") && !(scheme == "http" && port == "80") {
		host = net.JoinHostPort(strings.Trim(host, "[]"), port)
	}
	return scheme + "://" + host, true
}

// ownOrigins are this deployment's public addresses: each origin, followed by the base path when one is set, since
// every service behind the same host shares the bare origin. A trust's audience must be one of them, so a provider
// token made for any other service never carries it (#3787). ControlPlaneURL is left out: it is the internal
// listener's address. UIAdvertiseURL is left out too: it is the UI-sandbox listener's, whose origin every run's apps
// share when no per-run origin is set, so a provider token made for an app in a run could carry it.
func (s *Server) ownOrigins() []string {
	var out []string
	for _, raw := range []string{s.cfg.RunnerOrgURL, s.cfg.OIDCRedirectURL} {
		o, ok := urlOrigin(raw)
		if !ok {
			continue
		}
		o += s.cfg.BasePath
		if !slices.Contains(out, o) {
			out = append(out, o)
		}
	}
	return out
}

// ownAudience reports whether aud is one of ownOrigins, by exact bytes: the audience itself is never normalised.
func (s *Server) ownAudience(aud string) bool {
	return slices.Contains(s.ownOrigins(), aud)
}

// inboundPerson reports whether ctx is a signed-in person's session: never an API token, the admin token (no human)
// or a delegated request. The workload-binding rule.
func inboundPerson(ctx context.Context) bool {
	return oidcHumanFromContext(ctx) != "" && apiTokenIDFromContext(ctx) == uuid.Nil && !delegatedRequest(ctx)
}

// needsPerson guards the writes that widen inbound federation: false after the 403 when no signed-in person makes
// the request. It refuses the admin token too, so governanceWriteMode's bypass never reaches these writes.
func (s *Server) needsPerson(w http.ResponseWriter, r *http.Request) bool {
	if !inboundPerson(r.Context()) {
		writeErrorReason(w, http.StatusForbidden, reasonInboundFederationNeedsPerson, inboundFederationNeedsPersonMsg)
		return false
	}
	return true
}

// wardynOwnHost reports whether u is on a host this Wardyn serves: one of its configured addresses, or a per-run
// UI host. A trust never names such an issuer: the gateway key set lives on these hosts, and on a per-run UI host
// the sandbox's own app answers.
func (s *Server) wardynOwnHost(u *url.URL) bool {
	for _, raw := range []string{s.cfg.RunnerOrgURL, s.cfg.OIDCRedirectURL, s.cfg.UIAdvertiseURL, s.cfg.ControlPlaneURL} {
		if raw == "" {
			continue
		}
		own, err := url.Parse(raw)
		if err != nil || own.Hostname() == "" {
			continue
		}
		if strings.EqualFold(u.Hostname(), own.Hostname()) {
			return true
		}
	}
	return uiRunHost(u.Hostname(), s.cfg.UIOriginTemplate)
}

// uiRunHost reports whether host is one of the UI-sandbox gateway's per-run hosts (WARDYN_UI_SANDBOX_ORIGIN_TEMPLATE, {run} standing for any first label), where a run's own app answers.
func uiRunHost(host, template string) bool {
	if template == "" {
		return false
	}
	parsed, err := url.Parse(strings.ReplaceAll(template, "{run}", "x"))
	if err != nil {
		return false
	}
	tmplHost := template
	if i := strings.Index(tmplHost, "://"); i >= 0 {
		tmplHost = tmplHost[i+3:]
	}
	if i := strings.IndexAny(tmplHost, "/?#"); i >= 0 {
		tmplHost = tmplHost[:i]
	}
	if i := strings.LastIndex(tmplHost, ":"); i >= 0 && !strings.Contains(tmplHost[i:], "}") {
		tmplHost = tmplHost[:i]
	}
	first, rest, _ := strings.Cut(tmplHost, ".")
	prefix, suffix, wild := strings.Cut(first, "{run}")
	if !wild {
		return strings.EqualFold(host, parsed.Hostname())
	}
	label, after, _ := strings.Cut(host, ".")
	return len(label) > len(prefix)+len(suffix) &&
		strings.HasPrefix(strings.ToLower(label), strings.ToLower(prefix)) &&
		strings.HasSuffix(strings.ToLower(label), strings.ToLower(suffix)) &&
		strings.EqualFold(after, rest)
}

// inboundTrustStore is the store's inbound trust half, when it has one.
func inboundTrustStore(s *Server) (store.InboundTrustStore, bool) {
	its, ok := s.cfg.Store.(store.InboundTrustStore)
	return its, ok
}

// inboundSharedAudience reports whether aud is a provider's shared default, or the issuer itself.
func inboundSharedAudience(aud, iss string) bool {
	return slices.Contains(inboundSharedAudiences, aud) || strings.HasPrefix(aud, inboundSharedAudiencePrefix) || aud == iss
}

// inboundStoreOr409 is the store behind the security-tier routes, after the local-mode refusal: inbound federation
// needs single sign-on, since every trust names a service identity's sponsor. 501 on a store without trusts.
func (s *Server) inboundStoreOr409(w http.ResponseWriter) (store.InboundTrustStore, bool) {
	if s.cfg.LocalMode {
		writeErrorReason(w, http.StatusConflict, reasonNeedsSSO, "inbound federation needs single sign-on")
		return nil, false
	}
	its, ok := inboundTrustStore(s)
	if !ok {
		writeInboundStoreMissing(w)
	}
	return its, ok
}

func writeInboundStoreMissing(w http.ResponseWriter) {
	writeErrorReason(w, http.StatusNotImplemented, reasonPeopleStoreUnavailable, "inbound federation requires the Postgres store backend")
}

// inboundSwitchAnswer is the switch's answer: its state, the exchanged tokens a switch-off revoked and the pending
// turn-on changes it cancelled.
func inboundSwitchAnswer(f types.InboundFederation, revoked int, cancelled []uuid.UUID) map[string]any {
	ids := make([]string, 0, len(cancelled))
	for _, id := range cancelled {
		ids = append(ids, id.String())
	}
	return map[string]any{"enabled": f.Enabled, "updated_at": f.UpdatedAt, "updated_by": f.UpdatedBy,
		"tokens_revoked": revoked, "changes_cancelled": ids}
}

// handleGetInboundFederation is GET /api/v1/inbound-federation: the switch.
func (s *Server) handleGetInboundFederation(w http.ResponseWriter, r *http.Request) {
	its, ok := s.inboundStoreOr409(w)
	if !ok {
		return
	}
	cur, err := its.GetInboundFederation(r.Context())
	if err != nil {
		writeServerError(w, r, "read inbound federation", err)
		return
	}
	writeJSON(w, http.StatusOK, cur)
}

// handlePutInboundFederation is PUT /api/v1/inbound-federation. Turning it on needs a signed-in person and is held
// for a second human under WARDYN_GOVERNANCE_SECOND_HUMAN; a turn-on that finds it on writes nothing. Turning it off
// takes any security-tier credential, is always a write, revokes every exchanged token and cancels a pending
// turn-on.
func (s *Server) handlePutInboundFederation(w http.ResponseWriter, r *http.Request) {
	its, ok := s.inboundStoreOr409(w)
	if !ok {
		return
	}
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	if req.Enabled == nil {
		writeErrorReason(w, http.StatusBadRequest, reasonInboundTrustInvalid, "enabled: required")
		return
	}
	ctx := r.Context()
	enabled := *req.Enabled
	if enabled {
		cur, err := its.GetInboundFederation(ctx)
		if err != nil {
			writeServerError(w, r, "read inbound federation", err)
			return
		}
		if cur.Enabled {
			writeJSON(w, http.StatusOK, inboundSwitchAnswer(cur, 0, nil))
			return
		}
		if !s.needsPerson(w, r) {
			return
		}
		mode, ok := s.governanceWriteMode(w, r)
		if !ok {
			return
		}
		if mode == govQueue {
			s.holdInboundFederationOn(w, r, cur)
			return
		}
	}
	set, err := its.SetInboundFederation(ctx, enabled, principalFromRequest(r))
	if err != nil {
		writeServerError(w, r, "set inbound federation", err)
		return
	}
	if set.TokensRevoked > 0 {
		s.dropInboundSessions()
	}
	answer := inboundSwitchAnswer(set.State, set.TokensRevoked, set.ChangesCancelled)
	actorType, actor := actorFromRequest(r)
	s.recordAudit(ctx, s.auditEvent(nil, actorType, actor, "inbound_federation.update", inboundFederationKey, "success",
		mustJSON(map[string]any{"enabled": set.State.Enabled, "tokens_revoked": set.TokensRevoked,
			"changes_cancelled": answer["changes_cancelled"]})))
	writeJSON(w, http.StatusOK, answer)
}

// inboundTrustRequest is a trust's create body. There is no update: a change is a delete and a new trust.
type inboundTrustRequest struct {
	Name               string `json:"name"`
	Issuer             string `json:"issuer"`
	InternalIssuer     string `json:"internal_issuer,omitempty"`
	Audience           string `json:"audience"`
	Subject            string `json:"subject"`
	RequiredClaim      string `json:"required_claim,omitempty"`
	RequiredClaimValue string `json:"required_claim_value,omitempty"`
	ServiceIdentity    string `json:"service_identity"`
	TokenTTLSeconds    int    `json:"token_ttl_seconds,omitempty"`
	MaxTokenAgeSeconds int    `json:"max_token_age_seconds,omitempty"`
	MaxLive            int    `json:"max_live,omitempty"`
}

// inboundTrustRequestOf is the request a trust resolves to, defaults applied: a held create's payload and view.
func inboundTrustRequestOf(t types.InboundTrust) inboundTrustRequest {
	return inboundTrustRequest{Name: t.Name, Issuer: t.Issuer, InternalIssuer: t.InternalIssuer, Audience: t.Audience,
		Subject: t.Subject, RequiredClaim: t.RequiredClaim, RequiredClaimValue: t.RequiredClaimValue,
		ServiceIdentity: t.ServiceIdentity, TokenTTLSeconds: t.TokenTTLSeconds, MaxTokenAgeSeconds: t.MaxTokenAgeSeconds,
		MaxLive: t.MaxLive}
}

// inboundTrustURL checks an issuer or internal issuer address: UTF-8 text with the shape a portal's issuer has.
func inboundTrustURL(field, raw string) (*url.URL, string) {
	u, ok := portalURLShape(raw)
	if !utf8.ValidString(raw) || !ok {
		return nil, field + ": an https URL with a host, and no userinfo, query or fragment"
	}
	return u, ""
}

// inboundTrustText checks an exact-match field: set, bounded, printable UTF-8, and never a wildcard.
func inboundTrustText(field, v string, max int) string {
	if v == "" || len(v) > max || !utf8.ValidString(v) || !controlCharFree(v) || strings.Contains(v, "*") {
		return fmt.Sprintf("%s: 1 to %d bytes of printable UTF-8 with no wildcard, the match is exact", field, max)
	}
	return ""
}

// inboundTrustBound resolves a bound where 0 means the default.
func inboundTrustBound(field string, v, lo, hi, def int) (int, string) {
	if v == 0 {
		return def, ""
	}
	if v < lo || v > hi {
		return 0, fmt.Sprintf("%s: between %d and %d", field, lo, hi)
	}
	return v, ""
}

// validateInboundTrust checks a create body in a fixed order, the first failure winning, and returns the trust, or
// a reason and a message. It reads only the configuration, so it is tested without a store.
func (s *Server) validateInboundTrust(req inboundTrustRequest) (types.InboundTrust, string, string) {
	t, msg := s.inboundTrustShape(req)
	if msg != "" {
		return types.InboundTrust{}, reasonInboundTrustInvalid, msg
	}
	if msg = s.inboundTrustReserved(t); msg != "" {
		return types.InboundTrust{}, reasonInboundTrustReserved, msg
	}
	return t, "", ""
}

// inboundTrustShape is validateInboundTrust's field checks (steps 1 to 6).
func (s *Server) inboundTrustShape(req inboundTrustRequest) (types.InboundTrust, string) {
	if !serviceNameRE.MatchString(req.Name) {
		return types.InboundTrust{}, "name: 3 to 63 of a-z, 0-9 and -, starting with a letter"
	}
	if !serviceNameRE.MatchString(req.ServiceIdentity) {
		return types.InboundTrust{}, "service_identity: the name of a service identity"
	}
	if _, msg := inboundTrustURL("issuer", req.Issuer); msg != "" {
		return types.InboundTrust{}, msg
	}
	if req.InternalIssuer != "" {
		u, msg := inboundTrustURL("internal_issuer", req.InternalIssuer)
		if msg != "" {
			return types.InboundTrust{}, msg
		}
		if !portalHostListed(u) {
			return types.InboundTrust{}, "internal_issuer: its host is not on this deployment's WARDYN_PORTAL_INTERNAL_ISSUER_HOSTS list"
		}
	}
	for _, f := range []struct {
		name, v string
		max     int
	}{{"audience", req.Audience, inboundTrustTextMax}, {"subject", req.Subject, inboundTrustTextMax}} {
		if msg := inboundTrustText(f.name, f.v, f.max); msg != "" {
			return types.InboundTrust{}, msg
		}
	}
	if (req.RequiredClaim == "") != (req.RequiredClaimValue == "") {
		return types.InboundTrust{}, "required_claim: required_claim and required_claim_value are both set or both empty"
	}
	if req.RequiredClaim != "" {
		if msg := inboundTrustText("required_claim", req.RequiredClaim, inboundTrustClaimMax); msg != "" {
			return types.InboundTrust{}, msg
		}
		if msg := inboundTrustText("required_claim_value", req.RequiredClaimValue, inboundTrustTextMax); msg != "" {
			return types.InboundTrust{}, msg
		}
		if slices.Contains(inboundRegisteredClaims, req.RequiredClaim) {
			return types.InboundTrust{}, "required_claim: a registered claim the verifier checks itself (iss, aud, sub, exp, nbf, iat, jti)"
		}
	}
	return s.inboundTrustBounds(req)
}

// inboundTrustBounds resolves the three bounds (step 6) into the trust.
func (s *Server) inboundTrustBounds(req inboundTrustRequest) (types.InboundTrust, string) {
	ttl, msg := inboundTrustBound("token_ttl_seconds", req.TokenTTLSeconds, types.InboundTrustTTLMinSeconds,
		types.InboundTrustTTLMaxSeconds, types.InboundTrustTTLDefaultSeconds)
	if msg != "" {
		return types.InboundTrust{}, msg
	}
	age, msg := inboundTrustBound("max_token_age_seconds", req.MaxTokenAgeSeconds, types.InboundTrustTTLMinSeconds,
		types.InboundTrustTTLMaxSeconds, types.InboundTrustMaxAgeDefaultSeconds)
	if msg != "" {
		return types.InboundTrust{}, msg
	}
	live := req.MaxLive
	if live == 0 {
		live = types.InboundTrustMaxLiveDefault
	} else if live < 1 || live > types.InboundTrustMaxLiveMax {
		return types.InboundTrust{}, fmt.Sprintf("max_live: between 1 and %d", types.InboundTrustMaxLiveMax)
	}
	return types.InboundTrust{Name: req.Name, Issuer: req.Issuer, InternalIssuer: req.InternalIssuer, Audience: req.Audience,
		Subject: req.Subject, RequiredClaim: req.RequiredClaim, RequiredClaimValue: req.RequiredClaimValue,
		ServiceIdentity: req.ServiceIdentity, TokenTTLSeconds: ttl, MaxTokenAgeSeconds: age, MaxLive: live}, ""
}

// inboundTrustReserved is validateInboundTrust's reserved checks (step 7): an issuer this Wardyn serves, an audience
// that is not one of its own addresses, a provider's shared default, or the console sign-in client.
func (s *Server) inboundTrustReserved(t types.InboundTrust) string {
	if u, ok := portalURLShape(t.Issuer); ok && s.wardynOwnHost(u) {
		return "issuer: this Wardyn's own address is never a trusted issuer"
	}
	if !s.ownAudience(t.Audience) {
		own := s.ownOrigins()
		if len(own) == 0 {
			return "audience: this Wardyn has no public address configured (WARDYN_OIDC_REDIRECT_URL or WARDYN_RUNNER_ORG_URL)"
		}
		return "audience: must be one of this Wardyn's own addresses: " + strings.Join(own, ", ")
	}
	if inboundSharedAudience(t.Audience, t.Issuer) {
		return "audience: a provider's shared default, which other services accept too"
	}
	if s.cfg.OIDC != nil && t.Audience == s.cfg.OIDC.ClientID() {
		return "audience: the console sign-in client"
	}
	return ""
}

// errInboundSeqUnreadable is a revocation sequence read below zero. A sequence never is, so the order a trust would
// be confirmed at is unknown, and nothing is created or confirmed on it.
var errInboundSeqUnreadable = errors.New("the revocation sequence read below zero")

// writeInboundSeqUnreadable answers errInboundSeqUnreadable.
func writeInboundSeqUnreadable(w http.ResponseWriter) {
	writeErrorReason(w, http.StatusServiceUnavailable, reasonTokenLookupUnavailable, "The revocation order could not be read; retry later")
}

// writeConfirmNewTrustError answers confirmNewTrust's error.
func writeConfirmNewTrustError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errInboundSeqUnreadable) {
		writeInboundSeqUnreadable(w)
		return
	}
	writeServerError(w, r, "read the revocation sequence", err)
}

// confirmNewTrust sets where a new trust starts against its sponsor's revocations: the current sequence (the request
// never speaks for the service principal) at now, confirmed by its creator. A revoke of the sponsor that commits
// after this read stops the trust; one before it does not, as for any token the sponsor holds. A negative sequence
// is errInboundSeqUnreadable.
func (s *Server) confirmNewTrust(r *http.Request, t *types.InboundTrust, by string, now time.Time) error {
	issue, err := s.mintIssue(r.Context(), types.ServicePrincipalPrefix+t.ServiceIdentity, now)
	if err != nil {
		return err
	}
	if issue.Seq < 0 {
		return errInboundSeqUnreadable
	}
	t.CreatedBy, t.ConfirmedBy, t.ConfirmedAt, t.ConfirmedSeq = by, by, now, issue.Seq
	return nil
}

// writeInboundTrustCreateError answers a store create's refusal, shared by the direct create and the hold's dry
// run, and reports whether it wrote one.
func writeInboundTrustCreateError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrNotFound):
		writeErrorReason(w, http.StatusNotFound, reasonServiceIdentityNotFound, serviceNotFoundMsg)
	case errors.Is(err, store.ErrServiceIdentityInactive):
		writeErrorReason(w, http.StatusConflict, reasonServiceIdentityInactive, "this service identity is suspended or retired")
	case errors.Is(err, store.ErrInboundTrustExists):
		writeErrorReason(w, http.StatusConflict, reasonInboundTrustExists, "a trust with that name, or with that issuer, audience and subject, exists")
	case errors.Is(err, store.ErrInboundTrustAudienceReserved):
		writeErrorReason(w, http.StatusBadRequest, reasonInboundTrustReserved, "audience: a live portal's client id or audience")
	default:
		writeServerError(w, r, "create inbound trust", err)
	}
	return true
}

// inboundTrustCreateData is inbound_trust.create's data, for the direct create and an approved hold alike.
func inboundTrustCreateData(t types.InboundTrust, sponsor string) map[string]any {
	return map[string]any{"name": t.Name, "issuer": t.Issuer, "internal_issuer": t.InternalIssuer, "audience": t.Audience,
		"subject": t.Subject, "required_claim": t.RequiredClaim, "service_identity": t.ServiceIdentity, "sponsor": sponsor,
		"token_ttl_seconds": t.TokenTTLSeconds, "max_token_age_seconds": t.MaxTokenAgeSeconds, "max_live": t.MaxLive}
}

// handleCreateInboundTrust is POST /api/v1/inbound-trusts: a signed-in person adds a trust, held for a second human
// under WARDYN_GOVERNANCE_SECOND_HUMAN.
func (s *Server) handleCreateInboundTrust(w http.ResponseWriter, r *http.Request) {
	now := s.cfg.Now().UTC() // before the body is read
	its, ok := s.inboundStoreOr409(w)
	if !ok || !s.needsPerson(w, r) {
		return
	}
	var req inboundTrustRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	t, reason, msg := s.validateInboundTrust(req)
	if reason != "" {
		writeErrorReason(w, http.StatusBadRequest, reason, msg)
		return
	}
	mode, ok := s.governanceWriteMode(w, r)
	if !ok {
		return
	}
	if mode == govQueue {
		s.holdInboundTrustCreate(w, r, t, now)
		return
	}
	if err := s.confirmNewTrust(r, &t, principalFromRequest(r), now); err != nil {
		writeConfirmNewTrustError(w, r, err)
		return
	}
	created, sponsor, err := its.CreateInboundTrust(r.Context(), t)
	if writeInboundTrustCreateError(w, r, err) {
		return
	}
	actorType, actor := actorFromRequest(r)
	s.recordAudit(r.Context(), s.auditEvent(nil, actorType, actor, "inbound_trust.create", created.ID.String(), "success",
		mustJSON(inboundTrustCreateData(created, sponsor))))
	writeJSON(w, http.StatusCreated, created)
}

// handleListInboundTrusts is GET /api/v1/inbound-trusts: every trust.
func (s *Server) handleListInboundTrusts(w http.ResponseWriter, r *http.Request) {
	its, ok := s.inboundStoreOr409(w)
	if !ok {
		return
	}
	trusts, err := its.ListInboundTrusts(r.Context(), "")
	if err != nil {
		writeServerError(w, r, "list inbound trusts", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"trusts": trusts})
}

// handleGetInboundTrust is GET /api/v1/inbound-trusts/{id}.
func (s *Server) handleGetInboundTrust(w http.ResponseWriter, r *http.Request) {
	its, ok := s.inboundStoreOr409(w)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id", "inbound trust")
	if !ok {
		return
	}
	t, err := its.GetInboundTrust(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErrorReason(w, http.StatusNotFound, reasonInboundTrustNotFound, inboundTrustNotFoundMsg)
		return
	}
	if err != nil {
		writeServerError(w, r, "read inbound trust", err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// handleDeleteInboundTrust is DELETE /api/v1/inbound-trusts/{id}: any security-tier credential removes a trust and
// revokes the tokens exchanged under it. It only narrows, so it is never held.
func (s *Server) handleDeleteInboundTrust(w http.ResponseWriter, r *http.Request) {
	its, ok := s.inboundStoreOr409(w)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id", "inbound trust")
	if !ok {
		return
	}
	s.deleteInboundTrust(w, r, its, store.InboundTrustDelete{ID: id}, false)
}

// deleteInboundTrust removes one trust, for an admin or the identity's sponsor (bySponsor), and answers.
func (s *Server) deleteInboundTrust(w http.ResponseWriter, r *http.Request, its store.InboundTrustStore, d store.InboundTrustDelete, bySponsor bool) {
	trust, revoked, err := its.DeleteInboundTrust(r.Context(), d)
	if errors.Is(err, store.ErrNotFound) {
		writeErrorReason(w, http.StatusNotFound, reasonInboundTrustNotFound, inboundTrustNotFoundMsg)
		return
	}
	if err != nil {
		writeServerError(w, r, "delete inbound trust", err)
		return
	}
	if revoked > 0 {
		s.dropInboundSessions()
	}
	actorType, actor := actorFromRequest(r)
	s.recordAudit(r.Context(), s.auditEvent(nil, actorType, actor, "inbound_trust.delete", trust.ID.String(), "success",
		mustJSON(map[string]any{"name": trust.Name, "service_identity": trust.ServiceIdentity, "tokens_revoked": revoked,
			"by_sponsor": bySponsor})))
	writeJSON(w, http.StatusOK, map[string]any{"id": trust.ID, "tokens_revoked": revoked})
}

// sponsoredTrusts is the sponsor's half's opening: the signed-in sponsor, their identity (every non-sponsor gets the
// missing name's 404, byte for byte) and the trust store.
func (s *Server) sponsoredTrusts(w http.ResponseWriter, r *http.Request) (string, types.ServiceIdentity, store.InboundTrustStore, bool) {
	sub, ok := s.sponsorHuman(w, r)
	if !ok {
		return "", types.ServiceIdentity{}, nil, false
	}
	_, si, ok := s.sponsoredIdentity(w, r, sub)
	if !ok {
		return "", types.ServiceIdentity{}, nil, false
	}
	its, ok := inboundTrustStore(s)
	if !ok {
		writeInboundStoreMissing(w)
	}
	return sub, si, its, ok
}

// handleListSponsoredInboundTrusts is GET /api/v1/me/service-identities/{name}/inbound-trusts: the trusts on an
// identity, to its sponsor only.
func (s *Server) handleListSponsoredInboundTrusts(w http.ResponseWriter, r *http.Request) {
	_, si, its, ok := s.sponsoredTrusts(w, r)
	if !ok {
		return
	}
	trusts, err := its.ListInboundTrusts(r.Context(), si.Name)
	if err != nil {
		writeServerError(w, r, "list inbound trusts", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"trusts": trusts})
}

// handleDeleteSponsoredInboundTrust is DELETE /api/v1/me/service-identities/{name}/inbound-trusts/{id}: the
// sponsor's veto of a trust on their identity.
func (s *Server) handleDeleteSponsoredInboundTrust(w http.ResponseWriter, r *http.Request) {
	sub, si, its, ok := s.sponsoredTrusts(w, r)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id", "inbound trust")
	if !ok {
		return
	}
	s.deleteInboundTrust(w, r, its, store.InboundTrustDelete{ID: id, Service: si.Name, Sponsor: sub}, true)
}

// handleConfirmInboundTrust is POST /api/v1/me/service-identities/{name}/inbound-trusts/{id}/confirm: the sponsor,
// signed in, confirms a trust again after a revoke of their sessions stopped it. The trust records the sequence of
// the presenting session, so it is as live as that session: a later full or global revoke stops it again.
func (s *Server) handleConfirmInboundTrust(w http.ResponseWriter, r *http.Request) {
	authorizedAt := s.cfg.Now().UTC()
	sub, ok := s.sponsorHuman(w, r)
	if !ok {
		return
	}
	if apiTokenIDFromContext(r.Context()) != uuid.Nil || delegatedRequest(r.Context()) {
		writeErrorReason(w, http.StatusForbidden, reasonInboundFederationNeedsPerson, inboundFederationNeedsPersonMsg)
		return
	}
	_, si, ok := s.sponsoredIdentity(w, r, sub)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id", "inbound trust")
	if !ok {
		return
	}
	its, ok := inboundTrustStore(s)
	if !ok {
		writeInboundStoreMissing(w)
		return
	}
	issue, ok := s.mintSessionLive(w, r, sub, authorizedAt)
	if !ok {
		return
	}
	// A sequence is never negative; one that were leaves the order unknown, so nothing is confirmed on it.
	if issue.Seq < 0 {
		writeInboundSeqUnreadable(w)
		return
	}
	t, err := its.ConfirmInboundTrust(r.Context(), store.InboundTrustConfirm{ID: id, Service: si.Name, Sponsor: sub,
		At: issue.At, Seq: issue.Seq})
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErrorReason(w, http.StatusNotFound, reasonInboundTrustNotFound, inboundTrustNotFoundMsg)
		return
	case errors.Is(err, store.ErrServiceIdentityInactive):
		writeErrorReason(w, http.StatusConflict, reasonServiceIdentityInactive, "this service identity is suspended or retired")
		return
	case err != nil:
		writeServerError(w, r, "confirm inbound trust", err)
		return
	}
	actorType, actor := actorFromRequest(r)
	s.recordAudit(r.Context(), s.auditEvent(nil, actorType, actor, "inbound_trust.confirm", t.ID.String(), "success",
		mustJSON(map[string]any{"name": t.Name, "service_identity": t.ServiceIdentity})))
	writeJSON(w, http.StatusOK, t)
}
