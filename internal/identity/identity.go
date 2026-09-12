// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package identity defines the per-run workload identity contract.
//
// One provider implements it today, with a second planned:
//   - embedded (shipped, default): SPIFFE-shaped JWT issuer (sub-ms mint,
//     denylist+TTL revocation, runner-asserted attestation). A strict SPIFFE
//     subset using go-spiffe types — never custom attestation or federation.
//   - spire [v0.5 — planned, not yet implemented]: real SPIRE — per-class warm
//     parent entry + per-run child entries, cryptographic node attestation,
//     entry-deletion revocation. No internal/identity/spire package exists yet.
//
// INVARIANT (Confinement gating): cloud STS federation and hostile
// multi-tenant workloads HARD-REQUIRE the (not-yet-built) spire provider. The
// embedded provider must refuse to mint identities whose grants include
// types.GrantCloudSTS.
package identity

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Claims is the verified content of a run identity token. The delegation
// chain is first-class: Sub is the human principal, Act is the agent run.
type Claims struct {
	// SPIFFEID is spiffe://<trust-domain>/agent-run/<run-id>.
	SPIFFEID string
	RunID    uuid.UUID
	// Sub is the human principal the run acts on behalf of.
	Sub string
	// Sponsor is the accountable human owner (defaults to Sub).
	Sponsor string
	// JTI uniquely identifies this token for revocation/audit join.
	JTI string
	// Audience the token was minted for (RFC 8707 discipline).
	Audience string
	IssuedAt time.Time
	Expiry   time.Time
}

// ExpiredTokenError is the typed refusal a provider returns when a presented
// token's ONLY defect is that it expired — signature and audience were good, and
// the token names a run. It carries that run id, which is the whole reason the
// type exists: the API's internal-auth boundary sees only an error, so without it
// "a run is calling /internal/* with a dead identity" cannot be recorded against
// the run it is about (internal/api's run.identity.expired row).
//
// It is NOT an authorization result. Verify still returns nil claims with it, and
// every caller must keep failing closed — this says which run to TELL ABOUT the
// refusal, never that the refusal was soft. A provider that cannot resolve the
// run id returns an ordinary error instead, so the API layer's fallback is
// silence, not a row keyed to the wrong run.
type ExpiredTokenError struct {
	RunID uuid.UUID
	Err   error
}

func (e *ExpiredTokenError) Error() string {
	return "identity: token for run " + e.RunID.String() + " has expired: " + e.Err.Error()
}

func (e *ExpiredTokenError) Unwrap() error { return e.Err }

// RunIdentity is what a provider mints at sandbox start.
type RunIdentity struct {
	SPIFFEID string
	// Token is the JWT-SVID (or embedded JWT) presented by sidecars and the
	// in-sandbox credential helper when calling the broker.
	Token  string
	JTI    string
	Expiry time.Time
}

// Provider mints, verifies, and revokes per-run identities.
type Provider interface {
	// Name returns "embedded" or "spire" — surfaced in UI/audit so the
	// trust boundary is always visible.
	Name() string
	// MintRunIdentity creates the run's identity. audience binds the token.
	MintRunIdentity(ctx context.Context, runID uuid.UUID, humanSub, sponsor, audience string) (RunIdentity, error)
	// Verify authenticates a presented token and returns its claims.
	// Revoked or expired tokens must fail closed.
	Verify(ctx context.Context, token, expectedAudience string) (*Claims, error)
	// RevokeRun invalidates ALL identities for a run (kill-switch cascade).
	RevokeRun(ctx context.Context, runID uuid.UUID) error
}
