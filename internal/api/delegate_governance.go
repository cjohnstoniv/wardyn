// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Registration of an issuer portal: a portal that names its own identity provider, so its people are the
// provider's, not the console's. The checks that need nothing but the request run first (validatePortalRow);
// the ones that need the deployment follow (portalDeploymentRefusal); the registry decision runs on one
// transaction under the registry lock (decidePortalRegistration). With WARDYN_GOVERNANCE_SECOND_HUMAN on, a
// person's registration is held as the governance kind "delegate", decided by a super admin, and the approval
// re-runs every check against the world as it then stands. A console-issuer portal never comes here.
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// govKindDelegate is the governance kind of a held issuer-portal registration. Its approver is a super admin:
// registering a portal is the super admin's alone, held or not.
const govKindDelegate = "delegate"

// envPortalInternalIssuerHosts lists, as comma-separated host[:port], the hosts an issuer portal's internal
// address may name. It is empty by default, and the verifier dials a private address only for these.
const envPortalInternalIssuerHosts = "WARDYN_PORTAL_INTERNAL_ISSUER_HOSTS"

// The bounds of an issuer portal's fields.
const (
	portalMaxAgeMin       = 300
	portalMaxAgeMax       = 600
	portalGrantTTLMin     = 300
	portalURLMaxLen       = 512
	portalTextMaxLen      = 200
	portalClaimNameMaxLen = 64
	portalClaimValueMax   = 256
	portalScopesMax       = 32
	portalDomainsMax      = 64
)

// portalClaimNameRE is a top-level claim name a portal may require: no path, no space.
var portalClaimNameRE = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

// portalReservedClaims are the claim names that already mean something to the verifier, so a claim match
// naming one would say nothing the profile does not already say or would contradict it.
var portalReservedClaims = []string{"iss", "sub", "aud", "exp", "iat", "nbf", "jti", "cid", "uid", "scp", "scope",
	"client_id", "azp", "auth_time", "ver", "groups"}

// registrationRefusal is one refusal of a registration: the answer a direct registration gives, and what a
// held one's decision is refused with.
type registrationRefusal struct {
	status        int
	reason, field string
	msg           string
}

func refuseRegistration(status int, reason, field, format string, args ...any) *registrationRefusal {
	return &registrationRefusal{status: status, reason: reason, field: field, msg: fmt.Sprintf(format, args...)}
}

func (e *registrationRefusal) Error() string { return e.reason + ": " + e.msg }

func (e *registrationRefusal) write(w http.ResponseWriter) {
	writeErrorReasonField(w, e.status, e.reason, e.msg, e.field)
}

// decided is the refusal as a decision answers it: 409, since the request was valid when it was proposed and
// the world has changed.
func (e *registrationRefusal) decided() error {
	c := *e
	c.status = http.StatusConflict
	return &govRefusal{why: c.reason, write: func(w http.ResponseWriter, _ *http.Request) { c.write(w) }}
}

// registrationRefusalOf is the refusal err carries, if it carries one.
func registrationRefusalOf(err error) *registrationRefusal {
	var ref *registrationRefusal
	if errors.As(err, &ref) {
		return ref
	}
	return nil
}

// consolePathRefusal is the refusal for a registration with no issuer that names a field only an issuer
// portal has: such a body would have been an unknown field before, and must never be taken and ignored. nil
// for a body the console path takes as it always did.
func consolePathRefusal(req registerDelegateRequest) *registrationRefusal {
	switch {
	case req.UISessionRenewal:
		return refuseRegistration(http.StatusUnprocessableEntity, reasonDelegateRenewalNeedsIssuer, "ui_session_renewal",
			"ui_session_renewal: only a portal with its own issuer can renew a session")
	case req.PortalManagedAdmission:
		return refuseRegistration(http.StatusUnprocessableEntity, reasonDelegatePortalManagedNeedsIssuer, "portal_managed_admission",
			"portal_managed_admission: only a portal with its own issuer can choose its own people")
	}
	for field, set := range map[string]bool{
		"internal_issuer": req.InternalIssuer != "", "token_profile": req.TokenProfile != "", "groups_claim": req.GroupsClaim != "",
		"email_domains": len(req.EmailDomains) > 0, "user_type": req.UserType != "", "audience": req.Audience != "",
		"required_scopes": len(req.RequiredScopes) > 0, "max_subject_token_age_seconds": req.MaxSubjectTokenAgeSeconds != nil,
		"grant_ttl_seconds": req.GrantTTLSeconds != nil, "required_claim": req.RequiredClaim != nil,
	} {
		if set {
			return refuseRegistration(http.StatusUnprocessableEntity, reasonDelegateIssuerFieldNeedsIssuer, field,
				"%s: only a portal with its own issuer takes this field", field)
		}
	}
	return nil
}

