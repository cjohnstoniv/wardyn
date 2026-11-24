// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package embedded

import (
	"context"
	"errors"
	"fmt"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/cjohnstoniv/wardyn/internal/identity"
)

var _ identity.RecoveryVerifier = (*Provider)(nil)

// VerifyExpired authenticates a token that has expired, for the recovery door only (identity.RecoveryVerifier). It
// checks what Verify checks, with the audience and nbf judged at a moment when the token was still valid, and then
// requires the expiry to have passed: an unexpired token is identity.ErrNotExpired, and one that expired more than
// oldest ago is identity.ErrTokenTooOld, refused before the revocation store is read. A revoked token is a
// *identity.RevokedTokenError; a revocation store that cannot be read is an ordinary error, never a revocation.
func (p *Provider) VerifyExpired(ctx context.Context, token, expectedAudience string, oldest time.Duration) (*identity.Claims, error) {
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
	if claims.Expiry == nil {
		return nil, errors.New("embedded identity: token has no expiry")
	}

	// Audience and nbf are judged a second before the expiry, when the token was valid.
	if err := claims.Validate(jwt.Expected{
		AnyAudience: jwt.Audience{expectedAudience},
		Time:        claims.Expiry.Time().Add(-time.Second),
	}); err != nil {
		return nil, fmt.Errorf("embedded identity: validate claims: %w", err)
	}
	if p.now().Before(claims.Expiry.Time()) {
		return nil, identity.ErrNotExpired
	}
	if p.now().Sub(claims.Expiry.Time()) > oldest {
		return nil, identity.ErrTokenTooOld
	}

	runID, err := p.runIDFromActor(claims.Act.Sub)
	if err != nil {
		return nil, err
	}

	// Revocation: a store error is an error, never a pass.
	revoked, err := p.revocations.IsRevoked(ctx, claims.ID, runID)
	if err != nil {
		return nil, fmt.Errorf("embedded identity: revocation check failed (failing closed): %w", err)
	}

	out := &identity.Claims{
		SPIFFEID:      claims.Act.Sub,
		RunID:         runID,
		Sub:           claims.Subject,
		Sponsor:       claims.Sponsor,
		OperatorOwned: claims.OperatorOwned,
		JTI:           claims.ID,
		Audience:      expectedAudience,
		Expiry:        claims.Expiry.Time(),
	}
	if claims.IssuedAt != nil {
		out.IssuedAt = claims.IssuedAt.Time()
	}
	if revoked {
		return nil, &identity.RevokedTokenError{Claims: out,
			Err: fmt.Errorf("embedded identity: token jti %s (run %s) is revoked", claims.ID, runID)}
	}
	return out, nil
}
