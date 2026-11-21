// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base32"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
)

// The issuer-portal verifier. It checks a subject token against one registered issuer and one token profile,
// and answers a person and a signed end of life. It holds no sign-in configuration and derives no role: the
// delegated lane runs every portal person as an ordinary user.

const (
	// portalIdleDrop drops a verifier that has served nothing for this long.
	portalIdleDrop = time.Hour
	// portalRefreshEvery is how often discovery and keys are re-read.
	portalRefreshEvery = time.Hour
	// portalPersonIDMax is the longest person id accepted.
	portalPersonIDMax = 128
	// portalKeyDomain is hashed with the exact issuer into the issuer key.
	portalKeyDomain = "wardyn-portal-v2\n"
	// portalIssuerKeyLen is the issuer key's length in base32 characters: 130 bits.
	portalIssuerKeyLen = 26
)

// PortalRegistration is the part of a portal row the verifier reads. The issuer is the exact registered bytes.
type PortalRegistration struct {
	Issuer         string
	InternalIssuer string // "" or the address discovery and keys are fetched from
	TokenProfile   string // PortalProfileAccessToken or PortalProfileIDToken
	ClientID       string // the portal's own client at the provider
	Audience       string // access-token profile
	RequiredScopes []string
	Group          string // "" means no group is required
	GroupsClaim    string // "" means groups
	// RequiredClaim and RequiredClaimValue are the one optional claim match.
	RequiredClaim      string
	RequiredClaimValue string
	EmailDomains       []string      // ID-token profile only; empty means none
	MaxSubjectTokenAge time.Duration // 0 means 300 s
}

// PortalSubject is a verified person. It has no role: a portal person is always a user.
type PortalSubject struct {
	Principal     string // delegate:person:<k>:<e>
	Issuer        string // the exact issuer the token carried
	K             string // the issuer key
	PersonID      string // the verified person id the principal encodes
	Sub           string // the token's raw sub, display only
	Email         string // ID-token profile only, display only
	EmailVerified bool
	Groups        []string // canonical, only when the portal sets a group
	IssuedAt      time.Time
	Expiry        time.Time
	// GrantEnd is the signed part of a grant's end, the earlier of iat plus the maximum age and exp.
	GrantEnd time.Time
	// Matched names the claim match that held, at most one.
	Matched []string
}

// PortalRefusalClass says what kind of answer a refusal is.
type PortalRefusalClass int

const (
	// PortalRefusedRequest is a malformed request: the declared type does not match the profile.
	PortalRefusedRequest PortalRefusalClass = iota + 1
	// PortalRefusedToken is a token that fails a check.
	PortalRefusedToken
	// PortalRefusedAdmission is a valid token the portal may not act for.
	PortalRefusedAdmission
	// PortalRefusedUnavailable is an issuer outage, answered closed.
	PortalRefusedUnavailable
)

// PortalRefusal is a refusal. Reason is the audit reason; Cause names the coarse cause of an outage and is
// never upstream text; RetryAfter is set for a key cooldown.
type PortalRefusal struct {
	Reason     string
	Cause      string
	Class      PortalRefusalClass
	RetryAfter time.Duration
}

// OK reports whether the token was admitted.
func (r PortalRefusal) OK() bool { return r.Reason == "" }

// PortalIssuersOptions is the deployment's side of the verifier.
type PortalIssuersOptions struct {
	// Reserved reports whether a raw sub or person id names a non-person identity. Required.
	Reserved func(string) bool
	// InternalHosts lists the hosts, as host[:port], an internal issuer address may name. Only these may be
	// dialled at a private address.
	InternalHosts []string
	// InternalCAPEM is an optional CA bundle trusted for the listed hosts only, on top of the system roots.
	InternalCAPEM []byte

	clock            func() time.Time
	handshakeTimeout time.Duration
	roots            *x509.CertPool
	lookup           func(ctx context.Context, host string) ([]net.IP, error)
	dial             func(ctx context.Context, network, addr string) (net.Conn, error)
}

// PortalIssuers verifies subject tokens for issuer portals, one cached verifier per (issuer, internal address).
type PortalIssuers struct {
	reserved func(string) bool
	clock    func() time.Time
	net      *portalNet

	mu        sync.Mutex
	verifiers map[[2]string]*portalVerifier
}

