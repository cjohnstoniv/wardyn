// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package spire

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
	"github.com/spiffe/go-spiffe/v2/spiffeid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
)

// acceptedAlgs is exactly go-spiffe's JWT-SVID list: any other alg ("none", HS256 and EdDSA among them)
// is refused at parse.
var acceptedAlgs = []jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512, jose.ES256, jose.ES384, jose.ES512, jose.PS256, jose.PS384, jose.PS512,
}

// tokenJTI is a SPIRE token's jti: base64url of its SHA-256. A JWT-SVID need not carry one, and the
// digest names exactly one token.
func tokenJTI(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// MintRunIdentity implements identity.Provider.
func (p *Provider) MintRunIdentity(ctx context.Context, runID uuid.UUID, humanSub, sponsor, audience string, operatorOwned bool) (identity.RunIdentity, error) {
	return p.MintWorkloadRunIdentity(ctx, runID, identity.Workload{}, humanSub, sponsor, audience, operatorOwned)
}

// MintWorkloadRunIdentity has the SPIRE Server sign a JWT-SVID for the run's SPIFFE ID, registering the
// run's entry on its first mint, then writes the token's binding row. No row, no token. The host
// sensor's uuid.Nil mint registers no entry: no proxy ever presents an X.509-SVID for it.
func (p *Provider) MintWorkloadRunIdentity(ctx context.Context, runID uuid.UUID, w identity.Workload, humanSub, sponsor, audience string, operatorOwned bool) (identity.RunIdentity, error) {
	if humanSub == "" || audience == "" {
		return identity.RunIdentity{}, errors.New("spire identity: humanSub and audience are required")
	}
	if sponsor == "" {
		sponsor = humanSub
	}
	id, err := p.spiffeIDForRun(w, runID)
	if err != nil {
		return identity.RunIdentity{}, err
	}
	ri, err := p.mint(ctx, runID, id, humanSub, sponsor, audience, operatorOwned)
	if err != nil {
		p.audit(ctx, runID, id.String(), "identity.mint", "", "failure")
		return identity.RunIdentity{}, err
	}
	p.audit(ctx, runID, id.String(), "identity.mint", ri.JTI, "success")
	return ri, nil
}

func (p *Provider) mint(ctx context.Context, runID uuid.UUID, id spiffeid.ID, humanSub, sponsor, audience string, operatorOwned bool) (identity.RunIdentity, error) {
	if runID != uuid.Nil {
		if err := p.ensureEntry(ctx, runID, id); err != nil {
			return identity.RunIdentity{}, err
		}
	}
	token, err := p.server.MintJWTSVID(ctx, id, audience, identity.RunTokenTTL)
	if err != nil {
		return identity.RunIdentity{}, fmt.Errorf("spire identity: mint JWT-SVID for %s: %w", id, err)
	}
	exp, err := mintedExpiry(token, id)
	if err != nil {
		return identity.RunIdentity{}, err
	}
	b := identity.TokenBinding{JTI: tokenJTI(token), RunID: runID, SPIFFEID: id.String(), Sub: humanSub,
		Sponsor: sponsor, OperatorOwned: operatorOwned, Expiry: exp}
	if err := p.bindings.PutBinding(ctx, b); err != nil {
		return identity.RunIdentity{}, fmt.Errorf("spire identity: write token binding for run %s: %w", runID, err)
	}
	p.prune(ctx)
	return identity.RunIdentity{SPIFFEID: id.String(), Token: token, JTI: b.JTI, Expiry: exp}, nil
}

// mintedExpiry reads the exp of a token the server just minted over mTLS, and checks it names id.
func mintedExpiry(token string, id spiffeid.ID) (exp time.Time, err error) {
	parsed, err := jwt.ParseSigned(token, acceptedAlgs)
	if err != nil {
		return exp, fmt.Errorf("spire identity: the server's JWT-SVID does not parse: %w", err)
	}
	var c jwt.Claims
	if err := parsed.UnsafeClaimsWithoutVerification(&c); err != nil {
		return exp, fmt.Errorf("spire identity: the server's JWT-SVID claims: %w", err)
	}
	if c.Subject != id.String() || c.Expiry == nil {
		return exp, fmt.Errorf("spire identity: the server's JWT-SVID names %q, not %s, or has no exp", c.Subject, id)
	}
	return c.Expiry.Time(), nil
}

// ensureEntry registers the run's entry unless one already carries the run's selector, so a renew or a
// revive creates none.
func (p *Provider) ensureEntry(ctx context.Context, runID uuid.UUID, id spiffeid.ID) error {
	sels := runSelectors(p.cfg.selectors, runID.String())
	ids, err := p.server.FindEntries(ctx, runSelector(runID))
	if err != nil {
		return fmt.Errorf("spire identity: find entry for run %s: %w", runID, err)
	}
	if len(ids) > 0 {
		return nil
	}
	if err := p.server.CreateEntry(ctx, Entry{SPIFFEID: id, ParentID: p.cfg.parentID, Selectors: sels, X509SVIDTTL: entryX509SVIDTTL}); err != nil {
		return fmt.Errorf("spire identity: create entry for run %s: %w", runID, err)
	}
	return nil
}

// runSelector is the one selector only runID's entry carries.
func runSelector(runID uuid.UUID) Selector {
	return Selector{Type: "k8s", Value: selRunPrefix + runID.String()}
}

// prune deletes bindings past their lapse, for every run. Housekeeping: an error is logged, never
// returned, so it cannot fail a run start, a renew or a revoke.
func (p *Provider) prune(ctx context.Context) {
	if _, err := p.bindings.PruneBindings(ctx, p.now().Add(-identity.RunTokenLapseAfter)); err != nil {
		slog.WarnContext(ctx, "spire identity: prune of lapsed token bindings failed; the next mint or revoke retries", "err", err)
	}
}

type tokenClaims struct {
	jwt.Claims
}

// Verify checks the SPIRE signature against the trust bundle, the audience, that sub is a run of this
// trust domain, expiry, the binding row, then the denylist. A SPIRE-signed token with no binding row (a
// token a workload fetched for itself, or one minted outside wardynd) is refused.
func (p *Provider) Verify(ctx context.Context, token, expectedAudience string) (*identity.Claims, error) {
	if expectedAudience == "" {
		return nil, errors.New("spire identity: expected audience is required")
	}
	c, err := p.verifySignature(token)
	if err != nil {
		return nil, err
	}
	if len(c.Audience) != 1 || c.Audience[0] != expectedAudience {
		return nil, errors.New("spire identity: the token is not for this audience")
	}
	w, err := identity.WorkloadFromSPIFFEID(c.Subject, p.td.Name())
	if err != nil {
		return nil, fmt.Errorf("spire identity: token subject: %w", err)
	}
	runID, _ := identity.RunIDFromSPIFFEID(c.Subject, p.td.Name())
	if err := p.checkTime(c, runID); err != nil {
		return nil, err
	}
	jti := tokenJTI(token)
	b, err := p.bindings.GetBinding(ctx, jti)
	if err != nil {
		return nil, fmt.Errorf("spire identity: token binding: %w", err)
	}
	if b.RunID != runID || b.SPIFFEID != c.Subject {
		return nil, errors.New("spire identity: the token's binding names another run")
	}
	revoked, err := p.revocations.IsRevoked(ctx, jti, runID)
	if err != nil {
		return nil, fmt.Errorf("spire identity: revocation check failed (failing closed): %w", err)
	}
	out := &identity.Claims{SPIFFEID: c.Subject, RunID: runID, Sub: b.Sub, Sponsor: b.Sponsor,
		OperatorOwned: b.OperatorOwned, JTI: jti, Audience: expectedAudience, Workload: w, Expiry: c.Expiry.Time()}
	if c.IssuedAt != nil {
		out.IssuedAt = c.IssuedAt.Time()
	}
	if revoked {
		return nil, &identity.RevokedTokenError{Claims: out,
			Err: fmt.Errorf("spire identity: token jti %s (run %s) is revoked", jti, runID)}
	}
	return out, nil
}

// verifySignature parses with go-spiffe's algorithm list and checks the signature against the bundle's
// JWT authority for the token's kid.
func (p *Provider) verifySignature(token string) (tokenClaims, error) {
	var c tokenClaims
	parsed, err := jwt.ParseSigned(token, acceptedAlgs)
	if err != nil {
		return c, fmt.Errorf("spire identity: parse token: %w", err)
	}
	if len(parsed.Headers) != 1 || parsed.Headers[0].KeyID == "" {
		return c, errors.New("spire identity: the token names no key")
	}
	bundle, err := p.source.GetJWTBundleForTrustDomain(p.td)
	if err != nil {
		return c, fmt.Errorf("spire identity: JWT bundle: %w", err)
	}
	key, ok := bundle.FindJWTAuthority(parsed.Headers[0].KeyID)
	if !ok {
		return c, fmt.Errorf("spire identity: no JWT authority %q in the trust bundle", parsed.Headers[0].KeyID)
	}
	if err := parsed.Claims(key, &c); err != nil {
		return c, fmt.Errorf("spire identity: verify signature: %w", err)
	}
	return c, nil
}

// checkTime refuses a token with no exp, and returns an expired one as ExpiredTokenError with its run.
func (p *Provider) checkTime(c tokenClaims, runID uuid.UUID) error {
	if c.Expiry == nil {
		return errors.New("spire identity: the token has no exp")
	}
	err := c.Validate(jwt.Expected{Time: p.now()})
	if err == nil {
		return nil
	}
	if errors.Is(err, jwt.ErrExpired) {
		return &identity.ExpiredTokenError{RunID: runID, Expiry: c.Expiry.Time(),
			Err: fmt.Errorf("spire identity: validate claims: %w", err)}
	}
	return fmt.Errorf("spire identity: validate claims: %w", err)
}

// RevokeRun writes the denylist first, then deletes the run's entry (one already gone counts as deleted),
// then prunes lapsed bindings. Either of the first two failing returns an error, which the stop cascade
// retries. The run's own binding rows stay until they lapse, so Verify can still name the run.
func (p *Provider) RevokeRun(ctx context.Context, runID uuid.UUID) error {
	actor := p.adHocID(runID)
	if err := p.revocations.RevokeRun(ctx, runID); err != nil {
		p.audit(ctx, runID, actor, "identity.revoke", "", "failure")
		return fmt.Errorf("spire identity: revoke run %s: %w", runID, err)
	}
	if runID != uuid.Nil {
		if err := p.deleteEntries(ctx, runID); err != nil {
			p.audit(ctx, runID, actor, "identity.revoke", "", "failure")
			return err
		}
	}
	p.prune(ctx)
	p.audit(ctx, runID, actor, "identity.revoke", "", "success")
	return nil
}

func (p *Provider) deleteEntries(ctx context.Context, runID uuid.UUID) error {
	ids, err := p.server.FindEntries(ctx, runSelector(runID))
	if err == nil {
		err = p.server.DeleteEntries(ctx, ids)
	}
	if err != nil {
		return fmt.Errorf("spire identity: delete entry for run %s: %w", runID, err)
	}
	return nil
}

// RevokeJTI revokes one token without revoking its run (a revive retiring the run's old token).
func (p *Provider) RevokeJTI(ctx context.Context, jti string, runID uuid.UUID) error {
	if err := p.revocations.RevokeJTI(ctx, jti, runID); err != nil {
		p.audit(ctx, runID, p.adHocID(runID), "identity.jti.revoke", jti, "failure")
		return fmt.Errorf("spire identity: revoke jti for run %s: %w", runID, err)
	}
	p.audit(ctx, runID, p.adHocID(runID), "identity.jti.revoke", jti, "success")
	return nil
}

// adHocID is the run's ad-hoc SPIFFE ID for audit actor fields, as embedded writes it.
func (p *Provider) adHocID(runID uuid.UUID) string {
	id, err := p.spiffeIDForRun(identity.Workload{}, runID)
	if err != nil {
		return ""
	}
	return id.String()
}
