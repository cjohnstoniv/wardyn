// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The store half of a purge: the tombstone mark, the two candidate reads the purge sweeper makes, and
// the direct delete of a person's user-subject grants and assignments.

// JobKindPurge is the ledger kind of a purge; JobStepAuditDeprovision is the step it ends on, and the
// one MarkIdentityPurged records pending so a purge that dies right after the mark is still on the ledger.
const (
	JobKindPurge            = "purge"
	JobStepAuditDeprovision = "audit_deprovision"
)

// PendingLeaver is an identity with ledger rows still pending: a suspension its identity provider
// stopped retrying, or a purge begun and not finished.
type PendingLeaver struct {
	ID     uuid.UUID
	Purged bool
}

// MarkIdentityPurged turns the identity into a permanent tombstone (purged_at set) and records the
// purge's last step as pending, in one transaction that holds the row FOR UPDATE. It reports whether
// this call marked it: false when the row was already purged and, with requireDue, when it is not
// (still) deactivated with a purge_after that has passed. The re-check under the lock is what lets a
// reactivation that committed first win; once the mark commits a reactivation is ErrIdentityPurged.
func (s PG) MarkIdentityPurged(ctx context.Context, id uuid.UUID, requireDue bool) (bool, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, fmt.Errorf("store: mark identity purged: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var purged, due bool
	err = tx.QueryRow(ctx, `
		SELECT purged_at IS NOT NULL, deactivated_at IS NOT NULL AND purge_after IS NOT NULL AND purge_after <= now()
		  FROM principal_identities WHERE id = $1 FOR UPDATE`, id).Scan(&purged, &due)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("store: lock identity: %w", err)
	}
	if purged || (requireDue && !due) {
		return false, nil
	}
	if _, err = tx.Exec(ctx, `UPDATE principal_identities SET purged_at = clock_timestamp() WHERE id = $1`, id); err != nil {
		return false, fmt.Errorf("store: mark identity purged: %w", err)
	}
	if _, err = tx.Exec(ctx, `
		INSERT INTO deprovision_jobs (identity_id, kind, step, target) VALUES ($1, $2, $3, '')
		ON CONFLICT (identity_id, kind, step, target) DO NOTHING`, id, JobKindPurge, JobStepAuditDeprovision); err != nil {
		return false, fmt.Errorf("store: record purge step: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: commit purge mark: %w", err)
	}
	return true, nil
}

// PurgeDueIdentities is the identities deactivated and not yet purged whose purge_after has passed,
// oldest first, at most limit. It is only a candidate read: MarkIdentityPurged re-checks under the lock.
func (s PG) PurgeDueIdentities(ctx context.Context, limit int) ([]uuid.UUID, error) {
	return collect(ctx, s.Pool, "list", "identities due for purge", `
		SELECT id FROM principal_identities
		 WHERE purged_at IS NULL AND deactivated_at IS NOT NULL AND purge_after IS NOT NULL AND purge_after <= now()
		 ORDER BY purge_after, id LIMIT $1`, []any{limit}, func(r pgx.Row) (uuid.UUID, error) {
		var id uuid.UUID
		return id, r.Scan(&id)
	})
}

// PendingLeavers is the identities with a suspend or purge ledger row still pending that no step has
// touched for idleFor (a request still working on it is not one the sweeper may race), at most limit.
// An identity that is neither deactivated nor purged has a stale ledger and is not listed.
func (s PG) PendingLeavers(ctx context.Context, idleFor time.Duration, limit int) ([]PendingLeaver, error) {
	return collect(ctx, s.Pool, "list", "pending leavers", `
		SELECT i.id, i.purged_at IS NOT NULL
		  FROM principal_identities i
		 WHERE (i.purged_at IS NOT NULL OR i.deactivated_at IS NOT NULL)
		   AND EXISTS (SELECT 1 FROM deprovision_jobs j WHERE j.identity_id = i.id AND j.state = 'pending'
		                  AND j.kind IN ('suspend', 'purge'))
		   AND NOT EXISTS (SELECT 1 FROM deprovision_jobs j WHERE j.identity_id = i.id
		                  AND j.updated_at > now() - $1::bigint * interval '1 microsecond')
		 ORDER BY i.created_at, i.id LIMIT $2`, []any{idleFor.Microseconds(), limit}, func(r pgx.Row) (PendingLeaver, error) {
		var p PendingLeaver
		return p, r.Scan(&p.ID, &p.Purged)
	})
}