// issuerRowOf is the portal row an issuer-portal request asks for, with the request's text trimmed, its
// lists folded and its defaults filled in. It has no id, no registrar and no pin: the caller sets them. It
// checks nothing; validatePortalRow does.
func issuerRowOf(req registerDelegateRequest, name string) types.Delegate {
	d := types.Delegate{
		Name: name, IdPClientID: strings.TrimSpace(req.IdPClientID), Issuer: req.Issuer, InternalIssuer: req.InternalIssuer,
		TokenProfile: strings.TrimSpace(req.TokenProfile), GroupsClaim: strings.TrimSpace(req.GroupsClaim),
		UserType: strings.TrimSpace(req.UserType), UISessionRenewal: req.UISessionRenewal, Audience: strings.TrimSpace(req.Audience),
		PortalManagedAdmission: req.PortalManagedAdmission,
	}
	if g, ok := types.CanonicalGroupSubject(req.Group); ok {
		d.Group = g
	} else {
		d.Group = strings.TrimSpace(req.Group) // kept as typed, so the row check refuses it
	}
	for _, sc := range req.RequiredScopes {
		d.RequiredScopes = append(d.RequiredScopes, strings.TrimSpace(sc))
	}
	for _, dom := range req.EmailDomains {
		dom = strings.ToLower(strings.TrimSpace(dom))
		if !slices.Contains(d.EmailDomains, dom) {
			d.EmailDomains = append(d.EmailDomains, dom)
		}
	}
	d.MaxSubjectTokenAgeSeconds = portalMaxAgeMin
	if req.MaxSubjectTokenAgeSeconds != nil {
		d.MaxSubjectTokenAgeSeconds = *req.MaxSubjectTokenAgeSeconds
	}
	d.GrantTTLSeconds = d.MaxSubjectTokenAgeSeconds
	if req.GrantTTLSeconds != nil {
		d.GrantTTLSeconds = *req.GrantTTLSeconds
	}
	if req.RequiredClaim != nil {
		d.RequiredClaim = &types.PortalRequiredClaim{Claim: req.RequiredClaim.Claim, Value: req.RequiredClaim.Value}
	}
	return d
}

func unprocessable(reason, field, format string, args ...any) *registrationRefusal {
	return refuseRegistration(http.StatusUnprocessableEntity, reason, field, format, args...)
}

