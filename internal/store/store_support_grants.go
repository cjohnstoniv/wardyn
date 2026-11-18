// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Support grants (migration 0293): an admin's request to help inside someone else's running run, and the
// owner's answer. Every time is written and compared by the database's clock, so a replica's clock decides
// nothing about access.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

// SupportGrant is one row of run_support_grants. It is requested while AcceptedAt is nil, accepted once the
// owner answered, and open until EndedAt is set.
type SupportGrant struct {
	ID            uuid.UUID
	RunID         uuid.UUID
	Helper        string
	HelperEmail   string
	OwnerEmail    string // set at accept
	Reason        string
	DoorsAsked    []string
	Doors         []string // nil until accepted; a subset of DoorsAsked
	Minutes       int      // 0 until accepted
	RequestedAt   time.Time
	RequestRevSeq int64
	AcceptedAt    *time.Time
	AcceptRevSeq  int64
	ExpiresAt     *time.Time
	LastSeenAt    *time.Time
	EndedAt       *time.Time
	EndReason     string
	EndAudited    bool // true only when the route that ended the grant wrote its own audit row
}

// SupportGrantRef names an open accepted grant by its run, for the audit stamp.
type SupportGrantRef struct {
	RunID uuid.UUID
	ID    uuid.UUID
}

// ErrSupportGrantOpen is RequestSupportGrant's answer when the run already has an open grant.
var ErrSupportGrantOpen = errors.New("store: the run already has an open support grant")

// SupportGrantStore is the support grant record. Optional like AttachLeaseStore: the api layer type-asserts,
// and a store without it answers every door's support check as unavailable.
type SupportGrantStore interface {
	// RequestSupportGrant first ends the run's lapsed request (unanswered for 15 minutes) and expired grant,
	// then inserts g (a nil ID is given a new one; g.RequestedAt is the database's). It returns the row, and
	// ErrSupportGrantOpen when another grant of the run is still open.
	RequestSupportGrant(ctx context.Context, g SupportGrant) (SupportGrant, error)
	// AcceptSupportGrant answers a requested grant the run's owner (created_by) may still accept: unanswered
	// less than 15 minutes, the run RUNNING and not lost, doors a subset of those asked. It returns the row;
	// ErrNotFound when none of that holds. minutes must be 15, 30 or 60.
	AcceptSupportGrant(ctx context.Context, run, grant uuid.UUID, owner, ownerEmail string, doors []string, minutes int, revSeq int64) (SupportGrant, error)
	// DeclineSupportGrant ends a requested grant of owner's run as declined, audited. ErrNotFound when it is
	// not an open, unanswered grant of that run and owner.
	DeclineSupportGrant(ctx context.Context, run, grant uuid.UUID, owner string) (SupportGrant, error)
	// EndSupportGrant ends an open grant of the run with reason. audited is true only from the end route,
	// which writes support.end itself. ErrNotFound when the grant is not open.
	EndSupportGrant(ctx context.Context, run, grant uuid.UUID, reason string, audited bool) (SupportGrant, error)
	// SupportGrantLive reads helper's open grant on run (grant uuid.Nil: whichever it is). found is false when
	// there is none. live is true for an accepted grant before expires_at on a RUNNING, not lost run; an
	// accepted grant that is not live is ended at once ('run_ended' when the run is over, else 'expired'),
	// and the row returned says so. stamp moves last_seen_at when it is unset or older than 30 seconds.
	SupportGrantLive(ctx context.Context, run uuid.UUID, helper string, grant uuid.UUID, stamp bool) (g SupportGrant, found, live bool, err error)
	// SupportGrantStatus returns the run's open grant and its last ended one, each nil when there is none.
	SupportGrantStatus(ctx context.Context, run uuid.UUID) (open, ended *SupportGrant, err error)
	// SweepSupportGrants ends the open grants of runs that are not RUNNING or are lost, then lapsed and
	// expired ones, then (when off) every other open one as 'support_off'; and claims up to 100 ended grants
	// no route audited, returning them with EndAudited now true.
	SweepSupportGrants(ctx context.Context, off bool) ([]SupportGrant, error)
	// LiveSupportGrants lists every open accepted grant that has not expired.
	LiveSupportGrants(ctx context.Context) ([]SupportGrantRef, error)
	// SupportHelperSince reports whether principal was ever accepted as helper on run, ended or not, by a
	// grant requested at or before at. A zero at is any accepted grant.
	SupportHelperSince(ctx context.Context, run uuid.UUID, principal string, at time.Time) (bool, error)
}

