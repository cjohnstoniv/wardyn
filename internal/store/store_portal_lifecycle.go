// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The portal's own suspension, purge and reactivation writes and selectors. The SCIM ones stay as they are:
// they compare the row's issuer to the SCIM issuer, which delegate:person:<k> never equals. The selectors
// here put starts_with(i.issuer, 'delegate:person:') in the SQL before LIMIT, so a workforce row never fills
// their batch.
//
// The principal_identities trigger refuses a purge deadline, a reactivation and a tombstone for a portal row
// unless the transaction set the marker below; these writes, and SuspendIdentity with PortalLifecycle, are
// the only ones that do.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// PortalLifecycleStore is the portal lifecycle's store surface, optional like LeaverStore.
type PortalLifecycleStore interface {
	MarkPortalIdentityPurged(ctx context.Context, id uuid.UUID, mode PortalPurgeMode) (bool, error)
	ReactivatePortalIdentity(ctx context.Context, id uuid.UUID) (bool, error)
	PurgeDuePortalIdentities(ctx context.Context, now time.Time, limit int) ([]DueIdentity, error)
	PendingPortalLeavers(ctx context.Context, limit int) ([]PendingLeaver, error)
	PendingPortalRefusalAudits(ctx context.Context, limit int) ([]PendingRefusalAudit, error)
	ListDeactivatedPortalIdentities(ctx context.Context, limit int) ([]PrincipalIdentity, error)
	ListPortalDeprovisionFailures(ctx context.Context, limit int) ([]DeprovisionFailure, error)
}

var _ PortalLifecycleStore = PG{}

// PortalPurgeMode says who is purging a portal person and so what the row must be.
type PortalPurgeMode int

const (
	// PortalPurgeScheduled is the sweep's: it marks only a row that is due under its own suspension.
	PortalPurgeScheduled PortalPurgeMode = iota + 1
	// PortalPurgeExplicit is the admin route's: it marks a row that is deactivated, due or not.
	PortalPurgeExplicit
)

// markPortalLifecycle sets the transaction-scoped marker the principal_identities trigger reads, so the
// transaction may set a portal person's purge deadline, reactivate them or tombstone them. q must be a
// pgx.Tx: outside one the setting would not outlive its statement.
func markPortalLifecycle(ctx context.Context, q Querier) error {
	if _, err := q.Exec(ctx, `SELECT set_config('wardyn.portal_lifecycle', 'v1', true)`); err != nil {
		return fmt.Errorf("store: mark portal lifecycle: %w", err)
	}
	return nil
}