// validatePortalRow is every check of an issuer portal row that needs nothing outside the row and the
// deployment's host list, in the order a caller can fix them. A held registration's approval runs it again.
// It does not read the pin or the user type, which need the store.
func validatePortalRow(d types.Delegate) *registrationRefusal {
	access := d.TokenProfile == oidc.PortalProfileAccessToken
	// The verifier refuses a registration it cannot verify against, on every exchange. The same predicate
	// refuses it here: no client id, no issuer, or an access-token profile with no audience or no scopes.
	if d.IdPClientID == "" || d.Issuer == "" || (access && (d.Audience == "" || len(d.RequiredScopes) == 0)) {
		field := "idp_client_id"
		if d.IdPClientID != "" && d.Audience == "" {
			field = "audience"
		} else if d.IdPClientID != "" {
			field = "required_scopes"
		}
		return unprocessable(reasonPortalRegistrationInvalid, field,
			"a portal needs its client id, and an access-token portal its audience and at least one scope")
	}
	if len(d.IdPClientID) > portalTextMaxLen || !controlCharFree(d.IdPClientID) {
		return unprocessable(reasonDelegateClientIDInvalid, "idp_client_id", "idp_client_id: at most 200 bytes, no control characters")
	}
	if !access && d.TokenProfile != oidc.PortalProfileIDToken {
		return unprocessable(reasonDelegateTokenProfileInvalid, "token_profile", "token_profile: %s or %s", oidc.PortalProfileAccessToken, oidc.PortalProfileIDToken)
	}
	if !access && (d.Audience != "" || len(d.RequiredScopes) > 0) {
		return unprocessable(reasonDelegateAccessFieldsNeedAccess, "audience", "audience and required_scopes belong to the %s profile", oidc.PortalProfileAccessToken)
	}
	if access && len(d.EmailDomains) > 0 {
		return unprocessable(reasonDelegateEmailDomainsNeedIDToken, "email_domains", "email_domains: an access token carries no verified email, so only the %s profile takes them", oidc.PortalProfileIDToken)
	}
	if access && (len(d.Audience) > portalTextMaxLen || !controlCharFree(d.Audience)) {
		return unprocessable(reasonPortalRegistrationInvalid, "audience", "audience: at most 200 bytes, no control characters")
	}
	if len(d.RequiredScopes) > portalScopesMax {
		return unprocessable(reasonPortalRegistrationInvalid, "required_scopes", "required_scopes: at most %d", portalScopesMax)
	}
	for _, sc := range d.RequiredScopes {
		if sc == "" || len(sc) > portalTextMaxLen || strings.IndexFunc(sc, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
			return unprocessable(reasonPortalRegistrationInvalid, "required_scopes", "required_scopes: each scope is a non-empty word of at most 200 bytes")
		}
	}
	if len(d.EmailDomains) > portalDomainsMax {
		return unprocessable(reasonDelegateEmailDomainInvalid, "email_domains", "email_domains: at most %d", portalDomainsMax)
	}
	for _, dom := range d.EmailDomains {
		if dom == "" || len(dom) > 253 || strings.ContainsAny(dom, "@ \t") || !types.PrintableASCII(dom) {
			return unprocessable(reasonDelegateEmailDomainInvalid, "email_domains", "email_domains: each entry is a domain name, with no @")
		}
	}
	if d.GroupsClaim != "" && !portalClaimNameRE.MatchString(d.GroupsClaim) {
		return unprocessable(reasonDelegateGroupsClaimInvalid, "groups_claim", "groups_claim: 1-64 characters of letters, digits and _ . : -")
	}
	if ref := portalAdmissionRefusal(d); ref != nil {
		return ref
	}
	if d.MaxSubjectTokenAgeSeconds < portalMaxAgeMin || d.MaxSubjectTokenAgeSeconds > portalMaxAgeMax {
		return unprocessable(reasonDelegateMaxAgeInvalid, "max_subject_token_age_seconds",
			"max_subject_token_age_seconds: %d to %d", portalMaxAgeMin, portalMaxAgeMax)
	}
	if d.GrantTTLSeconds < portalGrantTTLMin {
		return unprocessable(reasonDelegateGrantTTLInvalid, "grant_ttl_seconds", "grant_ttl_seconds: at least %d", portalGrantTTLMin)
	}
	if d.GrantTTLSeconds > d.MaxSubjectTokenAgeSeconds {
		return unprocessable(reasonDelegateGrantTTLAboveMaxAge, "grant_ttl_seconds",
			"grant_ttl_seconds: at most the portal's max_subject_token_age_seconds (%d)", d.MaxSubjectTokenAgeSeconds)
	}
	if d.UserType == "" {
		return unprocessable(reasonDelegateUserTypeInvalid, "user_type", "user_type: required")
	}
	if _, ok := portalURLShape(d.Issuer); !ok {
		return unprocessable(reasonDelegateIssuerURLInvalid, "issuer", "issuer: an https URL with a host, and no userinfo, query or fragment")
	}
	if d.InternalIssuer != "" {
		u, ok := portalURLShape(d.InternalIssuer)
		if !ok {
			return unprocessable(reasonDelegateIssuerURLInvalid, "internal_issuer", "internal_issuer: an https URL with a host, and no userinfo, query or fragment")
		}
		if !portalHostListed(u) {
			return unprocessable(reasonDelegateInternalIssuerNotListed, "internal_issuer",
				"internal_issuer: its host is not on this deployment's %s list", envPortalInternalIssuerHosts)
		}
	}
	return nil
}