// NewPortalIssuers builds the verifier set.
func NewPortalIssuers(opts PortalIssuersOptions) (*PortalIssuers, error) {
	if opts.Reserved == nil {
		return nil, errors.New("portal issuers: a reserved-name check is required")
	}
	clock := opts.clock
	if clock == nil {
		clock = time.Now
	}
	pn := &portalNet{handshakeTimeout: opts.handshakeTimeout, internalHosts: map[string]bool{}, roots: opts.roots, lookup: opts.lookup, dial: opts.dial}
	for _, h := range opts.InternalHosts {
		if a := portalListedAuthority(h); a != "" {
			pn.internalHosts[a] = true
		}
	}
	if len(opts.InternalCAPEM) > 0 {
		pool := opts.roots
		if pool == nil {
			pool, _ = x509.SystemCertPool()
		}
		if pool == nil {
			pool = x509.NewCertPool()
		} else {
			pool = pool.Clone()
		}
		if !pool.AppendCertsFromPEM(opts.InternalCAPEM) {
			return nil, errors.New("portal issuers: the internal CA bundle holds no certificate")
		}
		pn.internalCA = pool
	}
	return &PortalIssuers{reserved: opts.Reserved, clock: clock, net: pn, verifiers: map[[2]string]*portalVerifier{}}, nil
}

// PortalIssuerKey is k: the first 26 characters of the lowercase base32 of SHA-256 over the domain tag and the
// exact issuer bytes. The issuer is never normalised, so two spellings are two people.
func PortalIssuerKey(issuer string) string {
	sum := sha256.Sum256([]byte(portalKeyDomain + issuer))
	return strings.ToLower(portalBase32.EncodeToString(sum[:]))[:portalIssuerKeyLen]
}

// PortalPrincipal is delegate:person:<k>:<e>, where e is the lowercase base32 of the exact person id bytes.
func PortalPrincipal(issuer, personID string) string {
	return "delegate:person:" + PortalIssuerKey(issuer) + ":" + strings.ToLower(portalBase32.EncodeToString([]byte(personID)))
}

var portalBase32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// portalPersonIDShape reports whether id is 1-128 bytes of printable ASCII with no whitespace.
func portalPersonIDShape(id string) bool {
	if id == "" || len(id) > portalPersonIDMax {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] <= ' ' || id[i] > '~' {
			return false
		}
	}
	return true
}

// Verify checks a subject token against a portal registration and the declared token type. A refusal returns the
// audit reason; an empty string means the token was admitted.
func (p *PortalIssuers) Verify(ctx context.Context, reg PortalRegistration, raw, declaredType string) (PortalSubject, string) {
	subj, refusal := p.VerifyDetail(ctx, reg, raw, declaredType)
	return subj, refusal.Reason
}

// VerifyDetail is Verify with the whole refusal: its class, the cause of an outage and the retry delay.
func (p *PortalIssuers) VerifyDetail(ctx context.Context, reg PortalRegistration, raw, declaredType string) (PortalSubject, PortalRefusal) {
	want := portalProfileType(reg.TokenProfile)
	if want == "" {
		return PortalSubject{}, PortalRefusal{Reason: PortalProfileUnknown, Class: PortalRefusedRequest}
	}
	if declaredType != want {
		return PortalSubject{}, PortalRefusal{Reason: SubjectTokenTypeMismatch, Class: PortalRefusedRequest}
	}
	// Only RS256 is ever verified: HS*, none and anything else is refused before a key is looked at.
	if jws, err := jose.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256}); err != nil || len(jws.Signatures) != 1 {
		return PortalSubject{}, tokenRefusal(SubjectTokenInvalid)
	}
	v, err := p.verifierFor(reg)
	if err != nil {
		return PortalSubject{}, PortalRefusal{Reason: PortalIssuerUnavailable, Cause: portalFetchCause(err), Class: PortalRefusedUnavailable}
	}
	box := &portalBox{}
	ctx = withPortalBox(ctx, box)
	if cause := v.prepare(ctx); cause != "" {
		return PortalSubject{}, PortalRefusal{Reason: PortalIssuerUnavailable, Cause: cause, Class: PortalRefusedUnavailable}
	}
	tok, err := v.idv.Verify(ctx, raw)
	if err != nil {
		return PortalSubject{}, verifyFailure(box, err)
	}
	// A refresh can have failed, or withdrawn the key this token was verified under, since the health check
	// above: look again before anything is admitted.
	if refusal := v.recheck(box); refusal.Reason != "" {
		return PortalSubject{}, refusal
	}
	// go-oidc lets one non-identical spelling of an issuer through; Wardyn compares the bytes.
	if tok.Issuer != reg.Issuer {
		return PortalSubject{}, tokenRefusal(SubjectTokenInvalid)
	}
	var claims portalClaims
	if tok.Claims(&claims) != nil || claims == nil {
		return PortalSubject{}, tokenRefusal(SubjectTokenInvalid)
	}
	// The library fills Issuer by Go's struct decoding, which also matches a member spelled "ISS" or "Iss". The
	// signed claim is the exact member "iss", read from the verified payload's own map.
	if iss, ok := claims.str("iss"); !ok || iss != reg.Issuer {
		return PortalSubject{}, tokenRefusal(SubjectTokenInvalid)
	}
	return p.checkVerified(reg, claims, box.typ)
}

