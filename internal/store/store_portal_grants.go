// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The grants an issuer portal hands out. They live in portal_grants, a table of their own: an older
// server's delegated-token lookups never read it, so a wdg_ bearer held here is unknown to them. Like
// store_delegates.go, every read hashes the RAW bearer before it reaches SQL.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// PortalGrantStore is the issuer-portal exchange's and delegated lane's grant surface, optional like
// DelegateStore.
type PortalGrantStore interface {
	GetIssuerDelegateByRaw(ctx context.Context, raw string) (types.Delegate, error)
	MintPortalGrant(ctx context.Context, g types.PortalGrant, raw string, signedEnd time.Time, grantTTL time.Duration, admissionRev int64) (types.PortalGrant, time.Time, error)
	GetPortalGrantByRaw(ctx context.Context, raw string, now time.Time) (types.PortalGrant, error)
	GetPortalGrantByID(ctx context.Context, id uuid.UUID, now time.Time) (types.PortalGrant, error)
	PortalChainLive(ctx context.Context, delegateID uuid.UUID, principal string, chain uuid.UUID) (end time.Time, renewal bool, err error)
	StampPortalUsed(ctx context.Context, id uuid.UUID, at time.Time) error
	SweepExpiredPortalGrants(ctx context.Context, at time.Time) (int64, error)
}

var _ PortalGrantStore = PG{}

// portalGrantMinLife is the least life a grant may be minted with. A grant with less would answer an
// expires_in of 0 to 4 seconds and could not outlast the request that follows.
const portalGrantMinLife = 5 * time.Second

const portalGrantCols = `g.id, g.delegate_id, g.principal, g.user_type, g.chain_id, g.subject_issued_at, g.rev_seq, g.created_at, g.expires_at, d.issuer, d.issuer_key`

func scanPortalGrant(row pgx.Row) (types.PortalGrant, error) {
	var g types.PortalGrant
	err := row.Scan(&g.ID, &g.DelegateID, &g.Principal, &g.UserType, &g.ChainID, &g.SubjectIssuedAt, &g.RevSeq,
		&g.CreatedAt, &g.ExpiresAt, &g.Issuer, &g.IssuerKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.PortalGrant{}, ErrNotFound
	}
	if err != nil {
		return types.PortalGrant{}, fmt.Errorf("store: scan portal grant: %w", err)
	}
	return g, nil
}

