// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Member drives: a person's own drive, a user_drives row whose owner_subject
// names them. Ownership is the authority, so there are no grant rows.
// owner_subject is written once, on INSERT; no statement here updates it.
package store

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ErrMemberDriveLimit is returned by CreateMemberDrive when the owner already
// holds the most drives allowed. Rows being deleted still count.
var ErrMemberDriveLimit = errors.New("store: the most drives one person may hold already exist")

// ErrMemberDriveNameTaken is returned by CreateMemberDrive when the owner
// already has a drive whose name folds to the same slug.
var ErrMemberDriveNameTaken = fmt.Errorf("%w: you already have a drive with this name", ErrConflict)

// ErrMemberDriveBusy is returned by a delete Begin when another delete of the
// same drive started less than a minute ago.
var ErrMemberDriveBusy = fmt.Errorf("%w: this drive is already being deleted", ErrConflict)

// errMemberDriveNoOwner refuses an empty owner: an empty owner_subject is an
// admin row, so an empty owner here is a caller bug, never a person.
var errMemberDriveNoOwner = errors.New("store: a member drive needs an owner")

// memberDriveSlugIndex is the migration's per-owner unique index.
const memberDriveSlugIndex = "user_drives_member_slug_uniq"

// memberDriveTakeover is how old a delete tombstone must be before another
// delete may take it over (a crashed delete must not pin the drive forever).
const memberDriveTakeover = "60 seconds"

// memberDriveCounted runs between CreateMemberDrive's count and its insert.
// Tests set it to widen the race window the advisory lock closes.
var memberDriveCounted = func() {}

// CreateMemberDrive inserts d as owner d.OwnerSubject's drive unless the owner
// already holds limit rows. The count and the insert run in one READ COMMITTED
// transaction under the owner's MemberDriveLockClass lock. The row's managed
// fields are fixed here: no host root, the hash home, the id object scheme,
// reclaim delete, writable.
func (s PG) CreateMemberDrive(ctx context.Context, d types.UserDrive, limit int) (types.UserDrive, error) {
	if d.OwnerSubject == "" {
		return types.UserDrive{}, errMemberDriveNoOwner
	}
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return types.UserDrive{}, fmt.Errorf("store: begin member drive create: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // best-effort on the failure path

	// A crc32 collision only serializes two people's creates.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`,
		db.MemberDriveLockClass, int32(crc32.ChecksumIEEE([]byte(d.OwnerSubject)))); err != nil {
		return types.UserDrive{}, fmt.Errorf("store: lock member drives: %w", err)
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM user_drives WHERE owner_subject = $1`,
		d.OwnerSubject).Scan(&n); err != nil {
		return types.UserDrive{}, fmt.Errorf("store: count member drives: %w", err)
	}
	if n >= limit {
		return types.UserDrive{}, ErrMemberDriveLimit
	}
	memberDriveCounted()

	out, err := scanDriveRow(tx.QueryRow(ctx, `
		INSERT INTO user_drives (id, name, backend, host_root, storage_class, home_template, size_mib,
			writable, reclaim, created_by, name_slug, object_scheme, owner_subject)
		VALUES ($1, $2, $3, '', $4, 'hash', $5, true, 'delete', $6, $7, 'id', $8)
		RETURNING `+driveRowCols,
		d.ID, d.Name, d.Backend, d.StorageClass, d.SizeMiB, d.CreatedBy, types.DriveSlug(d.Name), d.OwnerSubject))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			if pgErr.ConstraintName == memberDriveSlugIndex {
				return types.UserDrive{}, ErrMemberDriveNameTaken
			}
			return types.UserDrive{}, ErrConflict
		}
		return types.UserDrive{}, fmt.Errorf("store: create member drive: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return types.UserDrive{}, fmt.Errorf("store: commit member drive create: %w", err)
	}
	return out, nil
}

// ListMemberDrivesOf returns owner's drives by name, tombstoned ones included.
func (s PG) ListMemberDrivesOf(ctx context.Context, owner string) ([]types.UserDrive, error) {
	if owner == "" {
		return nil, errMemberDriveNoOwner
	}
	return collect(ctx, s.Pool, "list", "member drives",
		`SELECT `+driveRowCols+` FROM user_drives WHERE owner_subject = $1 ORDER BY name, id`,
		[]any{owner}, scanDriveRow)
}

// GetMemberDriveOf returns drive id if owner owns it, else ErrNotFound.
func (s PG) GetMemberDriveOf(ctx context.Context, owner string, id uuid.UUID) (types.UserDrive, error) {
	if owner == "" {
		return types.UserDrive{}, errMemberDriveNoOwner
	}
	return scanDriveRow(s.Pool.QueryRow(ctx,
		`SELECT `+driveRowCols+` FROM user_drives WHERE id = $1 AND owner_subject = $2`, id, owner))
}

// beginMemberDriveDeleteSQL sets the tombstone on a member drive (an empty $2 means
// any owner) unless a delete started under a minute ago. RETURNING the row
// carries the tombstone the caller later finishes or aborts with.
const beginMemberDriveDeleteSQL = `UPDATE user_drives SET delete_started_at = now()
	WHERE id = $1 AND owner_subject <> '' AND ($2::text = '' OR owner_subject = $2)
	  AND (delete_started_at IS NULL OR delete_started_at < now() - interval '` + memberDriveTakeover + `')
	RETURNING ` + driveRowCols

// BeginMemberDriveDelete tombstones owner's drive id. ErrNotFound when owner
// does not own it; ErrMemberDriveBusy when another delete holds it.
func (s PG) BeginMemberDriveDelete(ctx context.Context, owner string, id uuid.UUID) (types.UserDrive, time.Time, error) {
	if owner == "" {
		return types.UserDrive{}, time.Time{}, errMemberDriveNoOwner
	}
	return s.beginMemberDriveDelete(ctx, owner, id)
}

// BeginMemberDriveDeleteAdmin is BeginMemberDriveDelete for any owner.
func (s PG) BeginMemberDriveDeleteAdmin(ctx context.Context, id uuid.UUID) (types.UserDrive, time.Time, error) {
	return s.beginMemberDriveDelete(ctx, "", id)
}

func (s PG) beginMemberDriveDelete(ctx context.Context, owner string, id uuid.UUID) (types.UserDrive, time.Time, error) {
	d, err := scanDriveRow(s.Pool.QueryRow(ctx, beginMemberDriveDeleteSQL, id, owner))
	if err == nil {
		return d, *d.DeleteStartedAt, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return types.UserDrive{}, time.Time{}, err
	}
	// No row: told apart only on the failing path. The owner predicate is the
	// same as the update's, so a drive the caller cannot see stays ErrNotFound.
	var owned bool
	if err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM user_drives
		WHERE id = $1 AND owner_subject <> '' AND ($2::text = '' OR owner_subject = $2))`, id, owner).Scan(&owned); err != nil {
		return types.UserDrive{}, time.Time{}, fmt.Errorf("store: read member drive: %w", err)
	}
	if owned {
		return types.UserDrive{}, time.Time{}, ErrMemberDriveBusy
	}
	return types.UserDrive{}, time.Time{}, ErrNotFound
}