// portalAdmissionRefusal checks who a portal admits: the group, the portal-managed flag and the one claim.
func portalAdmissionRefusal(d types.Delegate) *registrationRefusal {
	if d.Group != "" {
		if canon, ok := types.CanonicalGroupSubject(d.Group); !ok || canon != d.Group {
			return unprocessable(reasonDelegateGroupInvalid, "group", "group: printable ASCII")
		}
		if d.PortalManagedAdmission {
			return unprocessable(reasonDelegatePortalManagedWithGroup, "portal_managed_admission",
				"portal_managed_admission: the portal chooses its own people only when no group is set")
		}
	} else if !d.PortalManagedAdmission {
		return unprocessable(reasonDelegateGroupOrPortalManaged, "group",
			"group: name one, or set portal_managed_admission to say the portal chooses its own people")
	}
	c := d.RequiredClaim
	if c == nil {
		return nil
	}
	switch {
	case c.Claim == "" || c.Value == "":
		return unprocessable(reasonDelegateClaimIncomplete, "required_claim", "required_claim: both claim and value, or neither")
	case slices.Contains(portalReservedClaims, c.Claim) || (d.GroupsClaim != "" && c.Claim == d.GroupsClaim):
		return unprocessable(reasonDelegateClaimReserved, "required_claim", "required_claim: %q already has a meaning to the verifier", c.Claim)
	case !portalClaimNameRE.MatchString(c.Claim):
		return unprocessable(reasonDelegateClaimInvalid, "required_claim", "required_claim: the claim is 1-64 characters of letters, digits and _ . : -")
	case len(c.Value) > portalClaimValueMax || !utf8.ValidString(c.Value) || strings.IndexFunc(c.Value, unicode.IsControl) >= 0:
		return unprocessable(reasonDelegateClaimInvalid, "required_claim", "required_claim: the value is 1-256 bytes of printable UTF-8")
	}
	return nil
}

