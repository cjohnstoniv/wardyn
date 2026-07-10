// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package embedded implements the default, SPIFFE-shaped JWT-SVID identity
// provider satisfying identity.Provider. It mints short-lived ES256 JWTs whose
// subject path is spiffe://<trust-domain>/agent-run/<run-id>, carrying the full
// delegation chain (human sub, agent-run act, sponsor) for attribution.
//
// INVARIANT (Confinement gating): this provider REFUSES to mint identities for
// runs whose grants include types.GrantCloudSTS — cloud STS federation
// hard-requires the spire provider. Callers MUST invoke CheckGrants before
// mint; ErrRequiresSPIRE is the typed refusal.
//
// All security decisions fail closed: signature, expiry, audience, and
// revocation are each independently verified, and any RevocationStore error is
// treated as revoked.
package embedded

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
	"github.com/spiffe/go-spiffe/v2/spiffeid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// providerName is surfaced in UI/audit so the trust boundary is visible.
	providerName = "embedded"
	// DefaultTrustDomain is used when New is given an empty trust domain.
	DefaultTrustDomain = "wardyn.local"
	// tokenTTL is the fixed lifetime of a minted JWT-SVID (1h ceiling).
	tokenTTL = time.Hour
	// agentRunPathPrefix builds spiffe://<td>/agent-run/<run-id>.
	agentRunPathPrefix = "/agent-run/"
)

// ErrRequiresSPIRE is the typed refusal returned by CheckGrants (and MintRunIdentity)
// when a run's grants include types.GrantCloudSTS. cloud_sts hard-requires the
// spire provider; the embedded provider must never mint for it.
var ErrRequiresSPIRE = errors.New("embedded identity: cloud_sts grant requires the spire identity provider")

// Provider is the embedded JWT-SVID identity.Provider.
type Provider struct {
	signKey     *ecdsa.PrivateKey
	kid         string
	trustDomain spiffeid.TrustDomain
	signer      jose.Signer
	revocations RevocationStore
	rec         audit.Recorder
	now         func() time.Time // overridable in tests
}

var _ identity.Provider = (*Provider)(nil)

// New constructs the embedded provider. If signKey is nil a fresh ECDSA P-256
// key is generated. trustDomain may be empty (defaults to DefaultTrustDomain).
// The RevocationStore and audit.Recorder are required.
func New(signKey *ecdsa.PrivateKey, trustDomain string, revocations RevocationStore, rec audit.Recorder) (*Provider, error) {
	if revocations == nil {
		return nil, errors.New("embedded identity: revocation store is required")
	}
	if rec == nil {
		return nil, errors.New("embedded identity: audit recorder is required")
	}
	if signKey == nil {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("embedded identity: generate signing key: %w", err)
		}
		signKey = k
	}
	if signKey.Curve != elliptic.P256() {
		return nil, errors.New("embedded identity: signing key must be ECDSA P-256")
	}

	if trustDomain == "" {
		trustDomain = DefaultTrustDomain
	}
	td, err := spiffeid.TrustDomainFromString(trustDomain)
	if err != nil {
		return nil, fmt.Errorf("embedded identity: invalid trust domain %q: %w", trustDomain, err)
	}

	kid := keyID(&signKey.PublicKey)
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES256, Key: signKey},
		(&jose.SignerOptions{}).
			WithType("JWT").
			WithHeader("kid", kid),
	)
	if err != nil {
		return nil, fmt.Errorf("embedded identity: build signer: %w", err)
	}

	return &Provider{
		signKey:     signKey,
		kid:         kid,
		trustDomain: td,
		signer:      signer,
		revocations: revocations,
		rec:         rec,
		now:         time.Now,
	}, nil
}

// Name reports the provider kind for UI/audit. Always "embedded".
func (p *Provider) Name() string { return providerName }

// svidClaims is the JWT-SVID body. Embeds jose's registered claims and adds the
// SPIFFE delegation chain (act, sponsor).
type svidClaims struct {
	jwt.Claims
	// Act is the actor (agent-run) acting on behalf of Sub (RFC 8693 shape).
	Act actClaim `json:"act"`
	// Sponsor is the accountable human owner.
	Sponsor string `json:"sponsor"`
}

type actClaim struct {
	Sub string `json:"sub"` // the agent-run SPIFFE ID
}

