// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SuspensionBounds is what a SCIM suspension's tail reads to reach only what it owed: the database's own
// insert time of runs, API tokens and SSH keys (inserted_at, migration 0281), a column default the application
// never writes, so on the same clock as an identity's deactivated_at; whether a session cutoff of a sub was
// written at or after a given instant while the identity is active again; and a step finish that a later
// suspension's ledger refuses. Optional, like LeaverStore: callers type-assert it.
type SuspensionBounds interface {
	RunsInsertedAfter(ctx context.Context, identity uuid.UUID, ids []uuid.UUID, at time.Time) (map[uuid.UUID]bool, error)
	APITokensInsertedAfter(ctx context.Context, identity uuid.UUID, ids []uuid.UUID, at time.Time) (map[uuid.UUID]bool, error)
	DeleteSSHKeysUpTo(ctx context.Context, identity uuid.UUID, principal string, at time.Time) (int, error)
	SubCutSince(ctx context.Context, identity uuid.UUID, sub string, at time.Time) (bool, error)
	FinishSuspensionJobAt(ctx context.Context, identity uuid.UUID, k JobKey, detail map[string]int, at time.Time) error
}

// personActiveSQL: the identity ($3) a suspension tail works for is active again. Only then is a row stamped
// after the tail's bound one a reactivated person made, and only then is a form's session cut not written
// again; while the person is deactivated nothing is spared, since not every insert is refused then (a service
// token's, or one under a principal no identity row binds).
const personActiveSQL = `EXISTS (SELECT 1 FROM principal_identities WHERE id = $3 AND deactivated_at IS NULL AND purged_at IS NULL)`

// RunsInsertedAfter is the subset of ids whose agent_runs row the database stamped strictly after at, read in
// the statement that finds the identity active again. A row stamped at at itself is not in it, nor is a row
// inserted before the stamp existed (NULL), and none is while the identity is deactivated.
func (s PG) RunsInsertedAfter(ctx context.Context, identity uuid.UUID, ids []uuid.UUID, at time.Time) (map[uuid.UUID]bool, error) {
	return insertedAfter(ctx, s, "runs", `SELECT id FROM agent_runs WHERE id = ANY($1::uuid[]) AND inserted_at > $2 AND `+personActiveSQL, identity, ids, at)
}

// APITokensInsertedAfter is RunsInsertedAfter for api_tokens rows.
func (s PG) APITokensInsertedAfter(ctx context.Context, identity uuid.UUID, ids []uuid.UUID, at time.Time) (map[uuid.UUID]bool, error) {
	return insertedAfter(ctx, s, "api tokens", `SELECT id FROM api_tokens WHERE id = ANY($1::uuid[]) AND inserted_at > $2 AND `+personActiveSQL, identity, ids, at)
}

func insertedAfter(ctx context.Context, s PG, noun, q string, identity uuid.UUID, ids []uuid.UUID, at time.Time) (map[uuid.UUID]bool, error) {
	after, err := collect(ctx, s.Pool, "read insert stamps of", noun, q, []any{ids, at, identity}, func(row pgx.Row) (uuid.UUID, error) {
		var id uuid.UUID
		return id, row.Scan(&id)
	})
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]bool, len(after))
	for _, id := range after {
		out[id] = true
	}
	return out, nil
}

// DeleteSSHKeysUpTo is DeleteSSHKeys for the keys the database stamped at or before at, and those inserted
// before the stamp existed (NULL), in one statement; every key of principal goes while the identity is
// deactivated.
func (s PG) DeleteSSHKeysUpTo(ctx context.Context, identity uuid.UUID, principal string, at time.Time) (int, error) {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM ssh_public_keys WHERE ($1 = '' OR principal = $1) AND (inserted_at IS NULL OR inserted_at <= $2 OR NOT `+personActiveSQL+`)`,
		principal, at, identity)
	if err != nil {
		return 0, fmt.Errorf("store: delete ssh keys up to an instant: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// SubCutSince says sub carries a session cutoff written at or after at, read in the statement that finds the
// identity active again. While the identity is deactivated it is false, so the tail cuts again as before the
// bound existed, and a failed cut fails its step. Nothing deletes a cutoff row; a false answer leads the
// caller to write one, so a cutoff a racing write set back is cut again.
func (s PG) SubCutSince(ctx context.Context, identity uuid.UUID, sub string, at time.Time) (bool, error) {
	var cut bool
	if err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM oidc_session_revocations WHERE sub = $1 AND revoked_at >= $2) AND `+personActiveSQL,
		sub, at, identity).Scan(&cut); err != nil {
		return false, fmt.Errorf("store: read session cutoff: %w", err)
	}
	return cut, nil
}

// FinishSuspensionJobAt is FinishDeprovisionJob for a suspension tail bound at at: in one statement, it closes the
// step only while the identity is still deactivated at at or, once the identity is active again, while the
// ledger's cutoff step still records at as the deactivation it left (SuspendIdentity writes the record in the
// transaction that resets the ledger, and a reactivation keeps it). Only a reactivation lets a later suspension
// reset the ledger, and that suspension records its own deactivation, so a tail bound to an earlier one closes
// none of its steps, also once the person is active again. Nor does it close one while the identity is
// deactivated at another instant, which a suspension of another identity can leave without resetting this
// ledger: a pass bound to that instant owes more than this tail did. A step the tail may not close, or whose row
// a later suspension replaced, is ErrSuperseded.
func (s PG) FinishSuspensionJobAt(ctx context.Context, identity uuid.UUID, k JobKey, detail map[string]int, at time.Time) error {
	if detail == nil {
		detail = map[string]int{}
	}
	raw, _ := json.Marshal(detail)
	tag, err := s.Pool.Exec(ctx, `
		UPDATE deprovision_jobs SET state = 'done', attempts = attempts + 1, last_error = '', detail = $4, updated_at = now()
		 WHERE identity_id = $1 AND kind = 'suspend' AND step = $2 AND target = $3
		   AND EXISTS (SELECT 1 FROM principal_identities i WHERE i.id = $1 AND (i.deactivated_at = $5 OR (i.deactivated_at IS NULL
		                AND EXISTS (SELECT 1 FROM deprovision_jobs c WHERE c.identity_id = $1 AND c.kind = 'suspend' AND c.step = 'cutoff' AND c.target = ''
		                             AND c.detail->'deactivated_at_us' = to_jsonb((extract(epoch FROM $5::timestamptz) * 1000000)::bigint)))))`,
		identity, k.Step, k.Target, raw, at)
	if err != nil {
		return fmt.Errorf("store: finish suspension job: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSuperseded
	}
	return nil
}