var _ SupportGrantStore = PG{}

// supportGrantCols is every column of a grant as a g-aliased row, in scanSupportGrant's order.
const supportGrantCols = `g.id, g.run_id, g.helper, g.helper_email, g.owner_email, g.reason, g.doors_asked, g.doors,
	COALESCE(g.minutes, 0), g.requested_at, g.request_rev_seq, g.accepted_at, g.accept_rev_seq, g.expires_at,
	g.last_seen_at, g.ended_at, COALESCE(g.end_reason, ''), g.end_audited`

// supportGrantRequestWindow is how long a request waits for its owner.
const supportGrantRequestWindow = `interval '15 minutes'`

func scanSupportGrant(row interface{ Scan(...any) error }, extra ...any) (SupportGrant, error) {
	var g SupportGrant
	err := row.Scan(append([]any{&g.ID, &g.RunID, &g.Helper, &g.HelperEmail, &g.OwnerEmail, &g.Reason, &g.DoorsAsked, &g.Doors,
		&g.Minutes, &g.RequestedAt, &g.RequestRevSeq, &g.AcceptedAt, &g.AcceptRevSeq, &g.ExpiresAt,
		&g.LastSeenAt, &g.EndedAt, &g.EndReason, &g.EndAudited}, extra...)...)
	return g, err
}

