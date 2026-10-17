// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
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

// PendingRefusalAudit is an identity and ledger kind that still owe a refusal row (JobStepRefusalAudit).
type PendingRefusalAudit struct {
	IdentityID uuid.UUID
	Kind       string
}

// ownSuspension is true when identity row i's current deactivation is its own suspension's: the cutoff
// step a suspension of i records holds the deactivated_at it left on i (SuspendIdentity), and a
// reactivation followed by any later deactivation changes deactivated_at. A suspend row from an earlier
// suspension, or one written before the record existed, is no proof, however recently it was updated.
const ownSuspension = `EXISTS (SELECT 1 FROM deprovision_jobs j WHERE j.identity_id = i.id AND j.kind = 'suspend'
	AND j.step = 'cutoff' AND j.target = '' AND j.state = 'done'
	AND j.detail->'deactivated_at_us' = to_jsonb((extract(epoch FROM i.deactivated_at) * 1000000)::bigint))`

// MarkIdentityPurged turns the identity into a permanent tombstone (purged_at set) and records the
// purge's last step as pending, in one transaction that holds the row FOR UPDATE. It reports whether
// this call marked it: false when the row was already purged and, with requireDue, when it is not
// (still) deactivated by its own suspension (ownSuspension) with a purge_after that has passed. The
// re-check under the lock is what lets a reactivation that committed first win; once the mark commits a
// reactivation is ErrIdentityPurged.
func (s PG) MarkIdentityPurged(ctx context.Context, id uuid.UUID, requireDue bool) (bool, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, fmt.Errorf("store: mark identity purged: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var purged, due bool
	err = tx.QueryRow(ctx, `
		SELECT i.purged_at IS NOT NULL, i.deactivated_at IS NOT NULL AND i.purge_after IS NOT NULL AND i.purge_after <= now()
		       AND `+ownSuspension+`
		  FROM principal_identities i WHERE i.id = $1 FOR UPDATE`, id).Scan(&purged, &due)
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

// DueIdentity is one identity PurgeDueIdentities lists. Suspended says the identity's current
// deactivation is its own suspension's (ownSuspension). One without that proof was deactivated alongside
// another identity's suspension, or by a version that did not record it, and the sweeper does not purge it.
type DueIdentity struct {
	ID        uuid.UUID
	Suspended bool
}

// PurgeDueIdentities is the identities deactivated and not yet purged whose purge_after has passed, those
// deactivated by their own suspension first, then oldest first, at most limit. It is only a candidate
// read: MarkIdentityPurged re-checks under the lock.
func (s PG) PurgeDueIdentities(ctx context.Context, limit int) ([]DueIdentity, error) {
	return collect(ctx, s.Pool, "list", "identities due for purge", `
		SELECT i.id, `+ownSuspension+` AS suspended
		  FROM principal_identities i
		 WHERE i.purged_at IS NULL AND i.deactivated_at IS NOT NULL AND i.purge_after IS NOT NULL AND i.purge_after <= now()
		 ORDER BY suspended DESC, i.purge_after, i.id LIMIT $1`, []any{limit}, func(r pgx.Row) (DueIdentity, error) {
		var d DueIdentity
		return d, r.Scan(&d.ID, &d.Suspended)
	})
}

// PendingLeavers is the identities with a suspend or purge ledger row still pending that no step has
// touched for idleFor (a request still working on it is not one the sweeper may race), at most limit.
// An identity that is neither deactivated nor purged has a stale ledger and is not listed. The fewest
// failed attempts come first, so steps a retry cannot finish (a held refusal is retried every pass) do
// not fill the batch ahead of newer leavers.
func (s PG) PendingLeavers(ctx context.Context, idleFor time.Duration, limit int) ([]PendingLeaver, error) {
	return collect(ctx, s.Pool, "list", "pending leavers", `
		SELECT i.id, i.purged_at IS NOT NULL
		  FROM principal_identities i
		 WHERE (i.purged_at IS NOT NULL OR i.deactivated_at IS NOT NULL)
		   AND EXISTS (SELECT 1 FROM deprovision_jobs j WHERE j.identity_id = i.id AND j.state = 'pending'
		                  AND j.kind IN ('suspend', 'purge'))
		   AND NOT EXISTS (SELECT 1 FROM deprovision_jobs j WHERE j.identity_id = i.id
		                  AND j.updated_at > now() - $1::bigint * interval '1 microsecond')
		 ORDER BY (SELECT COALESCE(MAX(j.attempts), 0) FROM deprovision_jobs j WHERE j.identity_id = i.id AND j.state = 'pending'),
		          i.created_at, i.id LIMIT $2`, []any{idleFor.Microseconds(), limit}, func(r pgx.Row) (PendingLeaver, error) {
		var p PendingLeaver
		return p, r.Scan(&p.ID, &p.Purged)
	})
}

// DeleteUserSubjectRows deletes the user-subject capability grants and governance assignments of the
// purged identity id, matched case-insensitively, and says how many of each; the same user subjects are
// stripped from every runner pool use policy. A direct delete, never the
// GOV4 apply path. Rows keyed by one of principals go: the subject is a purged identity that can never
// authenticate again. Rows keyed by one of emails go only while no other principal holds that address,
// because an address can be recycled and the new holder's deny rows are theirs: another identity that is
// neither deactivated nor purged, or an API token or people row under a principal that is not in
// principals (or the address itself, a form of the leaver). Rows kept for that reason are counted in Kept.
func (s PG) DeleteUserSubjectRows(ctx context.Context, id uuid.UUID, principals, emails []string) (UserSubjectRowsDeleted, error) {
	var out UserSubjectRowsDeleted
	own, mails := lowerForms(principals), lowerForms(emails)
	if len(own)+len(mails) == 0 {
		return out, nil
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return out, fmt.Errorf("store: delete user subject rows: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	held, err := heldEmails(ctx, tx, id, own, mails)
	if err != nil {
		return out, err
	}
	subjects := slices.Concat(own, slices.DeleteFunc(slices.Clone(mails), func(m string) bool { return slices.Contains(held, m) }))
	tag, err := tx.Exec(ctx, `DELETE FROM capability_grants WHERE subject_type = 'user' AND lower(subject) = ANY($1::text[])`, subjects)
	if err != nil {
		return UserSubjectRowsDeleted{}, fmt.Errorf("store: delete user grants: %w", err)
	}
	out.Grants = tag.RowsAffected()
	tag, err = tx.Exec(ctx, `DELETE FROM governance_assignments WHERE subject_type = 'user' AND lower(subject) = ANY($1::text[])`, subjects)
	if err != nil {
		return UserSubjectRowsDeleted{}, fmt.Errorf("store: delete user assignments: %w", err)
	}
	out.Assignments = tag.RowsAffected()
	if out.PoolsNarrowed, out.PoolsEmptied, err = stripRunnerPoolUseSubjects(ctx, tx, subjects); err != nil {
		return UserSubjectRowsDeleted{}, err
	}
	if err = stripRemoteRunnerPeopleSubjects(ctx, tx, subjects); err != nil {
		return UserSubjectRowsDeleted{}, err
	}
	if len(held) > 0 {
		err = tx.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM capability_grants WHERE subject_type = 'user' AND lower(subject) = ANY($1::text[])) +
			(SELECT count(*) FROM governance_assignments WHERE subject_type = 'user' AND lower(subject) = ANY($1::text[]))`, held).Scan(&out.Kept)
		if err != nil {
			return UserSubjectRowsDeleted{}, fmt.Errorf("store: count kept user rows: %w", err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return UserSubjectRowsDeleted{}, fmt.Errorf("store: commit user subject delete: %w", err)
	}
	return out, nil
}

// UserSubjectRowsDeleted is what DeleteUserSubjectRows changed: the grants and assignments deleted, the
// email-keyed rows kept, and the runner pools whose use policy lost a subject, of which PoolsEmptied
// were left with none and so permit nobody until an administrator acts.
type UserSubjectRowsDeleted struct {
	Grants, Assignments, Kept   int64
	PoolsNarrowed, PoolsEmptied int64
}

// stripRunnerPoolUseSubjects removes the user subjects (lower-cased forms) from every pool use policy
// and raises each changed pool's revision, locking the pools in id order so two purges cannot deadlock.
// The policy row is never deleted: a policy narrows a pool, so
// deleting it would open the pool to everyone, while a list emptied by the purge permits nobody until
// an administrator replaces or clears it. It says how many policies changed, and how many of them it emptied.
func stripRunnerPoolUseSubjects(ctx context.Context, q Querier, subjects []string) (narrowed, emptied int64, err error) {
	const stmt = `
		WITH hit AS MATERIALIZED (
			SELECT p.id AS pool_id FROM runner_pools p WHERE EXISTS (
				SELECT 1 FROM runner_pool_use_policies u WHERE u.pool_id = p.id AND EXISTS (
					SELECT 1 FROM jsonb_array_elements(u.subjects) e
					WHERE e->>'subject_type' = 'user' AND lower(e->>'subject') = ANY($1::text[])))
			ORDER BY p.id FOR UPDATE OF p),
		bumped AS (
			UPDATE runner_pools p SET revision = p.revision + 1, updated_at = now()
			FROM hit WHERE p.id = hit.pool_id RETURNING p.id, p.revision),
		stripped AS (
			UPDATE runner_pool_use_policies u
			SET subjects = COALESCE((SELECT jsonb_agg(e) FROM jsonb_array_elements(u.subjects) e
			                         WHERE NOT (e->>'subject_type' = 'user' AND lower(e->>'subject') = ANY($1::text[]))), '[]'::jsonb),
			    revision = bumped.revision, updated_by = 'leaver purge', updated_at = now()
			FROM bumped WHERE u.pool_id = bumped.id
			RETURNING u.subjects = '[]'::jsonb AS emptied)
		SELECT count(*), count(*) FILTER (WHERE emptied) FROM stripped`
	if err := q.QueryRow(ctx, stmt, subjects).Scan(&narrowed, &emptied); err != nil {
		return 0, 0, fmt.Errorf("store: strip runner pool use policy subjects: %w", err)
	}
	return narrowed, emptied, nil
}

// stripRemoteRunnerPeopleSubjects removes the user subjects (lower-cased forms) from every remote-hosted
// runner's people policy and raises each changed runner's revision. A listed policy stays listed: an
// emptied list admits nobody, never everyone.
func stripRemoteRunnerPeopleSubjects(ctx context.Context, q Querier, subjects []string) error {
	const stmt = `
		UPDATE remote_runners rr
		SET people_subjects = COALESCE((SELECT jsonb_agg(e) FROM jsonb_array_elements(rr.people_subjects) e
		                                WHERE NOT (e->>'subject_type' = 'user' AND lower(e->>'subject') = ANY($1::text[]))), '[]'::jsonb),
		    revision = rr.revision + 1, updated_by = 'leaver purge', updated_at = now()
		WHERE rr.people_pools = 'listed' AND EXISTS (
			SELECT 1 FROM jsonb_array_elements(rr.people_subjects) e
			WHERE e->>'subject_type' = 'user' AND lower(e->>'subject') = ANY($1::text[]))`
	if _, err := q.Exec(ctx, stmt, subjects); err != nil {
		return fmt.Errorf("store: strip remote runner people subjects: %w", err)
	}
	return nil
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
// rows are not unfinished work) and every owed refusal row whatever the identity's state, oldest identity first, at most limit. One row per identity, kind and step: a step that fails for
// several targets is listed once, with the error of the one touched last.
func (s PG) ListDeprovisionFailures(ctx context.Context, limit int) ([]DeprovisionFailure, error) {
	return collect(ctx, s.Pool, "list", "deprovision failures", `
		SELECT j.identity_id, j.kind, j.step, (array_agg(j.last_error ORDER BY j.updated_at DESC))[1]
		  FROM deprovision_jobs j JOIN principal_identities i ON i.id = j.identity_id
		 WHERE j.state = 'pending' AND j.last_error <> '' AND j.kind IN ('suspend', 'purge')
		   AND (i.deactivated_at IS NOT NULL OR i.purged_at IS NOT NULL OR starts_with(j.step, $2))
		 GROUP BY j.identity_id, j.kind, j.step
		 ORDER BY min(j.created_at), j.identity_id, j.kind, j.step LIMIT $1`, []any{limit, JobStepRefusalAudit}, func(r pgx.Row) (DeprovisionFailure, error) {
		var f DeprovisionFailure
		return f, r.Scan(&f.IdentityID, &f.Kind, &f.Step, &f.LastError)
	})
}

// PendingRefusalAudits is every identity and kind with an owed refusal row that no attempt has touched for
// idleFor, at most limit. Unlike PendingLeavers and PendingGroupRemovals it does not ask whether the person is
// still a leaver or out of the group: a row owed before a reactivation or a re-add is owed all the same.
func (s PG) PendingRefusalAudits(ctx context.Context, idleFor time.Duration, limit int) ([]PendingRefusalAudit, error) {
	return collect(ctx, s.Pool, "list", "pending refusal audits", `
		SELECT DISTINCT identity_id, kind FROM deprovision_jobs
		 WHERE state = 'pending' AND starts_with(step, $1)
		   AND updated_at <= now() - $2::bigint * interval '1 microsecond'
		 ORDER BY 1, 2 LIMIT $3`, []any{JobStepRefusalAudit, idleFor.Microseconds(), limit}, func(r pgx.Row) (PendingRefusalAudit, error) {
		var p PendingRefusalAudit
		return p, r.Scan(&p.IdentityID, &p.Kind)
	})
}

// HeldEmails is the emails another principal holds, by DeleteUserSubjectRows' rule: another identity than
// id that is neither deactivated nor purged, or a live API token (unrevoked, unexpired) or people row
// (not deactivated) under a principal that is neither one of principals nor the address itself and whose
// identity is neither deactivated nor purged. A previous holder that is gone holds nothing. Matched
// case-insensitively; the answer is lower-cased.
func (s PG) HeldEmails(ctx context.Context, id uuid.UUID, principals, emails []string) ([]string, error) {
	return heldEmails(ctx, s.Pool, id, lowerForms(principals), lowerForms(emails))
}

func heldEmails(ctx context.Context, q Querier, id uuid.UUID, own, mails []string) ([]string, error) {
	return collect(ctx, q, "list", "held emails", `
		SELECT e FROM unnest($2::text[]) AS e
		 WHERE EXISTS (SELECT 1 FROM principal_identities i
		                WHERE i.id <> $1 AND i.deactivated_at IS NULL AND i.purged_at IS NULL
		                  AND (i.email_lower = e OR lower(i.scim_user_name) = e OR EXISTS (
		                        SELECT 1 FROM principal_identity_aliases a WHERE a.identity_id = i.id AND a.value_lower = e)))
		    OR EXISTS (SELECT 1 FROM api_tokens t WHERE lower(t.email) = e AND t.principal <> ''
		                  AND lower(t.principal) <> e AND lower(t.principal) <> ALL($3::text[])
		                  AND t.revoked_at IS NULL AND (t.expires_at IS NULL OR t.expires_at > now())
		                  AND NOT EXISTS (SELECT 1 FROM principal_identities d WHERE d.principal = t.principal
		                                   AND (d.deactivated_at IS NOT NULL OR d.purged_at IS NOT NULL)))
		    OR EXISTS (SELECT 1 FROM people p WHERE lower(p.email) = e AND lower(p.principal) <> e
		                  AND lower(p.principal) <> ALL($3::text[]) AND p.deactivated_at IS NULL
		                  AND NOT EXISTS (SELECT 1 FROM principal_identities d WHERE d.principal = p.principal
		                                   AND (d.deactivated_at IS NOT NULL OR d.purged_at IS NOT NULL)))`,
		[]any{id, mails, own}, func(r pgx.Row) (string, error) {
			var v string
			return v, r.Scan(&v)
		})
}

func lowerForms(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v = strings.ToLower(strings.TrimSpace(v)); v != "" {
			out = append(out, v)
		}
	}
	return out
}
