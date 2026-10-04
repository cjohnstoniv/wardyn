// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The attach lease (migration 0129): the one writer slot of a run's shared terminal, in Postgres
// so every replica agrees who holds it. The database's clock decides when a lease lapses.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AttachLease is one row of run_attach_leases. HolderID is the lease's identity: whatever
// writes into the terminal must still be the holder.
type AttachLease struct {
	RunID     uuid.UUID
	HolderID  uuid.UUID
	Replica   string // the replica serving the holder's socket
	Principal string
	Source    string // "web" or "ssh"
	Since     time.Time
	Epoch     int64 // moves at each takeover of the row
	ExpiresAt time.Time
	Live      bool // read-only: ExpiresAt was ahead of the database's clock
}

// AttachLeaseStore is the attach lease record. Optional like RunOutputStore: the api layer
// type-asserts, and a store without it keeps the writer slot in the process.
type AttachLeaseStore interface {
	// AcquireAttachLease takes runID's lease for l.HolderID when it is free, has lapsed, or is
	// held by from (uuid.Nil: nobody in particular), for ttl. It reports whether it did.
	AcquireAttachLease(ctx context.Context, l AttachLease, from uuid.UUID, ttl time.Duration) (bool, error)
	// RenewAttachLease extends holderID's lease by ttl, reporting whether holderID still held it.
	RenewAttachLease(ctx context.Context, runID, holderID uuid.UUID, ttl time.Duration) (bool, error)
	// HoldsAttachLease reports whether holderID holds runID's lease right now, unlapsed.
	HoldsAttachLease(ctx context.Context, runID, holderID uuid.UUID) (bool, error)
	// ReleaseAttachLease deletes the lease if holderID holds it, reporting whether it did.
	ReleaseAttachLease(ctx context.Context, runID, holderID uuid.UUID) (bool, error)
	// ReserveAttachLease is a take-over: it replaces the live lease held by from (uuid.Nil:
	// whoever holds it) with a reservation for taker, which only taker's own client may take, for
	// ttl, so no queued client of someone else gets the slot the take-over freed. It returns the
	// lease it replaced and the reservation's holder id; found is false when no live lease was
	// held by from.
	ReserveAttachLease(ctx context.Context, runID, from uuid.UUID, taker, replica string, ttl time.Duration) (prev AttachLease, reservation uuid.UUID, found bool, err error)
	// GetAttachLease returns runID's lease, lapsed or not (Live says which).
	GetAttachLease(ctx context.Context, runID uuid.UUID) (AttachLease, bool, error)
}

var _ AttachLeaseStore = PG{}

// AcquireAttachLease — see AttachLeaseStore.
func (s PG) AcquireAttachLease(ctx context.Context, l AttachLease, from uuid.UUID, ttl time.Duration) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `
		INSERT INTO run_attach_leases (run_id, holder_id, replica, principal, source, since, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, now() + $7::interval)
		ON CONFLICT (run_id) DO UPDATE SET
			holder_id = EXCLUDED.holder_id, replica = EXCLUDED.replica, principal = EXCLUDED.principal,
			source = EXCLUDED.source, since = EXCLUDED.since, expires_at = EXCLUDED.expires_at,
			epoch = run_attach_leases.epoch + 1
		WHERE run_attach_leases.expires_at <= now() OR run_attach_leases.holder_id = $8 OR run_attach_leases.holder_id = EXCLUDED.holder_id
		   OR (run_attach_leases.source = 'reserved' AND run_attach_leases.principal = EXCLUDED.principal)`,
		l.RunID, l.HolderID, l.Replica, l.Principal, l.Source, l.Since, ttl.String(), from)
	if err != nil {
		return false, fmt.Errorf("store: acquire the attach lease: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// RenewAttachLease — see AttachLeaseStore.
func (s PG) RenewAttachLease(ctx context.Context, runID, holderID uuid.UUID, ttl time.Duration) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE run_attach_leases SET expires_at = now() + $3::interval
		WHERE run_id = $1 AND holder_id = $2 AND expires_at > now()`, runID, holderID, ttl.String())
	if err != nil {
		return false, fmt.Errorf("store: renew the attach lease: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// HoldsAttachLease — see AttachLeaseStore.
func (s PG) HoldsAttachLease(ctx context.Context, runID, holderID uuid.UUID) (bool, error) {
	var ok bool
	if err := s.Pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM run_attach_leases WHERE run_id = $1 AND holder_id = $2 AND expires_at > now())`,
		runID, holderID).Scan(&ok); err != nil {
		return false, fmt.Errorf("store: check the attach lease: %w", err)
	}
	return ok, nil
}

// ReleaseAttachLease — see AttachLeaseStore.
func (s PG) ReleaseAttachLease(ctx context.Context, runID, holderID uuid.UUID) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM run_attach_leases WHERE run_id = $1 AND holder_id = $2`, runID, holderID)
	if err != nil {
		return false, fmt.Errorf("store: release the attach lease: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// attachLeaseReserved is the source of a reservation row (ReserveAttachLease).
const attachLeaseReserved = "reserved"

// ReserveAttachLease — see AttachLeaseStore.
func (s PG) ReserveAttachLease(ctx context.Context, runID, from uuid.UUID, taker, replica string, ttl time.Duration) (AttachLease, uuid.UUID, bool, error) {
	var prev AttachLease
	var reservation uuid.UUID
	err := s.Pool.QueryRow(ctx, `
		WITH old AS (
			SELECT run_id, holder_id, replica, principal, source, since, epoch, expires_at FROM run_attach_leases
			WHERE run_id = $1 AND expires_at > now() AND ($2::uuid = '00000000-0000-0000-0000-000000000000'::uuid OR holder_id = $2)
			FOR UPDATE)
		UPDATE run_attach_leases l SET holder_id = gen_random_uuid(), replica = $4, principal = $3, source = '`+attachLeaseReserved+`',
			since = now(), expires_at = now() + $5::interval, epoch = l.epoch + 1
		FROM old WHERE l.run_id = old.run_id
		RETURNING old.run_id, old.holder_id, old.replica, old.principal, old.source, old.since, old.epoch, old.expires_at, l.holder_id`,
		runID, from, taker, replica, ttl.String()).
		Scan(&prev.RunID, &prev.HolderID, &prev.Replica, &prev.Principal, &prev.Source, &prev.Since, &prev.Epoch, &prev.ExpiresAt, &reservation)
	if errors.Is(err, pgx.ErrNoRows) {
		return AttachLease{}, uuid.Nil, false, nil
	}
	if err != nil {
		return AttachLease{}, uuid.Nil, false, fmt.Errorf("store: reserve the attach lease: %w", err)
	}
	prev.Live = true
	return prev, reservation, true, nil
}

// GetAttachLease — see AttachLeaseStore.
func (s PG) GetAttachLease(ctx context.Context, runID uuid.UUID) (AttachLease, bool, error) {
	var l AttachLease
	err := s.Pool.QueryRow(ctx, `
		SELECT run_id, holder_id, replica, principal, source, since, epoch, expires_at, expires_at > now()
		FROM run_attach_leases WHERE run_id = $1`, runID).
		Scan(&l.RunID, &l.HolderID, &l.Replica, &l.Principal, &l.Source, &l.Since, &l.Epoch, &l.ExpiresAt, &l.Live)
	if errors.Is(err, pgx.ErrNoRows) {
		return AttachLease{}, false, nil
	}
	if err != nil {
		return AttachLease{}, false, fmt.Errorf("store: read the attach lease: %w", err)
	}
	return l, true, nil
}