// AbortMemberDriveDelete clears the tombstone started set, and only that one.
// ErrNotFound when it is gone (finished, or taken over).
func (s PG) AbortMemberDriveDelete(ctx context.Context, id uuid.UUID, started time.Time) error {
	return memberDriveTombstoneExec(ctx, s, "abort",
		`UPDATE user_drives SET delete_started_at = NULL WHERE id = $1 AND delete_started_at = $2`, id, started)
}

// FinishMemberDriveDelete removes the row while it still carries the tombstone
// started set. ErrNotFound when it does not.
func (s PG) FinishMemberDriveDelete(ctx context.Context, id uuid.UUID, started time.Time) error {
	return memberDriveTombstoneExec(ctx, s, "finish",
		`DELETE FROM user_drives WHERE id = $1 AND delete_started_at = $2`, id, started)
}

func memberDriveTombstoneExec(ctx context.Context, s PG, verb, q string, id uuid.UUID, started time.Time) error {
	tag, err := s.Pool.Exec(ctx, q, id, started)
	if err != nil {
		return fmt.Errorf("store: %s member drive delete: %w", verb, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListMemberDrives returns one page of every person's drives, by owner then
// name, and the total count.
func (s PG) ListMemberDrives(ctx context.Context, p Page) ([]types.UserDrive, int, error) {
	var total int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM user_drives WHERE owner_subject <> ''`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: count member drives: %w", err)
	}
	q, args := p.appendTo(`SELECT `+driveRowCols+` FROM user_drives WHERE owner_subject <> ''
		ORDER BY owner_subject, name, id`, nil)
	out, err := collect(ctx, s.Pool, "list", "member drives", q, args, scanDriveRow)
	return out, total, err
}