// verifyFailure classifies a failed go-oidc verification. An outage is read from the box, never from the error
// text. A token whose signature held but whose iat has the wrong shape (which go-oidc cannot decode) says so.
func verifyFailure(box *portalBox, err error) PortalRefusal {
	if box.fault != nil {
		return PortalRefusal{Reason: PortalIssuerUnavailable, Cause: box.fault.cause, Class: PortalRefusedUnavailable, RetryAfter: box.fault.retryAfter}
	}
	var expired *gooidc.TokenExpiredError
	if box.payload != nil && !errors.As(err, &expired) {
		var claims portalClaims
		if json.Unmarshal(box.payload, &claims) == nil {
			if _, ok := claims.date("iat"); !ok {
				return tokenRefusal(SubjectTokenIatInvalid)
			}
		}
	}
	return tokenRefusal(SubjectTokenInvalid)
}

func tokenRefusal(reason string) PortalRefusal {
	return PortalRefusal{Reason: reason, Class: PortalRefusedToken}
}

// checkVerified runs every check that follows signature and issuer: time, the profile, the person, admission.
func (p *PortalIssuers) checkVerified(reg PortalRegistration, c portalClaims, headerTyp string) (PortalSubject, PortalRefusal) {
	maxAge := reg.MaxSubjectTokenAge
	if maxAge <= 0 {
		maxAge = portalDefaultMaxAge
	}
	times, denial := portalCheckTimes(c, p.clock(), maxAge)
	if denial != "" {
		return PortalSubject{}, tokenRefusal(denial)
	}
	var personID string
	if reg.TokenProfile == PortalProfileAccessToken {
		personID, denial = reg.checkAccessToken(c)
	} else {
		personID, denial = reg.checkIDToken(c, headerTyp)
	}
	if denial != "" {
		return PortalSubject{}, tokenRefusal(denial)
	}
	// Both the raw sub and the person id are checked against the reserved names; the computed principal never is.
	sub, hasSub := c.str("sub")
	if c.has("sub") && !hasSub {
		return PortalSubject{}, tokenRefusal(SubjectTokenInvalid)
	}
	if (hasSub && p.reserved(sub)) || p.reserved(personID) {
		return PortalSubject{}, tokenRefusal(DenialReservedPrincipal)
	}
	if !portalPersonIDShape(personID) {
		return PortalSubject{}, tokenRefusal(SubjectTokenInvalid)
	}
	adm, denial := reg.checkAdmission(c)
	if denial != "" {
		return PortalSubject{}, PortalRefusal{Reason: denial, Class: PortalRefusedAdmission}
	}
	out := PortalSubject{
		Principal: PortalPrincipal(reg.Issuer, personID), Issuer: reg.Issuer, K: PortalIssuerKey(reg.Issuer),
		PersonID: personID, Sub: sub, Groups: adm.groups, Matched: adm.matched,
		IssuedAt: times.issuedAt, Expiry: times.expiry, GrantEnd: times.grantEnd,
	}
	if reg.TokenProfile == PortalProfileIDToken {
		out.Email, _ = c.str("email")
		out.EmailVerified = string(c["email_verified"]) == "true"
	}
	return out, PortalRefusal{}
}

// verifierFor returns the cached verifier for a registration, building it on first use and dropping any that
// has sat idle for an hour.
func (p *PortalIssuers) verifierFor(reg PortalRegistration) (*portalVerifier, error) {
	now := p.clock()
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, v := range p.verifiers {
		if now.Sub(time.Unix(0, v.lastUsed.Load())) > portalIdleDrop {
			delete(p.verifiers, k)
		}
	}
	key := [2]string{reg.Issuer, reg.InternalIssuer}
	if v, ok := p.verifiers[key]; ok {
		v.lastUsed.Store(now.UnixNano())
		return v, nil
	}
	v, err := newPortalVerifier(reg.Issuer, reg.InternalIssuer, p.clock, p.net)
	if err != nil {
		return nil, err
	}
	v.lastUsed.Store(now.UnixNano())
	p.verifiers[key] = v
	return v, nil
}

// portalVerifier is the discovery and key state of one (issuer, internal address) pair.
type portalVerifier struct {
	issuer, internal string
	clock            func() time.Time
	client           *http.Client
	ks               *portalKeySet
	idv              *gooidc.IDTokenVerifier
	keysURL          atomic.Pointer[string]
	lastUsed         atomic.Int64

	mu         sync.Mutex // serialises refresh decisions and the fetches they make
	disc       *portalDiscovery
	discOKAt   time.Time
	discFailed bool
	attemptAt  time.Time
	standing   string // the cause of a build that failed or of a document that was dropped

	// down is the cause the verifier is closed under, nil while it is healthy. A refresh sets it on every
	// outcome, so an exchange already in flight can read it without waiting for the refresh.
	down        atomic.Pointer[string]
	attemptNano atomic.Int64 // unix nanoseconds of the last refresh attempt, for Retry-After
}

