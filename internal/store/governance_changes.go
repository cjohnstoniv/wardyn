// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Governance changes (migration 0126): a governance write held for a second human. The store owns the
// parts that must be one transaction: proposing (expire a lapsed change at the same target, then
// insert) and deciding (lock the change row, require it pending and unexpired, run the target's apply,
// move the state, commit). It knows nothing about any target: the apply a caller hands DecideGovernanceChange
// is that target's own code, run on the decision transaction.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

const governanceChangeCols = `id, target_kind, op, target_key, payload, diff, base_hash, deployment_hash,
	proposed_by, proposed_by_email, proposed_at, expires_at, state, decided_by, decided_by_email, decided_at, reason`

// governanceChangeColsRead is governanceChangeCols with an expired-but-unswept pending row reported as
// expired: a read computes expiry from expires_at, so no sweeper is needed for correctness.
const governanceChangeColsRead = `id, target_kind, op, target_key, payload, diff, base_hash, deployment_hash,
	proposed_by, proposed_by_email, proposed_at, expires_at,
	CASE WHEN state = 'pending' AND expires_at <= now() THEN 'expired' ELSE state END,
	decided_by, decided_by_email, decided_at, reason`

// maxGovernanceChangeList bounds one list read; the newest changes come first.
const maxGovernanceChangeList = 500

// ErrGovernanceChangePending is a proposal's refusal when a live change already holds the target: ID is
// that change.
type ErrGovernanceChangePending struct{ ID uuid.UUID }

func (e *ErrGovernanceChangePending) Error() string {
	return "store: a governance change is already pending for this target: " + e.ID.String()
}

// ErrGovernanceChangeNotPending is a decision's refusal when the change is no longer pending. State is
// what it is now; Lapsed is true when this very decision found it past expires_at and moved it to
// expired.
type ErrGovernanceChangeNotPending struct {
	State  string
	Lapsed bool
}

func (e *ErrGovernanceChangeNotPending) Error() string {
	return "store: governance change is not pending (" + e.State + ")"
}

// ErrGovernanceChangeStale is what a decision's apply returns when the target is no longer what the
// proposal reviewed. DecideGovernanceChange moves the change to stale and commits that, and returns
// this error.
var ErrGovernanceChangeStale = errors.New("store: the governance change is stale")

// GovernanceDecision is what a decision records on the change row.
type GovernanceDecision struct {
	// To is types.GovernanceChangeApplied or types.GovernanceChangeRejected.
	To      string
	By      string
	ByEmail string
	Reason  string
}

// GovernanceDecideFunc runs inside the decision transaction with the change as it was locked. A nil
// error commits the decision; ErrGovernanceChangeStale commits the change as stale; any other error
// rolls everything back, the change still pending.
type GovernanceDecideFunc func(q Querier, ch types.GovernanceChange) error

// ProposeGovernanceChange stores ch as pending, expiring ttl from now on the database's clock. In the
// same transaction it first moves any lapsed pending change for the same target to expired (their ids
// are returned, for the audit rows), so a live one is the only thing the one-pending-per-target index
// can refuse: that is *ErrGovernanceChangePending naming it.
func (s PG) ProposeGovernanceChange(ctx context.Context, ch types.GovernanceChange, ttl time.Duration) (types.GovernanceChange, []uuid.UUID, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return types.GovernanceChange{}, nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	rows, err := tx.Query(ctx, `
		UPDATE governance_changes SET state = 'expired', decided_at = now()
		WHERE target_kind = $1 AND target_key = $2 AND state = 'pending' AND expires_at <= now()
		RETURNING id`, ch.TargetKind, ch.TargetKey)
	if err != nil {
		return types.GovernanceChange{}, nil, fmt.Errorf("store: expire lapsed governance changes: %w", err)
	}
	expired, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return types.GovernanceChange{}, nil, fmt.Errorf("store: expire lapsed governance changes: %w", err)
	}
	if ch.ID == uuid.Nil {
		ch.ID = uuid.New()
	}
	out, err := scanGovernanceChange(tx.QueryRow(ctx, `
		INSERT INTO governance_changes (id, target_kind, op, target_key, payload, diff, base_hash, deployment_hash,
			proposed_by, proposed_by_email, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10, now() + $11::bigint * interval '1 microsecond')
		RETURNING `+governanceChangeCols,
		ch.ID, ch.TargetKind, ch.Op, ch.TargetKey, ch.Payload, ch.Diff, ch.BaseHash, ch.DeploymentHash,
		ch.ProposedBy, ch.ProposedByEmail, ttl.Microseconds()))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return types.GovernanceChange{}, nil, s.pendingRefusal(ctx, ch)
		}
		return types.GovernanceChange{}, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return types.GovernanceChange{}, nil, err
	}
	return out, expired, nil
}