// MarkPortalIdentityPurged turns a portal person into a permanent tombstone (purged_at set, display_email
// blanked, in one statement under the lifecycle marker) and records the purge's last step as pending, in one
// transaction that holds the row FOR UPDATE. It reports whether this call marked it.
//
// The conditions are decided under the lock. Nothing is written, and false is returned, for a row that is
// not a portal person's, one already purged (a retry of an unfinished purge), and, in PortalPurgeScheduled
// mode, one that is not due under its own suspension: a person reactivated after the sweep selected them, or
// suspended again with a later deadline. In PortalPurgeExplicit mode a row that is not deactivated is
// ErrPersonNotSuspended, so a reactivation that committed first wins. An unknown id is ErrNotFound.
func (s PG) MarkPortalIdentityPurged(ctx context.Context, id uuid.UUID, mode PortalPurgeMode) (bool, error) {
	if mode != PortalPurgeScheduled && mode != PortalPurgeExplicit {
		return false, fmt.Errorf("store: mark portal identity purged: unknown mode %d", mode)
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, fmt.Errorf("store: mark portal identity purged: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = markPortalLifecycle(ctx, tx); err != nil {
		return false, err
	}
	var owned, purged, deactivated, due bool
	err = tx.QueryRow(ctx, `
		SELECT starts_with(i.issuer, $2), i.purged_at IS NOT NULL, i.deactivated_at IS NOT NULL,
		       i.deactivated_at IS NOT NULL AND i.purge_after IS NOT NULL AND i.purge_after <= now() AND `+ownSuspension+`
		  FROM principal_identities i WHERE i.id = $1 FOR UPDATE`, id, types.PortalPersonPrefix).Scan(&owned, &purged, &deactivated, &due)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("store: lock portal identity: %w", err)
	}
	switch {
	case !owned || purged:
		return false, nil
	case mode == PortalPurgeScheduled && !due:
		return false, nil
	case mode == PortalPurgeExplicit && !deactivated:
		return false, ErrPersonNotSuspended
	}
	if _, err = tx.Exec(ctx, `UPDATE principal_identities SET purged_at = clock_timestamp(), display_email = '' WHERE id = $1`, id); err != nil {
		return false, fmt.Errorf("store: mark portal identity purged: %w", err)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM portal_person_portals WHERE identity_id = $1`, id); err != nil {
		return false, fmt.Errorf("store: delete portal use: %w", err)
	}
	if _, err = tx.Exec(ctx, `
		INSERT INTO deprovision_jobs (identity_id, kind, step, target) VALUES ($1, $2, $3, '')
		ON CONFLICT (identity_id, kind, step, target) DO NOTHING`, id, JobKindPurge, JobStepAuditDeprovision); err != nil {
		return false, fmt.Errorf("store: record purge step: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: commit portal purge mark: %w", err)
	}
	return true, nil
}

// ReactivatePortalIdentity clears a portal person's deactivation and purge deadline, and their people row's
// deactivation, in one marked transaction. A row that is purged, or not a portal person's, is left alone: it
// reports false. The mirror of the SCIM reactivation, with the origin in the predicate.
func (s PG) ReactivatePortalIdentity(ctx context.Context, id uuid.UUID) (bool, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, fmt.Errorf("store: reactivate portal identity: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = markPortalLifecycle(ctx, tx); err != nil {
		return false, err
	}
	var principal *string
	err = tx.QueryRow(ctx, `
		UPDATE principal_identities SET deactivated_at = NULL, purge_after = NULL
		 WHERE id = $1 AND purged_at IS NULL AND starts_with(issuer, $2) RETURNING principal`, id, types.PortalPersonPrefix).Scan(&principal)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: reactivate portal identity: %w", err)
	}
	if principal != nil {
		if _, err = tx.Exec(ctx, `UPDATE people SET deactivated_at = NULL WHERE principal = $1`, *principal); err != nil {
			return false, fmt.Errorf("store: reactivate portal person: %w", err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: commit portal reactivation: %w", err)
	}
	return true, nil
}

// PurgeDuePortalIdentities is the portal people deactivated and not purged whose purge_after has passed at
// now, those deactivated by their own suspension first, then oldest first, at most limit. A workforce row is
// never listed, and that is decided in the SQL before the limit. It is only a candidate read:
// MarkPortalIdentityPurged re-checks under the lock.
func (s PG) PurgeDuePortalIdentities(ctx context.Context, now time.Time, limit int) ([]DueIdentity, error) {
	return collect(ctx, s.Pool, "list", "portal identities due for purge", `
		SELECT i.id, `+ownSuspension+` AS suspended
		  FROM principal_identities i
		 WHERE i.purged_at IS NULL AND i.deactivated_at IS NOT NULL AND i.purge_after IS NOT NULL AND i.purge_after <= $2
		   AND starts_with(i.issuer, $3)
		 ORDER BY suspended DESC, i.purge_after, i.id LIMIT $1`, []any{limit, now, types.PortalPersonPrefix}, func(r pgx.Row) (DueIdentity, error) {
		var d DueIdentity
		return d, r.Scan(&d.ID, &d.Suspended)
	})
}

// PendingPortalLeavers is the portal people with a suspend or purge ledger row still pending, the fewest
// failed attempts first, at most limit: PendingLeavers for rows whose issuer starts delegate:person:. A
// person neither deactivated nor purged has a stale ledger and is not listed.
func (s PG) PendingPortalLeavers(ctx context.Context, limit int) ([]PendingLeaver, error) {
	return collect(ctx, s.Pool, "list", "pending portal leavers", `
		SELECT i.id, i.purged_at IS NOT NULL, i.deactivated_at
		  FROM principal_identities i
		 WHERE (i.purged_at IS NOT NULL OR i.deactivated_at IS NOT NULL)
		   AND starts_with(i.issuer, $2)
		   AND EXISTS (SELECT 1 FROM deprovision_jobs j WHERE j.identity_id = i.id AND j.state = 'pending'
		                  AND j.kind IN ('suspend', 'purge'))
		 ORDER BY (SELECT COALESCE(MAX(j.attempts), 0) FROM deprovision_jobs j WHERE j.identity_id = i.id AND j.state = 'pending'),
		          i.created_at, i.id LIMIT $1`, []any{limit, types.PortalPersonPrefix}, func(r pgx.Row) (PendingLeaver, error) {
		var p PendingLeaver
		return p, r.Scan(&p.ID, &p.Purged, &p.DeactivatedAt)
	})
}

// PendingPortalRefusalAudits is every portal person and ledger kind with an owed refusal row, at most limit:
// PendingRefusalAudits for rows whose issuer starts delegate:person:, scoped before the limit.
func (s PG) PendingPortalRefusalAudits(ctx context.Context, limit int) ([]PendingRefusalAudit, error) {
	return collect(ctx, s.Pool, "list", "pending portal refusal audits", `
		SELECT DISTINCT j.identity_id, j.kind FROM deprovision_jobs j
		  JOIN principal_identities i ON i.id = j.identity_id
		 WHERE j.state = 'pending' AND starts_with(j.step, $1) AND starts_with(i.issuer, $3)
		 ORDER BY 1, 2 LIMIT $2`, []any{JobStepRefusalAudit, limit, types.PortalPersonPrefix}, func(r pgx.Row) (PendingRefusalAudit, error) {
		var p PendingRefusalAudit
		return p, r.Scan(&p.IdentityID, &p.Kind)
	})
}

// ListDeactivatedPortalIdentities is the portal people deactivated and not purged, most recently deactivated
// first, at most limit: the portal view's half of ListDeactivatedIdentities.
func (s PG) ListDeactivatedPortalIdentities(ctx context.Context, limit int) ([]PrincipalIdentity, error) {
	return collect(ctx, s.Pool, "list", "deactivated portal identities", `SELECT `+principalIdentityCols+`
		FROM principal_identities i WHERE i.deactivated_at IS NOT NULL AND i.purged_at IS NULL AND starts_with(i.issuer, $2)
		ORDER BY i.deactivated_at DESC, i.id LIMIT $1`, []any{limit, types.PortalPersonPrefix}, scanPrincipalIdentity)
}

// ListPortalDeprovisionFailures is ListDeprovisionFailures for portal people: the pending suspend and purge
// ledger rows with a recorded error, and every owed refusal row, scoped before the limit.
func (s PG) ListPortalDeprovisionFailures(ctx context.Context, limit int) ([]DeprovisionFailure, error) {
	return collect(ctx, s.Pool, "list", "portal deprovision failures", `
		SELECT j.identity_id, j.kind, j.step, (array_agg(j.last_error ORDER BY j.updated_at DESC))[1]
		  FROM deprovision_jobs j JOIN principal_identities i ON i.id = j.identity_id
		 WHERE j.state = 'pending' AND j.last_error <> '' AND j.kind IN ('suspend', 'purge')
		   AND (i.deactivated_at IS NOT NULL OR i.purged_at IS NOT NULL OR starts_with(j.step, $2))
		   AND starts_with(i.issuer, $3)
		 GROUP BY j.identity_id, j.kind, j.step
		 ORDER BY min(j.created_at), j.identity_id, j.kind, j.step LIMIT $1`, []any{limit, JobStepRefusalAudit, types.PortalPersonPrefix}, func(r pgx.Row) (DeprovisionFailure, error) {
		var f DeprovisionFailure
		return f, r.Scan(&f.IdentityID, &f.Kind, &f.Step, &f.LastError)
	})
}
