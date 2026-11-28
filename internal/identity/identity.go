// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package identity defines the per-run workload identity contract. Two providers exist:
// embedded (the default), a SPIFFE-shaped JWT issuer (sub-ms mint, denylist+TTL revocation,
// runner-asserted attestation, a strict SPIFFE subset via go-spiffe — no custom attestation
// or federation); and spire (experimental, off by default, Kubernetes only), which has a
// SPIRE Server sign each run token as a JWT-SVID and registers one SPIRE entry per run so
// only that run's proxy is attested for its X.509-SVID.
//
// INVARIANT (Confinement gating): cloud STS federation is not built; every provider must
// refuse to mint identities whose grants include types.GrantCloudSTS.
package identity

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Claims is the verified content of a run identity token; the delegation chain is
// first-class — Sub is the human principal, Act is the agent run.
type Claims struct {
	// SPIFFEID is spiffe://<trust-domain> then RunPath's path: /agent-run/<run-id>, or
	// /workload/<owner>/<name>/run/<run-id> for a run a workload launched.
	SPIFFEID string
	RunID    uuid.UUID
	// Sub is the human principal the run acts on behalf of.
	Sub string
	// Sponsor is the accountable human owner (defaults to Sub).
	Sponsor string
	// OperatorOwned says the run's owner is the operator itself (admin token, local mode),
	// not a person — decided from what authenticated the request, never from Sub
	// (IdP-spoofable), and signed into the token.
	OperatorOwned bool
	// JTI uniquely identifies this token for revocation/audit join.
	JTI string
	// Audience the token was minted for (RFC 8707 discipline).
	Audience string
	IssuedAt time.Time
	Expiry   time.Time
	// Workload is the workload the run was launched for, zero for an ad-hoc run. Every provider derives
	// it from the token's verified run path (ParseRunPath), never from a stored or presented field, so
	// nothing but the signed ID can set it. A rule that tells workloads apart compares it whole, as
	// Workload.String's "<owner>/<name>", never a full SPIFFE ID, which is per run.
	Workload Workload
}

// ExpiredTokenError is the typed refusal for a presented token whose ONLY defect is
// expiry — signature and audience were good, and the token names a run. It carries that
// run id so the API's internal-auth boundary can record a dead-identity call against the
// right run (run.identity.expire), since callers otherwise see only an error.
//
// NOT an authorization result: Verify still returns nil claims with it, and callers must
// keep failing closed. A provider that can't resolve the run id returns an ordinary error
// instead, so the fallback is silence, not a row keyed to the wrong run.
//
// Expiry is the token's own expiry claim (zero when it had none), so a caller can tell a
// token that lapsed just now from one that lapsed long ago.
type ExpiredTokenError struct {
	RunID  uuid.UUID
	Expiry time.Time
	Err    error
}

func (e *ExpiredTokenError) Error() string {
	return "identity: token for run " + e.RunID.String() + " has expired: " + e.Err.Error()
}

func (e *ExpiredTokenError) Unwrap() error { return e.Err }

// RevokedTokenError is the typed refusal for a presented token whose ONLY defect is
// revocation: signature, audience and expiry were good. It carries the token's claims so the
// API's internal-auth boundary can still take a run's last egress reports after the run ended
// and its identity was revoked (internal/api's verifyRunToken), and nothing else.
//
// NOT an authorization result: Verify still returns nil claims with it, and every other
// caller keeps failing closed. A revocation-store error is an ordinary error, never this.
type RevokedTokenError struct {
	Claims *Claims
	Err    error
}

func (e *RevokedTokenError) Error() string { return e.Err.Error() }

func (e *RevokedTokenError) Unwrap() error { return e.Err }

// RunIdentity is what a provider mints at sandbox start.
type RunIdentity struct {
	SPIFFEID string
	// Token is the JWT-SVID (or embedded JWT) presented by sidecars and the in-sandbox credential helper when calling the broker.
	Token  string
	JTI    string
	Expiry time.Time
}

// Provider mints, verifies, and revokes per-run identities.
type Provider interface {
	// Name returns "embedded" or "spire" — surfaced in UI/audit so the trust boundary is always visible.
	Name() string
	// MintRunIdentity creates the run's identity; audience binds the token, operatorOwned becomes Claims.OperatorOwned.
	MintRunIdentity(ctx context.Context, runID uuid.UUID, humanSub, sponsor, audience string, operatorOwned bool) (RunIdentity, error)
	// Verify authenticates a presented token and returns its claims; revoked or expired tokens must fail closed.
	Verify(ctx context.Context, token, expectedAudience string) (*Claims, error)
	// RevokeRun invalidates ALL identities for a run (kill-switch cascade).
	RevokeRun(ctx context.Context, runID uuid.UUID) error
}