// DeleteUserSubjectRows deletes the user-subject capability grants and governance assignments of
// subjects (matched case-insensitively), and says how many of each. A direct delete, never the GOV4
// apply path: the subject is a purged identity that can never authenticate again.
func (s PG) DeleteUserSubjectRows(ctx context.Context, subjects []string) (grants, assignments int64, err error) {
	lowered := make([]string, 0, len(subjects))
	for _, v := range subjects {
		if v = strings.ToLower(strings.TrimSpace(v)); v != "" {
			lowered = append(lowered, v)
		}
	}
	if len(lowered) == 0 {
		return 0, 0, nil
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, 0, fmt.Errorf("store: delete user subject rows: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `DELETE FROM capability_grants WHERE subject_type = 'user' AND lower(subject) = ANY($1::text[])`, lowered)
	if err != nil {
		return 0, 0, fmt.Errorf("store: delete user grants: %w", err)
	}
	grants = tag.RowsAffected()
	tag, err = tx.Exec(ctx, `DELETE FROM governance_assignments WHERE subject_type = 'user' AND lower(subject) = ANY($1::text[])`, lowered)
	if err != nil {
		return 0, 0, fmt.Errorf("store: delete user assignments: %w", err)
	}
	assignments = tag.RowsAffected()
	if err = tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("store: commit user subject delete: %w", err)
	}
	return grants, assignments, nil
}

// DeprovisionFailure is a ledger row of a suspension or purge that is still pending and has failed at
// least once: the step, and the error its last attempt ended on. The console's SCIM card lists them.
type DeprovisionFailure struct {
	IdentityID uuid.UUID
	Kind       string
	Step       string
	LastError  string
}

// ListDeactivatedIdentities is the identities deactivated and not purged, most recently deactivated
// first, at most limit.
func (s PG) ListDeactivatedIdentities(ctx context.Context, limit int) ([]PrincipalIdentity, error) {
	return collect(ctx, s.Pool, "list", "deactivated identities", `SELECT `+principalIdentityCols+`
		FROM principal_identities WHERE deactivated_at IS NOT NULL AND purged_at IS NULL
		ORDER BY deactivated_at DESC, id LIMIT $1`, []any{limit}, scanPrincipalIdentity)
}

// ListDeprovisionFailures is the suspend and purge ledger rows that are pending with a recorded error
// for an identity still deactivated or purged (the sweeper's scope: a reactivated person's leftover
// rows are not unfinished work), oldest identity first, at most limit. One row per identity, kind and step: a step that fails for
// several targets is listed once, with the error of the one touched last.
func (s PG) ListDeprovisionFailures(ctx context.Context, limit int) ([]DeprovisionFailure, error) {
	return collect(ctx, s.Pool, "list", "deprovision failures", `
		SELECT j.identity_id, j.kind, j.step, (array_agg(j.last_error ORDER BY j.updated_at DESC))[1]
		  FROM deprovision_jobs j JOIN principal_identities i ON i.id = j.identity_id
		 WHERE j.state = 'pending' AND j.last_error <> '' AND j.kind IN ('suspend', 'purge')
		   AND (i.deactivated_at IS NOT NULL OR i.purged_at IS NOT NULL)
		 GROUP BY j.identity_id, j.kind, j.step
		 ORDER BY min(j.created_at), j.identity_id, j.kind, j.step LIMIT $1`, []any{limit}, func(r pgx.Row) (DeprovisionFailure, error) {
		var f DeprovisionFailure
		return f, r.Scan(&f.IdentityID, &f.Kind, &f.Step, &f.LastError)
	})
}
