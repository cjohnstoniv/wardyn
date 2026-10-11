// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

var (
	_ oidc.SessionRevocations  = (*pgSessionRevocations)(nil)
	_ oidc.IdentityRevocations = (*pgSessionRevocations)(nil)
	_ oidc.SessionCutter       = (*pgSessionRevocations)(nil)
)

// sessionRevocationsFor returns the D16 pg-backed revocations store when OIDC
// is actually configured (authn != nil), else nil — mirrors "OIDC:
// feats.authn" on api.Config being nil exactly when OIDC is unconfigured, so
// the admin revoke-sessions surface never mounts with no session mechanism
// for it to act on. A second, independent *pgSessionRevocations instance from
// the one buildOptionalFeatures wires into oidc.Config.Revocations — both are
// stateless wrappers over the same pool, so two instances cost nothing.
func sessionRevocationsFor(authn *oidc.Authenticator, pool *pgxpool.Pool) oidc.SessionRevocations {
	if authn == nil {
		return nil
	}
	return &pgSessionRevocations{pool: pool}
}

// globalRevokeSub is the reserved oidc_session_revocations.sub sentinel for a
// revoke-all — see the migration's doc comment.
const globalRevokeSub = ""

// pgSessionRevocations is the pg-backed oidc.SessionRevocations (D16): a
// per-principal (and global) revoke CUTOFF over oidc_session_revocations,
// checked by internal/auth/oidc's Middleware on every authenticated request
// once wired. Distinct from pgRevocations above, which is the per-run SPIFFE
// identity denylist — a different table, a different session concept
// entirely (a stateless signed cookie has no row of its own to delete).
type pgSessionRevocations struct {
	pool *pgxpool.Pool
	// now is the APP clock IsSessionRevoked measures a credential's age on; nil
	// means time.Now. A test injects a clock that runs ahead of the database's,
	// which is the only honest way to simulate the F289 skew: the age helper
	// clamps a stamp from its own future to zero, so handing IsSessionRevoked an
	// issuedAt ahead of the real clock does not model a fast wardynd — it models
	// a stamp the app itself could never have written.
	now func() time.Time
}

func (r *pgSessionRevocations) appNow() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// IsSessionRevoked reports revoked when issuedAt is at-or-before the LATER of
// the cutoffs matching this human and the global one — a single query (MAX
// over the candidate rows) so a caller with no wired revocations at all (the
// common case: no row for either identity or globally) pays one lookup and
// gets back SQL NULL, which is "never revoked", not a zero-time false alarm.
//
// THREE candidate keys, because a revoke may name either identity (see
// oidc.SessionRevocations): the sub EXACTLY — an OIDC sub is opaque and
// case-sensitive, so folding it could collide two distinct principals — the
// email CASE-INSENSITIVELY, since that is how a human types one and the admin
// naming a target has no reason to match the IdP's casing, and the reserved ""
// global row.
//
// An empty email needs NO guard, and adding one would be unpinnable defensive
// code: lower(sub) = lower(”) selects exactly the sub = ” row, which is the
// global row the third arm already selects. The two arms return the same
// cutoff, so a session with no email claim behaves identically either way —
// verified by removing a NULLIF guard and finding no test could tell the
// difference, because there is no difference to tell.
//
// lower(sub) defeats the index on this arm. Deliberate: oidc_session_revocations
// holds one row per revoked principal plus the global one — tens of rows on a
// real deployment, not a scan worth an expression index — and the alternative
// (folding at write time) cannot work, since the writer does not know whether
// the caller named a sub or an email.
func (r *pgSessionRevocations) IsSessionRevoked(ctx context.Context, sub, email string, issuedAt time.Time) (bool, error) {
	st, err := r.sessionStatus(ctx, r.pool, sub, email, issuedAt, false, 0, true)
	return st == oidc.SessionRevoked, err
}

// SessionStatus is IsSessionRevoked plus the owner's identity rows, in the same statement so the
// check adds no round trip (oidc.IdentityRevocations): sub's identity rows refuse when one is
// deactivated or purged, or, for epoch >= 0, when its authority epoch is past the one the
// credential was admitted under. The cutoff answer is IsSessionRevoked's, unchanged.
func (r *pgSessionRevocations) SessionStatus(ctx context.Context, sub, email string, issuedAt time.Time, epoch int64) (oidc.SessionStatus, error) {
	return r.SessionStatusQ(ctx, r.pool, sub, email, issuedAt, epoch)
}