// portalURLShape parses an issuer or internal address and reports whether it has the shape a portal may
// register: https, a host with an optional port, no userinfo, query or fragment, no space or control
// character. It reads nothing and dials nothing.
func portalURLShape(raw string) (*url.URL, bool) {
	if raw == "" || len(raw) > portalURLMaxLen || !strings.HasPrefix(raw, "https://") ||
		strings.IndexFunc(raw, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0 || strings.ContainsAny(raw, "?#") {
		return nil, false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, false
	}
	return u, true
}

// portalAuthority is a URL's host and effective port, as the verifier's list reads them: the host lower-cased,
// 443 when none is given.
func portalAuthority(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// portalHostListed reports whether the deployment lists u's authority as an internal issuer host.
func portalHostListed(u *url.URL) bool {
	want := portalAuthority(u)
	for _, entry := range strings.Split(os.Getenv(envPortalInternalIssuerHosts), ",") {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		host, port, err := net.SplitHostPort(entry)
		if err != nil {
			host, port = strings.Trim(entry, "[]"), "443"
		}
		if net.JoinHostPort(host, port) == want {
			return true
		}
	}
	return false
}

// portalActivation is the store's read of whether issuer portals have been switched on.
type portalActivation interface {
	PortalIssuersActivated(ctx context.Context) (bool, error)
}

// portalDeploymentRefusal is every check of an issuer portal that reads the deployment: the user type still
// exists, issuer portals are switched on, console sign-in is configured and the issuer is not its issuer. A
// non-nil error is a failed read, not a refusal.
func (s *Server) portalDeploymentRefusal(ctx context.Context, d types.Delegate) (*registrationRefusal, error) {
	if s.cfg.OIDC != nil && isOwnClientID(d.IdPClientID, s.cfg.OIDC.ClientID()) {
		return unprocessable(reasonDelegateClientIDIsPortal, "idp_client_id", "idp_client_id: must be the portal's own client, not this deployment's"), nil
	}
	known, err := s.userTypeExists(ctx, d.UserType)
	if err != nil {
		return nil, err
	}
	if !known {
		return unprocessable(reasonDelegateUserTypeInvalid, "user_type", "user_type: no such user type"), nil
	}
	act, ok := s.cfg.Store.(portalActivation)
	if !ok {
		return refuseRegistration(http.StatusNotImplemented, reasonDelegationStoreUnavailable, "", "issuer portals require the Postgres store backend"), nil
	}
	on, err := act.PortalIssuersActivated(ctx)
	if err != nil {
		return nil, err
	}
	if !on {
		return refuseRegistration(http.StatusConflict, reasonPortalIssuersNotActivated, "",
			"issuer portals have not been switched on for this deployment"), nil
	}
	if s.cfg.OIDC == nil {
		return refuseRegistration(http.StatusConflict, reasonPortalIssuersNeedSSO, "", "issuer portals need console sign-in configured"), nil
	}
	if types.NormIssuerURL(d.Issuer) == types.NormIssuerURL(s.cfg.OIDC.Issuer()) {
		return refuseRegistration(http.StatusConflict, reasonDelegateIssuerIsConsole, "issuer",
			"issuer: this is the console's own issuer, which a portal may not also be"), nil
	}
	return nil, nil
}

// hashPortalCredential is the SHA-256 digest a portal row keeps of its credential, as the store's own
// CreateDelegate keeps it.
func hashPortalCredential(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// portalPersonIDClaim is the claim that names a person under a profile: uid for an access token, sub for an
// ID token.
func portalPersonIDClaim(profile string) string {
	if profile == oidc.PortalProfileAccessToken {
		return "uid"
	}
	return "sub"
}

// portalRegistry is the part of the store a registration decision writes through.
type portalRegistry interface {
	store.PortalIssuerRegistryStore
}

// decidePortalRegistration is the registry decision of one issuer-portal registration, on the transaction q:
// the registry lock first, then the console issuers this deployment has served, then the binding and, for an
// issuer key no binding holds, the namespace census, and last the row. A refusal is a *registrationRefusal.
// The caller commits or, for a dry run, rolls back.
func (s *Server) decidePortalRegistration(ctx context.Context, q store.Querier, reg portalRegistry, d types.Delegate, digest, by string) (types.Delegate, error) {
	if err := store.LockPortalIssuerRegistry(ctx, q); err != nil {
		return types.Delegate{}, err
	}
	served, err := reg.ConsoleIssuersQ(ctx, q)
	if err != nil {
		return types.Delegate{}, err
	}
	norm := types.NormIssuerURL(d.Issuer)
	for _, c := range served {
		if c.IssuerNorm == norm || types.NormIssuerURL(c.Issuer) == norm {
			return types.Delegate{}, refuseRegistration(http.StatusConflict, reasonDelegateIssuerIsConsole, "issuer",
				"issuer: this deployment has served it as its console issuer, so a portal may never use it")
		}
	}
	key := types.PortalIssuerKey(d.Issuer)
	bound, err := store.IssuerKeyBoundQ(ctx, q, key)
	if err != nil {
		return types.Delegate{}, err
	}
	if !bound {
		hits, err := store.NamespaceCensusQ(ctx, q, key)
		if err != nil {
			return types.Delegate{}, err
		}
		if len(hits) > 0 {
			return types.Delegate{}, refuseRegistration(http.StatusConflict, reasonDelegateNamespaceInUse, "issuer",
				"issuer: stored records already name people under this issuer: %s", censusSources(hits))
		}
	}
	binding, err := reg.BindPortalIssuerQ(ctx, q, d.Issuer, types.NormIssuerURL(d.InternalIssuer), portalPersonIDClaim(d.TokenProfile), by)
	switch {
	case errors.Is(err, store.ErrIssuerAddressConflict):
		return types.Delegate{}, refuseRegistration(http.StatusConflict, reasonDelegateIssuerAddressConflict, "internal_issuer",
			"internal_issuer: this issuer is bound to another internal address")
	case errors.Is(err, store.ErrIssuerProfileConflict):
		return types.Delegate{}, refuseRegistration(http.StatusConflict, reasonDelegateIssuerProfileConflict, "token_profile",
			"token_profile: this issuer is bound to another person-id claim")
	case errors.Is(err, store.ErrIssuerSpellingConflict):
		return types.Delegate{}, refuseRegistration(http.StatusConflict, reasonDelegateIssuerSpellingConflict, "issuer",
			"issuer: another spelling of this issuer is already registered; use the spelling the provider stamps")
	case err != nil:
		return types.Delegate{}, err
	}
	d.IssuerKey = binding.IssuerKey
	return reg.CreateDelegateQ(ctx, q, d, digest)
}

// censusSources names the stored places a census found, with their counts and never a value, in a fixed order.
func censusSources(hits map[string]int) string {
	names := make([]string, 0, len(hits))
	for k := range hits {
		names = append(names, k)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, k := range names {
		parts[i] = fmt.Sprintf("%s (%d)", k, hits[k])
	}
	return strings.Join(parts, ", ")
}

// portalRegistryOf is the registry surface of the store, or false for a store without one.
func (s *Server) portalRegistryOf() (portalRegistry, bool) {
	reg, ok := s.cfg.Store.(portalRegistry)
	return reg, ok
}

// registerIssuerDelegate is POST /admin/delegates for a body that names an issuer. It answers 201 with the
// portal and its credential once, or, when the registration is held for a second human, 202 with the pending
// change and the credential once.
func (s *Server) registerIssuerDelegate(w http.ResponseWriter, r *http.Request, req registerDelegateRequest, name string) {
	d := issuerRowOf(req, name)
	if ref := validatePortalRow(d); ref != nil {
		ref.write(w)
		return
	}
	if strings.TrimSpace(req.RunInUnder) == "" {
		unprocessable(reasonDelegateIssuerPinRequired, "run_in_under", "run_in_under: an issuer portal runs in one unit; name it").write(w)
		return
	}
	pin, ok := s.resolvePortalUnit(w, r, req.RunInUnder)
	if !ok {
		return
	}
	d.RunInUnder = pin
	ctx := r.Context()
	ref, err := s.portalDeploymentRefusal(ctx, d)
	if err != nil {
		writeServerError(w, r, "register issuer portal", err)
		return
	}
	if ref != nil {
		ref.write(w)
		return
	}
	reg, ok := s.portalRegistryOf()
	if !ok {
		refuseRegistration(http.StatusNotImplemented, reasonDelegationStoreUnavailable, "", "issuer portals require the Postgres store backend").write(w)
		return
	}
	mode, ok := s.governanceWriteMode(w, r)
	if !ok {
		return
	}
	d.ID, d.RegisteredBy = uuid.New(), principalFromRequest(r)
	raw := newBearer(store.PortalCredentialPrefix)
	digest := hashPortalCredential(raw)
	if mode == govQueue {
		s.holdDelegate(w, r, reg, d, digest, raw)
		return
	}
	var created types.Delegate
	err = s.writeGovernance(ctx, func(q store.Querier) (err error) {
		created, err = s.decidePortalRegistration(ctx, q, reg, d, digest, d.RegisteredBy)
		return err
	})
	if ref := registrationRefusalOf(err); ref != nil {
		ref.write(w)
		return
	}
	if err != nil {
		writeServerError(w, r, "register issuer portal", err)
		return
	}
	s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"delegate.create", created.ID.String(), "success", mustJSON(portalCreateAudit(created))))
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindDelegate, created.ID.String(), "success", nil)
	}
	created.Credential = raw
	writeJSON(w, http.StatusCreated, created)
}