// CheckGrants returns ErrRequiresSPIRE if any grant requires the spire provider
// (currently: cloud_sts). The broker/API MUST call this before mint.
func (p *Provider) CheckGrants(grants []types.GrantSpec) error {
	for _, g := range grants {
		if g.Kind == types.GrantCloudSTS {
			return ErrRequiresSPIRE
		}
	}
	return nil
}

// MintRunIdentity issues a 1h ES256 JWT-SVID for runID. humanSub is the human
// principal; sponsor is the accountable owner (defaults to humanSub when
// empty); audience binds the token (RFC 8707). It emits an identity.mint audit
// event (actor_type system).
func (p *Provider) MintRunIdentity(ctx context.Context, runID uuid.UUID, humanSub, sponsor, audience string) (identity.RunIdentity, error) {
	if humanSub == "" {
		return identity.RunIdentity{}, errors.New("embedded identity: humanSub is required")
	}
	if audience == "" {
		return identity.RunIdentity{}, errors.New("embedded identity: audience is required")
	}
	if sponsor == "" {
		sponsor = humanSub
	}

	id, err := p.spiffeIDForRun(runID)
	if err != nil {
		return identity.RunIdentity{}, err
	}
	spiffeID := id.String()

	now := p.now()
	exp := now.Add(tokenTTL)
	jti := uuid.NewString()

	claims := svidClaims{
		Claims: jwt.Claims{
			Issuer:    p.trustDomain.IDString(),
			Subject:   humanSub,
			Audience:  jwt.Audience{audience},
			ID:        jti,
			IssuedAt:  jwt.NewNumericDate(now),
			Expiry:    jwt.NewNumericDate(exp),
			NotBefore: jwt.NewNumericDate(now),
		},
		Act:     actClaim{Sub: spiffeID},
		Sponsor: sponsor,
	}

	token, err := jwt.Signed(p.signer).Claims(claims).Serialize()
	if err != nil {
		p.audit(ctx, runID, spiffeID, "identity.mint", jti, "failure")
		return identity.RunIdentity{}, fmt.Errorf("embedded identity: sign token: %w", err)
	}

	p.audit(ctx, runID, spiffeID, "identity.mint", jti, "success")

	return identity.RunIdentity{
		SPIFFEID: spiffeID,
		Token:    token,
		JTI:      jti,
		Expiry:   exp,
	}, nil
}

// Verify authenticates a presented token and returns its claims. It validates,
// independently and failing closed: ES256 signature, expiry/nbf, audience, and
// revocation. A RevocationStore error is treated as revoked.
func (p *Provider) Verify(ctx context.Context, token, expectedAudience string) (*identity.Claims, error) {
	if expectedAudience == "" {
		return nil, errors.New("embedded identity: expected audience is required")
	}

	parsed, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		return nil, fmt.Errorf("embedded identity: parse token: %w", err)
	}

	var claims svidClaims
	// Claims with the public key verifies the ES256 signature.
	if err := parsed.Claims(&p.signKey.PublicKey, &claims); err != nil {
		return nil, fmt.Errorf("embedded identity: verify signature: %w", err)
	}

	// Validate registered claims: expiry/nbf/iat (with leeway) and audience.
	if err := claims.Validate(jwt.Expected{
		AnyAudience: jwt.Audience{expectedAudience},
		Time:        p.now(),
	}); err != nil {
		return nil, fmt.Errorf("embedded identity: validate claims: %w", err)
	}

	runID, err := p.runIDFromActor(claims.Act.Sub)
	if err != nil {
		return nil, err
	}

	// Revocation: fail closed on store error.
	revoked, err := p.revocations.IsRevoked(ctx, claims.ID, runID)
	if err != nil {
		return nil, fmt.Errorf("embedded identity: revocation check failed (failing closed): %w", err)
	}
	if revoked {
		return nil, fmt.Errorf("embedded identity: token jti %s (run %s) is revoked", claims.ID, runID)
	}

	out := &identity.Claims{
		SPIFFEID: claims.Act.Sub,
		RunID:    runID,
		Sub:      claims.Subject,
		Sponsor:  claims.Sponsor,
		JTI:      claims.ID,
		Audience: expectedAudience,
	}
	if claims.IssuedAt != nil {
		out.IssuedAt = claims.IssuedAt.Time()
	}
	if claims.Expiry != nil {
		out.Expiry = claims.Expiry.Time()
	}
	return out, nil
}