// pendingRefusal names the live change that holds ch's target; the failed transaction is gone, so it
// reads on the pool. A change that was decided in the meantime leaves a plain conflict.
func (s PG) pendingRefusal(ctx context.Context, ch types.GovernanceChange) error {
	var id uuid.UUID
	err := s.Pool.QueryRow(ctx, `SELECT id FROM governance_changes WHERE target_kind = $1 AND target_key = $2 AND state = 'pending'`,
		ch.TargetKind, ch.TargetKey).Scan(&id)
	if err != nil {
		return ErrConflict
	}
	return &ErrGovernanceChangePending{ID: id}
}

// ListGovernanceChanges returns changes newest first. state narrows to one state ("" is every state);
// a pending row past its expiry reads as expired.
func (s PG) ListGovernanceChanges(ctx context.Context, state string) ([]types.GovernanceChange, error) {
	q := `SELECT ` + governanceChangeColsRead + ` FROM governance_changes`
	var args []any
	if state != "" {
		q = `SELECT * FROM (` + q + `) c WHERE c.state = $1`
		args = append(args, state)
	}
	q += fmt.Sprintf(` ORDER BY proposed_at DESC, id LIMIT %d`, maxGovernanceChangeList)
	return collect(ctx, s.Pool, "list", "governance changes", q, args, scanGovernanceChange)
}

// GetGovernanceChange returns one change, ErrNotFound when none; a pending row past its expiry reads
// as expired.
func (s PG) GetGovernanceChange(ctx context.Context, id uuid.UUID) (types.GovernanceChange, error) {
	return scanGovernanceChange(s.Pool.QueryRow(ctx,
		`SELECT `+governanceChangeColsRead+` FROM governance_changes WHERE id = $1`, id))
}