// heldDelegate is a held registration as stored: the row to write and the digest of the credential the
// proposer was given. Its payload is never served (types.GovernanceChange.Payload is json:"-").
type heldDelegate struct {
	Delegate types.Delegate `json:"delegate"`
	Digest   string         `json:"digest"`
}

// delegateDiffView is a registration as the approver reads it. It carries the registration's fields, the claim
// match with its value (the approver must see what they approve) and never a credential or its digest.
type delegateDiffView struct {
	Name                      string                     `json:"name"`
	IdPClientID               string                     `json:"idp_client_id"`
	Issuer                    string                     `json:"issuer"`
	InternalIssuer            string                     `json:"internal_issuer,omitempty"`
	TokenProfile              string                     `json:"token_profile"`
	Group                     string                     `json:"group,omitempty"`
	PortalManagedAdmission    bool                       `json:"portal_managed_admission,omitempty"`
	RequiredClaim             *types.PortalRequiredClaim `json:"required_claim,omitempty"`
	GroupsClaim               string                     `json:"groups_claim,omitempty"`
	EmailDomains              []string                   `json:"email_domains,omitempty"`
	Audience                  string                     `json:"audience,omitempty"`
	RequiredScopes            []string                   `json:"required_scopes,omitempty"`
	UserType                  string                     `json:"user_type"`
	RunInUnder                string                     `json:"run_in_under,omitempty"`
	UISessionRenewal          bool                       `json:"ui_session_renewal,omitempty"`
	MaxSubjectTokenAgeSeconds int                        `json:"max_subject_token_age_seconds"`
	GrantTTLSeconds           int                        `json:"grant_ttl_seconds"`
}