// SessionStatusQ checks the owner on the governance decision's transaction.
func (r *pgSessionRevocations) SessionStatusQ(ctx context.Context, q store.Querier, sub, email string, issuedAt time.Time, epoch int64) (oidc.SessionStatus, error) {
	return r.sessionStatus(ctx, q, sub, email, issuedAt, true, epoch, epoch >= 0)
}

func (r *pgSessionRevocations) sessionStatus(ctx context.Context, querier store.Querier, sub, email string, issuedAt time.Time, identity bool, epoch int64, cuts bool) (oidc.SessionStatus, error) {
	// Either clock's revocation wins. The database clock is returned with the
	// cutoff; app age is sampled after Scan so pool/server/response waits cannot
	// translate an old cookie forward across the cutoff. Raw-time comparison
	// retains the conservative earlier-revoking answer for DB-stamped tokens.
	// cuts says the browser-session cuts (CutSessions) count too: for IsSessionRevoked and for a credential
	// that carries an epoch, a session cookie, and not for an API token or an SSH key.
	q := `
		SELECT MAX(revoked_at), clock_timestamp(),
		        $4::boolean AND EXISTS (SELECT 1 FROM principal_identities WHERE principal = $1
		          AND (deactivated_at IS NOT NULL OR purged_at IS NOT NULL OR ($5::bigint >= 0 AND authority_epoch > $5::bigint)))
		FROM (SELECT sub, revoked_at FROM oidc_session_revocations
		      UNION ALL SELECT sub, cut_at FROM oidc_session_cuts WHERE $6::boolean) r
		WHERE sub = $1
		   OR lower(sub) = lower($2)
		   OR sub = $3`
	var cutoff sql.NullTime
	var databaseAt time.Time
	var blocked bool
	if err := querier.QueryRow(ctx, q, sub, email, globalRevokeSub, identity, epoch, cuts).Scan(&cutoff, &databaseAt, &blocked); err != nil {
		return oidc.SessionLive, fmt.Errorf("wardynd: is-session-revoked query: %w", err)
	}
	if blocked {
		return oidc.SessionDeactivated, nil
	}
	if !cutoff.Valid {
		return oidc.SessionLive, nil // no revocation on record for this sub or globally
	}
	// issuedAt.IsZero() (a pre-D16 cookie with no iat) sorts before EVERY real
	// cutoff, so it reads as revoked the moment any matching row exists at
	// all — see oidc.SessionRevocations' doc comment for why that is
	// deliberate rather than a bug. Said here rather than left to the arithmetic:
	// db.AppClockAgeMicros CLAMPS an age at a century, so the zero time would
	// otherwise be answered by a clamp rather than by the rule.
	anchor := db.AppClockAnchor{DatabaseAt: databaseAt, AppAt: r.appNow()}
	if issuedAt.IsZero() || !issuedAt.After(cutoff.Time) || !anchor.Translate(issuedAt).After(cutoff.Time) {
		return oidc.SessionRevoked, nil
	}
	return oidc.SessionLive, nil
}

// RevokeSub stamps sub's cutoff at now, invalidating every current session
// for that principal. Idempotent (repeat revokes just move the cutoff later).
func (r *pgSessionRevocations) RevokeSub(ctx context.Context, sub string) error {
	return r.upsertCutoff(ctx, sub)
}

// CutSessions stamps sub's session-only cutoff at now (oidc.SessionCutter): current browser sessions
// stop, API tokens and SSH keys are not read against it. Idempotent, like RevokeSub.
func (r *pgSessionRevocations) CutSessions(ctx context.Context, sub string) error {
	const q = `
		INSERT INTO oidc_session_cuts (sub, cut_at)
		VALUES ($1, now())
		ON CONFLICT (sub) DO UPDATE SET cut_at = EXCLUDED.cut_at`
	if _, err := r.pool.Exec(ctx, q, sub); err != nil {
		return fmt.Errorf("wardynd: cut sessions: %w", err)
	}
	return nil
}

// RevokeAll stamps the global cutoff at now, invalidating every current
// session for every principal.
func (r *pgSessionRevocations) RevokeAll(ctx context.Context) error {
	return r.upsertCutoff(ctx, globalRevokeSub)
}

func (r *pgSessionRevocations) upsertCutoff(ctx context.Context, sub string) error {
	const q = `
		INSERT INTO oidc_session_revocations (sub, revoked_at)
		VALUES ($1, now())
		ON CONFLICT (sub) DO UPDATE SET revoked_at = EXCLUDED.revoked_at`
	if _, err := r.pool.Exec(ctx, q, sub); err != nil {
		return fmt.Errorf("wardynd: revoke session cutoff: %w", err)
	}
	return nil
}