// DecideGovernanceChange decides one change in one transaction:
//
//  1. lock the change row;
//  2. require it pending and unexpired. A lapsed pending row is moved to expired here and that is
//     committed, and the caller gets *ErrGovernanceChangeNotPending with Lapsed set;
//  3. run fn on the transaction (the target's checks and apply);
//  4. move the row to d.To, stamping the decider and the time;
//  5. commit.
//
// fn returning ErrGovernanceChangeStale moves the row to stale instead and commits that. Any other
// error from fn, or from the state move, rolls the whole transaction back: a target written by fn is
// then unwritten and the change is still pending. ErrNotFound when no such change.
func (s PG) DecideGovernanceChange(ctx context.Context, id uuid.UUID, d GovernanceDecision, fn GovernanceDecideFunc) (types.GovernanceChange, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return types.GovernanceChange{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	var lapsed bool
	cur, err := scanGovernanceChangeInto(tx.QueryRow(ctx,
		`SELECT `+governanceChangeCols+`, expires_at <= now() FROM governance_changes WHERE id = $1 FOR UPDATE`, id), &lapsed)
	if err != nil {
		return types.GovernanceChange{}, err
	}
	if cur.State != types.GovernanceChangePending {
		return cur, &ErrGovernanceChangeNotPending{State: cur.State}
	}
	if lapsed {
		if _, err := tx.Exec(ctx, `UPDATE governance_changes SET state = 'expired', decided_at = now() WHERE id = $1`, id); err != nil {
			return cur, fmt.Errorf("store: expire a governance change: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return cur, err
		}
		cur.State = types.GovernanceChangeExpired
		return cur, &ErrGovernanceChangeNotPending{State: types.GovernanceChangeExpired, Lapsed: true}
	}
	if fn != nil {
		if ferr := fn(tx, cur); ferr != nil {
			if !errors.Is(ferr, ErrGovernanceChangeStale) {
				return cur, ferr
			}
			// The target was never written (the staleness check precedes the apply), so this commit
			// carries only the state.
			if _, err := tx.Exec(ctx, `UPDATE governance_changes SET state = 'stale', decided_at = now() WHERE id = $1`, id); err != nil {
				return cur, fmt.Errorf("store: mark a governance change stale: %w", err)
			}
			if err := tx.Commit(ctx); err != nil {
				return cur, err
			}
			cur.State = types.GovernanceChangeStale
			return cur, ferr
		}
	}
	out, err := scanGovernanceChange(tx.QueryRow(ctx, `
		UPDATE governance_changes
		SET state = $2, decided_by = $3, decided_by_email = $4, decided_at = now(), reason = $5
		WHERE id = $1 AND state = 'pending'
		RETURNING `+governanceChangeCols, id, d.To, d.By, d.ByEmail, d.Reason))
	if err != nil {
		return cur, fmt.Errorf("store: record a governance decision: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return cur, err
	}
	return out, nil
}

// DryRunGovernance runs fn on a transaction that is always rolled back, and returns fn's error. It is
// how a proposal learns whether a write would be accepted (and what it would produce) by running the
// very code that applies it, with nothing left behind.
func (s PG) DryRunGovernance(ctx context.Context, fn func(q Querier) error) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // always rolled back
	return fn(tx)
}

// EraseGovernanceChangePersonalFields clears the proposer and decider (and their emails) of every
// change whose recorded principal is one of names or whose recorded email matches one of them (case
// folded), and returns how many rows it touched. A pending change whose proposer is erased leaves
// pending in the same statement (to expired), so a change with no recorded proposer can never be
// approved. Idempotent: a row already cleared matches nothing.
func (s PG) EraseGovernanceChangePersonalFields(ctx context.Context, names []string) (int, error) {
	if len(names) == 0 {
		return 0, nil
	}
	tag, err := s.Pool.Exec(ctx, `
		UPDATE governance_changes SET
			state = CASE WHEN state = 'pending' AND (proposed_by = ANY($1) OR lower(proposed_by_email) = ANY($2))
			             THEN 'expired' ELSE state END,
			decided_at = CASE WHEN state = 'pending' AND (proposed_by = ANY($1) OR lower(proposed_by_email) = ANY($2))
			                  THEN now() ELSE decided_at END,
			proposed_by       = CASE WHEN proposed_by = ANY($1) OR lower(proposed_by_email) = ANY($2) THEN '' ELSE proposed_by END,
			proposed_by_email = CASE WHEN proposed_by = ANY($1) OR lower(proposed_by_email) = ANY($2) THEN '' ELSE proposed_by_email END,
			decided_by        = CASE WHEN decided_by = ANY($1) OR lower(decided_by_email) = ANY($2) THEN '' ELSE decided_by END,
			decided_by_email  = CASE WHEN decided_by = ANY($1) OR lower(decided_by_email) = ANY($2) THEN '' ELSE decided_by_email END
		WHERE proposed_by = ANY($1) OR lower(proposed_by_email) = ANY($2)
		   OR decided_by = ANY($1) OR lower(decided_by_email) = ANY($2)`,
		names, lowerAll(names))
	if err != nil {
		return 0, fmt.Errorf("store: erase governance change personal fields: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

func lowerAll(in []string) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = strings.ToLower(v)
	}
	return out
}

func scanGovernanceChange(row pgx.Row) (types.GovernanceChange, error) {
	return scanGovernanceChangeInto(row, nil)
}

// scanGovernanceChangeInto is scanGovernanceChange plus one trailing boolean column some reads add.
func scanGovernanceChangeInto(row pgx.Row, extra *bool) (types.GovernanceChange, error) {
	var c types.GovernanceChange
	dest := []any{&c.ID, &c.TargetKind, &c.Op, &c.TargetKey, &c.Payload, &c.Diff, &c.BaseHash, &c.DeploymentHash,
		&c.ProposedBy, &c.ProposedByEmail, &c.ProposedAt, &c.ExpiresAt, &c.State,
		&c.DecidedBy, &c.DecidedByEmail, &c.DecidedAt, &c.Reason}
	if extra != nil {
		dest = append(dest, extra)
	}
	err := row.Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.GovernanceChange{}, ErrNotFound
	}
	if err != nil {
		return types.GovernanceChange{}, fmt.Errorf("store: scan governance change: %w", err)
	}
	return c, nil
}