// oneSupportGrant runs a statement that returns at most one grant: ErrNotFound for none.
func (s PG) oneSupportGrant(ctx context.Context, what, sql string, args ...any) (SupportGrant, error) {
	g, err := scanSupportGrant(s.Pool.QueryRow(ctx, sql, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return SupportGrant{}, ErrNotFound
	}
	if err != nil {
		return SupportGrant{}, fmt.Errorf("store: %s: %w", what, err)
	}
	return g, nil
}

func (s PG) manySupportGrants(ctx context.Context, what, sql string, args ...any) ([]SupportGrant, error) {
	rows, err := s.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("store: %s: %w", what, err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (SupportGrant, error) { return scanSupportGrant(row) })
	if err != nil {
		return nil, fmt.Errorf("store: %s: %w", what, err)
	}
	return out, nil
}

// RequestSupportGrant — see SupportGrantStore.
func (s PG) RequestSupportGrant(ctx context.Context, g SupportGrant) (SupportGrant, error) {
	if g.ID == uuid.Nil {
		g.ID = uuid.New()
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return SupportGrant{}, fmt.Errorf("store: request a support grant: %w", err)
	}
	defer db.Rollback(ctx, tx) // a no-op after Commit
	if _, err := tx.Exec(ctx, `UPDATE run_support_grants
		SET ended_at = now(), end_reason = CASE WHEN accepted_at IS NULL THEN 'lapsed' ELSE 'expired' END
		WHERE run_id = $1 AND ended_at IS NULL
		  AND ((accepted_at IS NULL AND requested_at <= now() - `+supportGrantRequestWindow+`) OR expires_at <= now())`,
		g.RunID); err != nil {
		return SupportGrant{}, fmt.Errorf("store: end a lapsed support grant: %w", err)
	}
	out, err := scanSupportGrant(tx.QueryRow(ctx, `INSERT INTO run_support_grants AS g
			(id, run_id, helper, helper_email, reason, doors_asked, request_rev_seq)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING `+supportGrantCols,
		g.ID, g.RunID, g.Helper, g.HelperEmail, g.Reason, g.DoorsAsked, g.RequestRevSeq))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "run_support_grants_one_open" {
		return SupportGrant{}, ErrSupportGrantOpen
	}
	if err != nil {
		return SupportGrant{}, fmt.Errorf("store: request a support grant: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return SupportGrant{}, fmt.Errorf("store: request a support grant: %w", err)
	}
	return out, nil
}

// AcceptSupportGrant — see SupportGrantStore.
func (s PG) AcceptSupportGrant(ctx context.Context, run, grant uuid.UUID, owner, ownerEmail string, doors []string, minutes int, revSeq int64) (SupportGrant, error) {
	if doors == nil {
		doors = []string{}
	}
	return s.oneSupportGrant(ctx, "accept a support grant", `
		UPDATE run_support_grants g
		SET accepted_at = now(), expires_at = now() + make_interval(mins => $4), minutes = $4,
		    doors = $5, accept_rev_seq = $6, owner_email = $7
		FROM agent_runs r
		WHERE g.id = $1 AND g.run_id = $2 AND g.ended_at IS NULL AND g.accepted_at IS NULL
		  AND g.requested_at > now() - `+supportGrantRequestWindow+`
		  AND r.id = g.run_id AND r.state = 'RUNNING' AND r.lost_at IS NULL AND r.created_by = $3
		  AND $5::text[] <@ g.doors_asked
		RETURNING `+supportGrantCols,
		grant, run, owner, minutes, doors, revSeq, ownerEmail)
}

// DeclineSupportGrant — see SupportGrantStore.
func (s PG) DeclineSupportGrant(ctx context.Context, run, grant uuid.UUID, owner string) (SupportGrant, error) {
	return s.oneSupportGrant(ctx, "decline a support grant", `
		UPDATE run_support_grants g SET ended_at = now(), end_reason = 'declined', end_audited = true
		FROM agent_runs r
		WHERE g.id = $1 AND g.run_id = $2 AND g.ended_at IS NULL AND g.accepted_at IS NULL
		  AND r.id = g.run_id AND r.created_by = $3
		RETURNING `+supportGrantCols, grant, run, owner)
}

// EndSupportGrant — see SupportGrantStore.
func (s PG) EndSupportGrant(ctx context.Context, run, grant uuid.UUID, reason string, audited bool) (SupportGrant, error) {
	return s.oneSupportGrant(ctx, "end a support grant", `
		UPDATE run_support_grants g SET ended_at = now(), end_reason = $3, end_audited = $4
		WHERE g.id = $1 AND g.run_id = $2 AND g.ended_at IS NULL
		RETURNING `+supportGrantCols, grant, run, reason, audited)
}

// SupportGrantLive — see SupportGrantStore.
func (s PG) SupportGrantLive(ctx context.Context, run uuid.UUID, helper string, grant uuid.UUID, stamp bool) (SupportGrant, bool, bool, error) {
	var arg *uuid.UUID
	if grant != uuid.Nil {
		arg = &grant
	}
	var live, over bool
	g, err := scanSupportGrant(s.Pool.QueryRow(ctx, `
		SELECT `+supportGrantCols+`,
		       (g.accepted_at IS NOT NULL AND now() < g.expires_at AND r.state = 'RUNNING' AND r.lost_at IS NULL),
		       (r.state <> 'RUNNING' OR r.lost_at IS NOT NULL)
		FROM run_support_grants g JOIN agent_runs r ON r.id = g.run_id
		WHERE g.run_id = $1 AND g.helper = $2 AND g.ended_at IS NULL AND ($3::uuid IS NULL OR g.id = $3)`,
		run, helper, arg), &live, &over)
	if errors.Is(err, pgx.ErrNoRows) {
		return SupportGrant{}, false, false, nil
	}
	if err != nil {
		return SupportGrant{}, false, false, fmt.Errorf("store: read a support grant: %w", err)
	}
	if g.AcceptedAt != nil && !live {
		// M1's lazy end: left audited-false, so the sweep writes the support.end row.
		reason := "expired"
		if over {
			reason = "run_ended"
		}
		ended, err := s.oneSupportGrant(ctx, "end a support grant that is no longer live", `
			UPDATE run_support_grants g SET ended_at = now(), end_reason = $2
			WHERE g.id = $1 AND g.ended_at IS NULL RETURNING `+supportGrantCols, g.ID, reason)
		if errors.Is(err, ErrNotFound) { // another replica ended it first: read what it wrote
			ended, err = s.oneSupportGrant(ctx, "read a support grant", `SELECT `+supportGrantCols+` FROM run_support_grants g WHERE g.id = $1`, g.ID)
		}
		if err != nil {
			return SupportGrant{}, false, false, err
		}
		return ended, true, false, nil
	}
	if stamp && live {
		var seen time.Time
		err := s.Pool.QueryRow(ctx, `UPDATE run_support_grants SET last_seen_at = now()
			WHERE id = $1 AND ended_at IS NULL AND (last_seen_at IS NULL OR last_seen_at < now() - interval '30 seconds')
			RETURNING last_seen_at`, g.ID).Scan(&seen)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return SupportGrant{}, false, false, fmt.Errorf("store: stamp a support grant: %w", err)
		}
		if err == nil {
			g.LastSeenAt = &seen
		}
	}
	return g, true, live, nil
}

// SupportGrantStatus — see SupportGrantStore.
func (s PG) SupportGrantStatus(ctx context.Context, run uuid.UUID) (*SupportGrant, *SupportGrant, error) {
	var open, ended *SupportGrant
	for _, q := range []struct {
		into *(*SupportGrant)
		sql  string
	}{
		{&open, `AND g.ended_at IS NULL`},
		{&ended, `AND g.ended_at IS NOT NULL ORDER BY g.ended_at DESC, g.requested_at DESC LIMIT 1`},
	} {
		g, err := s.oneSupportGrant(ctx, "read a run's support grants",
			`SELECT `+supportGrantCols+` FROM run_support_grants g WHERE g.run_id = $1 `+q.sql, run)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		*q.into = &g
	}
	return open, ended, nil
}

// SweepSupportGrants — see SupportGrantStore.
func (s PG) SweepSupportGrants(ctx context.Context, off bool) ([]SupportGrant, error) {
	steps := []struct{ what, sql string }{
		{"end the support grants of ended runs", `UPDATE run_support_grants g SET ended_at = now(), end_reason = 'run_ended'
			FROM agent_runs r WHERE r.id = g.run_id AND g.ended_at IS NULL AND (r.state <> 'RUNNING' OR r.lost_at IS NOT NULL)`},
		{"end the lapsed and expired support grants", `UPDATE run_support_grants
			SET ended_at = now(), end_reason = CASE WHEN accepted_at IS NULL THEN 'lapsed' ELSE 'expired' END
			WHERE ended_at IS NULL
			  AND ((accepted_at IS NULL AND requested_at <= now() - ` + supportGrantRequestWindow + `) OR expires_at <= now())`},
	}
	for _, st := range steps {
		if _, err := s.Pool.Exec(ctx, st.sql); err != nil {
			return nil, fmt.Errorf("store: %s: %w", st.what, err)
		}
	}
	if off {
		if _, err := s.Pool.Exec(ctx, `UPDATE run_support_grants SET ended_at = now(), end_reason = 'support_off'
			WHERE ended_at IS NULL`); err != nil {
			return nil, fmt.Errorf("store: end the support grants of a switched-off feature: %w", err)
		}
	}
	return s.manySupportGrants(ctx, "claim the ended support grants", `
		WITH c AS (SELECT id FROM run_support_grants WHERE ended_at IS NOT NULL AND NOT end_audited
			ORDER BY ended_at, id LIMIT 100 FOR UPDATE SKIP LOCKED)
		UPDATE run_support_grants g SET end_audited = true FROM c WHERE g.id = c.id
		RETURNING `+supportGrantCols)
}

// LiveSupportGrants — see SupportGrantStore.
func (s PG) LiveSupportGrants(ctx context.Context) ([]SupportGrantRef, error) {
	rows, err := s.Pool.Query(ctx, `SELECT run_id, id FROM run_support_grants
		WHERE ended_at IS NULL AND accepted_at IS NOT NULL AND expires_at > now()`)
	if err != nil {
		return nil, fmt.Errorf("store: list the live support grants: %w", err)
	}
	refs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[SupportGrantRef])
	if err != nil {
		return nil, fmt.Errorf("store: list the live support grants: %w", err)
	}
	return refs, nil
}

// SupportHelperSince — see SupportGrantStore. A zero at goes in as SQL NULL: Go's zero time is year 1,
// which would make "requested at or before at" false for every grant.
func (s PG) SupportHelperSince(ctx context.Context, run uuid.UUID, principal string, at time.Time) (bool, error) {
	var since *time.Time
	if !at.IsZero() {
		since = &at
	}
	var ok bool
	if err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM run_support_grants
		WHERE run_id = $1 AND helper = $2 AND accepted_at IS NOT NULL AND ($3::timestamptz IS NULL OR requested_at <= $3))`,
		run, principal, since).Scan(&ok); err != nil {
		return false, fmt.Errorf("store: read a support helper: %w", err)
	}
	return ok, nil
}