// RevokeRun invalidates ALL identities for a run (kill-switch cascade) and
// emits an identity.revoke audit event (actor_type system).
func (p *Provider) RevokeRun(ctx context.Context, runID uuid.UUID) error {
	if err := p.revocations.RevokeRun(ctx, runID); err != nil {
		p.audit(ctx, runID, p.spiffeIDString(runID), "identity.revoke", "", "failure")
		return fmt.Errorf("embedded identity: revoke run %s: %w", runID, err)
	}
	p.audit(ctx, runID, p.spiffeIDString(runID), "identity.revoke", "", "success")
	return nil
}

func (p *Provider) spiffeIDForRun(runID uuid.UUID) (spiffeid.ID, error) {
	id, err := spiffeid.FromSegments(p.trustDomain, "agent-run", runID.String())
	if err != nil {
		return spiffeid.ID{}, fmt.Errorf("embedded identity: build spiffe id for run %s: %w", runID, err)
	}
	return id, nil
}

// spiffeIDString is a best-effort string form for audit actor fields.
func (p *Provider) spiffeIDString(runID uuid.UUID) string {
	id, err := p.spiffeIDForRun(runID)
	if err != nil {
		return ""
	}
	return id.String()
}

// runIDFromActor extracts the run UUID from an agent-run SPIFFE ID, verifying
// the trust domain matches this provider's.
func (p *Provider) runIDFromActor(actor string) (uuid.UUID, error) {
	id, err := spiffeid.FromString(actor)
	if err != nil {
		return uuid.Nil, fmt.Errorf("embedded identity: malformed actor spiffe id %q: %w", actor, err)
	}
	if id.TrustDomain() != p.trustDomain {
		return uuid.Nil, fmt.Errorf("embedded identity: actor trust domain %q does not match %q", id.TrustDomain(), p.trustDomain)
	}
	path := id.Path()
	if len(path) <= len(agentRunPathPrefix) || path[:len(agentRunPathPrefix)] != agentRunPathPrefix {
		return uuid.Nil, fmt.Errorf("embedded identity: actor path %q is not an agent-run", path)
	}
	runID, err := uuid.Parse(path[len(agentRunPathPrefix):])
	if err != nil {
		return uuid.Nil, fmt.Errorf("embedded identity: actor path %q has invalid run id: %w", path, err)
	}
	return runID, nil
}

// audit records an attribution event; failures must not block the operation
// (the Recorder owns durability/retry semantics), but a dropped audit write is
// logged loudly rather than silently swallowed — invariant 6 (every mint/revoke
// is an audit event) is best-effort here, so a swallowed write must stay visible.
func (p *Provider) audit(ctx context.Context, runID uuid.UUID, actor, action, jti, outcome string) {
	run := runID
	ev := types.AuditEvent{
		ID:        uuid.New(),
		Time:      p.now(),
		RunID:     &run,
		ActorType: types.ActorSystem,
		Actor:     actor,
		Action:    action,
		Target:    jti,
		Outcome:   outcome,
	}
	if err := p.rec.Record(ctx, ev); err != nil {
		log.Printf("wardyn: AUDIT WRITE FAILED action=%s target=%s outcome=%s: %v", ev.Action, ev.Target, ev.Outcome, err)
	}
}

// keyID derives a stable JWKS kid from the public key: base64url of the
// SHA-256 of the marshaled EC point.
func keyID(pub *ecdsa.PublicKey) string {
	// Uncompressed SEC1 point bytes (0x04||X||Y) as the stable, deterministic
	// thumbprint input, via crypto/ecdh (elliptic.Marshal is deprecated). The
	// encoding is byte-identical, so the derived kid is unchanged. ECDH() only
	// errors for a non-ECDH curve; the issuer key is always P-256 (ES256), so
	// fall back to the equivalent deprecated encoding rather than panic.
	if ep, err := pub.ECDH(); err == nil {
		sum := sha256.Sum256(ep.Bytes())
		return base64.RawURLEncoding.EncodeToString(sum[:])
	}
	//lint:ignore SA1019 unreachable for the P-256 issuer key; equivalent stable encoding
	pt := elliptic.Marshal(pub.Curve, pub.X, pub.Y)
	sum := sha256.Sum256(pt)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