func delegateDiffViewOf(d types.Delegate) *delegateDiffView {
	v := &delegateDiffView{Name: d.Name, IdPClientID: d.IdPClientID, Issuer: d.Issuer, InternalIssuer: d.InternalIssuer,
		TokenProfile: d.TokenProfile, Group: d.Group, PortalManagedAdmission: d.PortalManagedAdmission, RequiredClaim: d.RequiredClaim,
		GroupsClaim: d.GroupsClaim, EmailDomains: d.EmailDomains, Audience: d.Audience, RequiredScopes: d.RequiredScopes,
		UserType: d.UserType, UISessionRenewal: d.UISessionRenewal, MaxSubjectTokenAgeSeconds: d.MaxSubjectTokenAgeSeconds,
		GrantTTLSeconds: d.GrantTTLSeconds}
	if d.RunInUnder != nil {
		v.RunInUnder = d.RunInUnder.String()
	}
	return v
}

// delegateAbsent is a held registration's base state: the portal did not exist when it was proposed.
type delegateAbsent struct {
	ID     uuid.UUID `json:"id"`
	Absent bool      `json:"absent"`
}

// holdDelegate stores a registration as a pending change. A dry run of the registry decision first shows it
// would apply, so a conflict or a dirty census is the direct registration's own refusal and is never held.
// The credential is returned once, in the 202; the change holds only its digest.
func (s *Server) holdDelegate(w http.ResponseWriter, r *http.Request, reg portalRegistry, d types.Delegate, digest, raw string) {
	ctx := r.Context()
	err := s.readGovernanceState(r, func(q store.Querier) error {
		_, err := s.decidePortalRegistration(ctx, q, reg, d, digest, d.RegisteredBy)
		return err
	})
	if ref := registrationRefusalOf(err); ref != nil {
		ref.write(w)
		return
	}
	if err != nil {
		writeServerError(w, r, "hold issuer portal registration", err)
		return
	}
	after := delegateDiffViewOf(d)
	s.proposeGovernanceChange(w, r, govProposal{
		kind: govKindDelegate, op: "create", key: d.ID.String(), payload: heldDelegate{Delegate: d, Digest: digest},
		after: after, changed: changedPaths(nil, after), baseState: delegateAbsent{ID: d.ID, Absent: true},
		answer: func(w http.ResponseWriter, saved types.GovernanceChange) {
			w.Header().Set("Location", "/api/v1/governance/changes/"+saved.ID.String())
			writeJSON(w, http.StatusAccepted, map[string]any{
				"pending_change": heldChangeAnswer{GovernanceChange: s.changeForRead(r, saved)},
				"credential":     raw,
			})
		},
	})
}