// MintPortalGrant stores one grant for g.Principal through the portal g.DelegateID and returns it with the
// database time the mint read. raw is the plaintext wdg_ bearer; only its hash persists. signedEnd is the
// verifier's min(iat + max age, exp).
//
// One transaction, in this order: the portal's issuer binding row FOR KEY SHARE, then the portal's delegates
// row FOR SHARE, re-checking that the portal is live and its admission revision is admissionRev (ErrPortalGone,
// ErrAdmissionChanged), both held to commit so a retirement, a revoke or an admission change waits for the
// mint and the next one finds it done; then the per-(portal, person) advisory lock; then one read of the
// database clock, which decides everything after it. The grant ends at min(signedEnd, dbnow + grantTTL), and a
// mint with less than five seconds of life left is ErrSubjectTokenStale, nothing written. Lock order: the
// binding, the portal's row, the per-(portal, person) advisory lock, the per-person advisory lock, then the
// person's identity row FOR SHARE (ErrPortalPersonPurged when the person is purged, nothing written). The
// purge holds only the identity row (FOR UPDATE) and the credential erase only per-person locks, neither of
// which waits on a lock a mint holds, and a mint waits for them at its last two locks: no cycle. The grant joins
// the chain of the person's latest-expiring grant through this portal that is live at dbnow, or starts one.
//
// Nothing in the transaction writes delegates: two people's mints through one portal both hold its row
// FOR SHARE, and a write to it would make each wait for the other. The caller stamps last use and sweeps
// expired grants after the commit (StampPortalUsed, SweepExpiredPortalGrants).
func (s PG) MintPortalGrant(ctx context.Context, g types.PortalGrant, raw string, signedEnd time.Time, grantTTL time.Duration, admissionRev int64) (types.PortalGrant, time.Time, error) {
	var out types.PortalGrant
	var dbnow time.Time
	err := s.inTx(ctx, func(q Querier) error {
		key, err := lockPortalForExchange(ctx, q, g.DelegateID, admissionRev)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(g.Principal, types.PortalPersonPrefix+key+":") {
			return fmt.Errorf("store: the principal is not one of portal %s's people", g.DelegateID)
		}
		if _, err = q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text || $2, 0))`, g.DelegateID, g.Principal); err != nil {
			return fmt.Errorf("store: lock portal grant mints: %w", err)
		}
		// The person's own lock, shared with the credential erase and taken whether or not the identity row
		// exists yet: the erase either commits before the grant and the history are written, or waits for this
		// commit and deletes both.
		if _, err = q.Exec(ctx, `SELECT pg_advisory_xact_lock(`+portalPersonLockKey("$1::text")+`)`, g.Principal); err != nil {
			return fmt.Errorf("store: lock portal person: %w", err)
		}
		// The person's identity row FOR SHARE, held to commit: a permanent purge takes it FOR UPDATE, so it
		// either finished before this read, and the mint is refused, or waits for this commit and then deletes
		// the history written here. A person with no identity row records no history below.
		var purged bool
		err = q.QueryRow(ctx, `SELECT purged_at IS NOT NULL FROM principal_identities WHERE issuer = $1 AND principal = $2 FOR SHARE`,
			types.PortalPersonPrefix+key, g.Principal).Scan(&purged)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return fmt.Errorf("store: lock portal identity: %w", err)
		case purged:
			return ErrPortalPersonPurged
		}
		if err = q.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&dbnow); err != nil {
			return fmt.Errorf("store: read database clock: %w", err)
		}
		end := dbnow.Add(grantTTL)
		if signedEnd.Before(end) {
			end = signedEnd
		}
		end = end.Truncate(time.Microsecond)
		if end.Sub(dbnow) < portalGrantMinLife {
			return ErrSubjectTokenStale
		}
		id := g.ID
		if id == uuid.Nil {
			id = uuid.New()
		}
		var chain uuid.UUID
		err = q.QueryRow(ctx, `
			SELECT chain_id FROM portal_grants
			 WHERE delegate_id = $1 AND principal = $2 AND expires_at > $3
			 ORDER BY expires_at DESC LIMIT 1`, g.DelegateID, g.Principal, dbnow).Scan(&chain)
		if errors.Is(err, pgx.ErrNoRows) {
			chain, err = id, nil
		}
		if err != nil {
			return fmt.Errorf("store: read portal grant chain: %w", err)
		}
		out, err = scanPortalGrant(q.QueryRow(ctx, `
			WITH g AS (
				INSERT INTO portal_grants (id, delegate_id, token_sha256, principal, user_type, chain_id, subject_issued_at, rev_seq, created_at, expires_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
				RETURNING *)
			SELECT `+portalGrantCols+` FROM g JOIN delegates d ON d.id = g.delegate_id`,
			id, g.DelegateID, hashToken(raw), g.Principal, g.UserType, chain, g.SubjectIssuedAt, g.RevSeq, dbnow, end))
		if err != nil {
			return err
		}
		// The person's use of this portal is recorded where the grants' expiry and a suspension do not reach.
		// A person with no identity row records nothing; the exchange writes the identity first.
		if _, err = q.Exec(ctx, `
			INSERT INTO portal_person_portals (identity_id, delegate_id, first_used_at, last_used_at)
			SELECT id, $1, $3, $3 FROM principal_identities WHERE issuer = $4 AND principal = $2
			ON CONFLICT (identity_id, delegate_id) DO UPDATE SET last_used_at = EXCLUDED.last_used_at`,
			g.DelegateID, g.Principal, dbnow, types.PortalPersonPrefix+key); err != nil {
			return fmt.Errorf("store: record portal use: %w", err)
		}
		return nil
	})
	if err != nil {
		return types.PortalGrant{}, time.Time{}, err
	}
	return out, dbnow, nil
}

// lockPortalForExchange is the first half of every issuer-portal exchange write: the portal's binding row
// FOR KEY SHARE, then the portal's own delegates row FOR SHARE, both held to the caller's commit. It returns
// the portal's issuer key. A binding that is gone or a portal that is revoked is ErrPortalGone; an admission
// revision other than admissionRev is ErrAdmissionChanged; a row that is not an issuer portal is ErrNotFound.
func lockPortalForExchange(ctx context.Context, q Querier, portal uuid.UUID, admissionRev int64) (string, error) {
	var key string
	err := q.QueryRow(ctx, `SELECT issuer_key FROM delegates WHERE id = $1 AND issuer <> ''`, portal).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: read portal: %w", err)
	}
	var bound int
	err = q.QueryRow(ctx, `SELECT 1 FROM portal_issuers WHERE issuer_key = $1 FOR KEY SHARE`, key).Scan(&bound)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrPortalGone
	}
	if err != nil {
		return "", fmt.Errorf("store: lock issuer binding: %w", err)
	}
	var revoked bool
	var rev int64
	var lockedKey string
	err = q.QueryRow(ctx, `SELECT revoked_at IS NOT NULL, admission_rev, issuer_key FROM delegates WHERE id = $1 FOR SHARE`, portal).Scan(&revoked, &rev, &lockedKey)
	if err != nil {
		return "", fmt.Errorf("store: lock portal: %w", err)
	}
	switch {
	case revoked || lockedKey != key:
		return "", ErrPortalGone
	case rev != admissionRev:
		return "", ErrAdmissionChanged
	}
	return key, nil
}

// GetPortalGrantByRaw is the delegated lane's lookup of a portal grant: unexpired at now, through a portal
// that is still registered and has an issuer. It returns the portal's issuer pair for the reverse check (the
// principal must start with delegate:person:<IssuerKey>:). A revoked portal's grants answer ErrNotFound, so
// revoking a portal ends them at their next request.
func (s PG) GetPortalGrantByRaw(ctx context.Context, raw string, now time.Time) (types.PortalGrant, error) {
	return scanPortalGrant(s.Pool.QueryRow(ctx, `
		SELECT `+portalGrantCols+`
		  FROM portal_grants g JOIN delegates d ON d.id = g.delegate_id AND d.revoked_at IS NULL AND d.issuer <> ''
		 WHERE g.token_sha256 = $1 AND g.expires_at > $2`, hashToken(raw), now))
}

// GetPortalGrantByID is GetPortalGrantByRaw by the grant's id, for a UI session or an attach ticket that
// names the grant it was opened under.
func (s PG) GetPortalGrantByID(ctx context.Context, id uuid.UUID, now time.Time) (types.PortalGrant, error) {
	return scanPortalGrant(s.Pool.QueryRow(ctx, `
		SELECT `+portalGrantCols+`
		  FROM portal_grants g JOIN delegates d ON d.id = g.delegate_id AND d.revoked_at IS NULL AND d.issuer <> ''
		 WHERE g.id = $1 AND g.expires_at > $2`, id, now))
}

// PortalChainLive answers whether a renewable session's chain is still live: end is the latest expires_at
// among the grants of (delegate, principal, chain) that are unexpired on the database clock, or the zero time
// when there is none or the portal is revoked, and renewal is the portal's ui_session_renewal. A portal that
// is not live answers (zero, false).
func (s PG) PortalChainLive(ctx context.Context, delegateID uuid.UUID, principal string, chain uuid.UUID) (time.Time, bool, error) {
	var end *time.Time
	var renewal *bool
	err := s.Pool.QueryRow(ctx, `
		SELECT max(g.expires_at), bool_or(d.ui_session_renewal)
		  FROM delegates d
		  LEFT JOIN portal_grants g ON g.delegate_id = d.id AND g.principal = $2 AND g.chain_id = $3 AND g.expires_at > clock_timestamp()
		 WHERE d.id = $1 AND d.revoked_at IS NULL AND d.issuer <> ''`, delegateID, principal, chain).Scan(&end, &renewal)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: read portal chain: %w", err)
	}
	var out time.Time
	if end != nil {
		out = *end
	}
	return out, renewal != nil && *renewal, nil
}

// StampPortalUsed sets the portal's last_used_at to at, at most once a minute and never waiting: a row that
// is locked, or was stamped less than a minute before at, is left alone. It is one statement the exchange
// runs after its mint commits; a failure is the caller's to log. The column is display only.
func (s PG) StampPortalUsed(ctx context.Context, id uuid.UUID, at time.Time) error {
	if _, err := s.Pool.Exec(ctx, `
		UPDATE delegates SET last_used_at = $1
		 WHERE id = (SELECT id FROM delegates WHERE id = $2 AND (last_used_at IS NULL OR last_used_at < $1 - interval '1 minute') FOR NO KEY UPDATE SKIP LOCKED)`,
		at, id); err != nil {
		return fmt.Errorf("store: stamp portal use: %w", err)
	}
	return nil
}

// SweepExpiredPortalGrants deletes every grant that ended at or before at and reports how many. It runs after
// a mint commits, never inside it, so two people's mints never wait on each other's deletes; chain inheritance
// reads live grants only, so the sweep's timing changes nothing.
func (s PG) SweepExpiredPortalGrants(ctx context.Context, at time.Time) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM portal_grants WHERE expires_at <= $1`, at)
	if err != nil {
		return 0, fmt.Errorf("store: sweep portal grants: %w", err)
	}
	return tag.RowsAffected(), nil
}

// portalPersonLockKey is the SQL for the advisory lock key of one portal person, from the SQL expression of the
// person's principal. The mint and the credential erase both take this lock, so it must not depend on a row
// that may not exist yet.
func portalPersonLockKey(principal string) string {
	return `hashtextextended('portal-person:' || ` + principal + `, 0)`
}