func newPortalVerifier(issuer, internal string, clock func() time.Time, pn *portalNet) (*portalVerifier, error) {
	allowed, private, err := portalAllowedAuthorities(issuer, internal, pn)
	if err != nil {
		return nil, err
	}
	v := &portalVerifier{issuer: issuer, internal: internal, clock: clock, client: newPortalClient(pn, allowed, private)}
	v.ks = newPortalKeySet(clock, func(ctx context.Context) ([]jose.JSONWebKey, error) {
		u := v.keysURL.Load()
		if u == nil {
			return nil, &portalFetchError{cause: portalCauseUnreachable, detail: "no keys address"}
		}
		return portalFetchKeys(ctx, v.client, *u)
	})
	v.idv = gooidc.NewVerifier(issuer, v.ks, &gooidc.Config{SkipClientIDCheck: true, SupportedSigningAlgs: []string{string(jose.RS256)}})
	return v, nil
}

// prepare brings discovery and keys up to date before a token is looked at and returns the cause of a 503, or "".
// It never lets a token through while the last fetch failed: a retry runs at most every 30 seconds, and a document
// that fails a rule drops the verifier until a refresh passes.
func (v *portalVerifier) prepare(ctx context.Context) string {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.clock()
	keysOKAt, keysAttempt, keysFailed := v.ks.state()
	last := v.attemptAt
	if keysAttempt.After(last) {
		last = keysAttempt
	}
	cooling := now.Before(last.Add(portalKeyCooldown))
	switch {
	case v.disc == nil:
		if v.standing != "" && cooling {
			return v.standing
		}
	case v.discFailed || keysFailed:
		if cooling {
			return portalCauseIssuerDown
		}
	case now.Before(v.discOKAt.Add(portalRefreshEvery)) && now.Before(keysOKAt.Add(portalRefreshEvery)):
		return ""
	}
	return v.refresh(ctx, now)
}

// refresh re-reads discovery and then the keys. A hostile or refused document drops the verifier; an outage keeps
// the last good result but leaves the verifier closed until a fetch succeeds.
func (v *portalVerifier) refresh(ctx context.Context, now time.Time) string {
	v.attemptAt = now
	v.attemptNano.Store(now.UnixNano())
	disc, err := portalDiscover(ctx, v.client, v.issuer, v.internal)
	if err != nil {
		cause := portalFetchCause(err)
		slog.Warn("portal issuer: discovery refresh failed", "issuer", v.issuer, "cause", cause)
		if cause == portalCauseUnreachable {
			v.discFailed = true
			if v.disc == nil {
				v.standing = cause
			}
			v.setDown(portalCauseIssuerDown)
			return cause
		}
		v.disc, v.discFailed, v.standing = nil, false, cause
		v.keysURL.Store(nil)
		v.ks.drop()
		v.setDown(cause)
		return cause
	}
	v.disc, v.discOKAt, v.discFailed, v.standing = &disc, now, false, ""
	v.keysURL.Store(&disc.fetchURL)
	if err := v.ks.refresh(ctx); err != nil {
		slog.Warn("portal issuer: key refresh failed", "issuer", v.issuer, "cause", portalFetchCause(err))
		v.setDown(portalCauseIssuerDown)
		return portalCauseKeysUnreachable
	}
	v.down.Store(nil)
	return ""
}

func (v *portalVerifier) setDown(cause string) { v.down.Store(&cause) }

// recheck is the success-point test: it reads the current health and key map again, without waiting for a
// refresh, and refuses a token that was verified under a state that has since failed or lost the key. An
// outage is the same 503 as a refused-at-the-door token, with the rest of the retry interval as Retry-After; a
// key a successful refresh withdrew is refused as a removed key is, and nothing is fetched early.
func (v *portalVerifier) recheck(box *portalBox) PortalRefusal {
	cause := ""
	if c := v.down.Load(); c != nil {
		cause = *c
	} else {
		switch v.ks.recheck(box.kid, box.key, box.gen) {
		case portalKeyUnhealthy:
			cause = portalCauseIssuerDown
		case portalKeyWithdrawn:
			return tokenRefusal(SubjectTokenInvalid)
		}
	}
	if cause == "" {
		return PortalRefusal{}
	}
	last := time.Unix(0, v.attemptNano.Load())
	if _, ksAttempt, _ := v.ks.state(); ksAttempt.After(last) {
		last = ksAttempt
	}
	wait := max(last.Add(portalKeyCooldown).Sub(v.clock()), 0)
	return PortalRefusal{Reason: PortalIssuerUnavailable, Cause: cause, Class: PortalRefusedUnavailable, RetryAfter: wait}
}