// applyDelegateChange applies a held registration inside the decision transaction, after the live approver
// and distinct-human checks. Every fact that can change while it is held is read again: the row's own checks
// (the host list included), the pin's unit, the user type, the activation, the console issuer, the binding
// and the census. A failure refuses the decision 409 with that field's reason and inserts nothing.
func applyDelegateChange(s *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	ctx := r.Context()
	var held heldDelegate
	if err := decodeHeldPayload(ch.Payload, &held); err != nil {
		return govApplied{}, writeRefusal(http.StatusBadRequest, reasonInvalidRequestBody, "invalid portal registration: %v", err)
	}
	d := held.Delegate
	if ref := validatePortalRow(d); ref != nil {
		return govApplied{}, ref.decided()
	}
	if d.RunInUnder == nil {
		return govApplied{}, unprocessable(reasonDelegateIssuerPinRequired, "run_in_under", "run_in_under: required").decided()
	}
	units, err := s.unitOutlines(ctx)
	if err != nil {
		return govApplied{}, err
	}
	if !slices.ContainsFunc(units, func(u types.GovernanceUnit) bool { return u.ID == *d.RunInUnder }) {
		return govApplied{}, unprocessable(reasonDelegateRunInUnderInvalid, "run_in_under", "%s", portalUnitInvalidMsg).decided()
	}
	ref, err := s.portalDeploymentRefusal(ctx, d)
	if err != nil {
		return govApplied{}, err
	}
	if ref != nil {
		return govApplied{}, ref.decided()
	}
	reg, ok := s.portalRegistryOf()
	if !ok {
		return govApplied{}, writeRefusal(http.StatusNotImplemented, reasonDelegationStoreUnavailable, "issuer portals require the Postgres store backend")
	}
	created, err := s.decidePortalRegistration(ctx, q, reg, d, held.Digest, d.RegisteredBy)
	if ref := registrationRefusalOf(err); ref != nil {
		return govApplied{}, ref.decided()
	}
	if err != nil {
		return govApplied{}, err
	}
	return govApplied{action: "delegate.create", target: created.ID.String(), data: portalCreateAudit(created)}, nil
}

// portalCreateAudit is the delegate.create row's data for an issuer portal: the registration's fields and the
// names of what it admits, never a claim's value, a credential or its digest.
func portalCreateAudit(d types.Delegate) map[string]any {
	admission := map[string]any{"group": d.Group != "", "portal_managed": d.PortalManagedAdmission, "claim": nil}
	if d.RequiredClaim != nil {
		admission["claim"] = d.RequiredClaim.Claim
	}
	data := map[string]any{"name": d.Name, "idp_client_id": d.IdPClientID, "group": d.Group, "issuer": d.Issuer, "issuer_key": d.IssuerKey,
		"token_profile": d.TokenProfile, "user_type": d.UserType, "ui_session_renewal": d.UISessionRenewal, "admission": admission,
		"max_subject_token_age_seconds": d.MaxSubjectTokenAgeSeconds, "grant_ttl_seconds": d.GrantTTLSeconds}
	if d.InternalIssuer != "" {
		data["internal_issuer"] = d.InternalIssuer
	}
	if d.GroupsClaim != "" {
		data["groups_claim"] = d.GroupsClaim
	}
	if len(d.EmailDomains) > 0 {
		data["email_domains"] = d.EmailDomains
	}
	if d.Audience != "" {
		data["audience"] = d.Audience
		data["required_scopes"] = d.RequiredScopes
	}
	if d.RunInUnder != nil {
		data["run_in_under"] = d.RunInUnder.String()
	}
	return data
}
