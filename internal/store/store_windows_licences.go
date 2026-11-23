// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// windowsInsertErr maps a take or fence refusal, named by its constraint, to the fence's error. runInsertErr asks
// it for every 23514 whose constraint starts windows_, and windowsFenceErr for SetRunImage's.
func windowsInsertErr(constraint string) error {
	var err error
	switch constraint {
	case "windows_vms_off":
		err = ErrWindowsVMsOff
	case "windows_image_unregistered":
		err = ErrWindowsImageUnregistered
	case "windows_licence_pool_inactive":
		err = ErrWindowsLicencePoolInactive
	case "windows_licence_not_holder":
		err = ErrWindowsLicenceNotHolder
	case "windows_licence_in_use":
		err = ErrWindowsLicenceInUse
	default:
		err = ErrWindowsVMFenced
	}
	return fmt.Errorf("%w (%s)", err, constraint)
}

// windowsFenceErr maps an update the fence refused, a 23514 whose constraint starts windows_, through
// windowsInsertErr; any other error passes through unchanged.
func windowsFenceErr(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23514" && strings.HasPrefix(pe.ConstraintName, "windows_") {
		return windowsInsertErr(pe.ConstraintName)
	}
	return err
}

const windowsPoolCols = `id, name, seats, state, revision, created_by, created_at, updated_at`

func scanWindowsPool(row pgx.Row) (types.WindowsLicencePool, error) {
	var p types.WindowsLicencePool
	err := row.Scan(&p.ID, &p.Name, &p.Seats, &p.State, &p.Revision, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt)
	return p, windowsNotFound(err, "Windows licence pool")
}

const windowsHolderCols = `id, pool_id, subject, added_by, added_at`

func scanWindowsHolder(row pgx.Row) (types.WindowsLicenceHolder, error) {
	var h types.WindowsLicenceHolder
	err := row.Scan(&h.ID, &h.PoolID, &h.Subject, &h.AddedBy, &h.AddedAt)
	return h, windowsNotFound(err, "Windows licence holder")
}

const windowsImageCols = `id, name, ref, licence_pool_id, state, revision, created_by, created_at, updated_at`

func scanWindowsImage(row pgx.Row) (types.WindowsImage, error) {
	var i types.WindowsImage
	err := row.Scan(&i.ID, &i.Name, &i.Ref, &i.LicencePoolID, &i.State, &i.Revision, &i.CreatedBy, &i.CreatedAt, &i.UpdatedAt)
	return i, windowsNotFound(err, "Windows image")
}

// windowsNotFound is ErrNotFound for no row, a wrapped scan error otherwise.
func windowsNotFound(err error, what string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNotFound
	}
	return fmt.Errorf("store: read %s: %w", what, err)
}

// windowsUniqueConflict is ErrConflict for a unique violation (a live name, ref or holder taken twice).
func windowsUniqueConflict(err error, what string) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return fmt.Errorf("%w: %s", ErrConflict, what)
	}
	return err
}

func (s PG) ListWindowsLicencePools(ctx context.Context) ([]types.WindowsLicencePool, error) {
	return collect(ctx, s.Pool, "list", "Windows licence pools",
		`SELECT `+windowsPoolCols+` FROM windows_licence_pools ORDER BY lower(name), id`, nil, scanWindowsPool)
}

func (s PG) GetWindowsLicencePool(ctx context.Context, id uuid.UUID) (types.WindowsLicencePool, error) {
	return scanWindowsPool(s.Pool.QueryRow(ctx, `SELECT `+windowsPoolCols+` FROM windows_licence_pools WHERE id = $1`, id))
}

func (s PG) CreateWindowsLicencePool(ctx context.Context, p types.WindowsLicencePool) (types.WindowsLicencePool, error) {
	if err := p.Validate(); err != nil {
		return types.WindowsLicencePool{}, err
	}
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	out, err := scanWindowsPool(s.Pool.QueryRow(ctx, `INSERT INTO windows_licence_pools (id, name, seats, created_by)
		VALUES ($1, $2, $3, $4) RETURNING `+windowsPoolCols, p.ID, p.Name, p.Seats, p.CreatedBy))
	return out, windowsUniqueConflict(err, "a live Windows licence pool has that name")
}

// lockWindowsPool locks an active pool's row for an admin write and checks its revision (0 skips the check).
func lockWindowsPool(ctx context.Context, tx pgx.Tx, id uuid.UUID, revision int64) (types.WindowsLicencePool, error) {
	p, err := scanWindowsPool(tx.QueryRow(ctx, `SELECT `+windowsPoolCols+` FROM windows_licence_pools
		WHERE id = $1 AND state = 'active' FOR UPDATE`, id))
	if err == nil && revision != 0 && p.Revision != revision {
		err = ErrWindowsLicenceStale
	}
	return p, err
}

// liveSeat is the run of one live lease the where clause matches, or uuid.Nil when none does.
func liveSeat(ctx context.Context, q Querier, where string, arg any) (uuid.UUID, error) {
	var run uuid.UUID
	err := q.QueryRow(ctx, `SELECT run_id FROM windows_licence_leases WHERE `+where+` ORDER BY taken_at, run_id LIMIT 1`, arg).Scan(&run)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, nil
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("store: read live Windows seats: %w", err)
	}
	return run, nil
}

// refuseLiveSeat is WindowsSeatLiveError naming the live seat the where clause matches, or nil when none does.
func refuseLiveSeat(ctx context.Context, q Querier, where string, arg any) error {
	run, err := liveSeat(ctx, q, where, arg)
	if err != nil || run == uuid.Nil {
		return err
	}
	return &WindowsSeatLiveError{RunID: run}
}

func (s PG) UpdateWindowsLicencePool(ctx context.Context, id uuid.UUID, revision int64, name *string, seats *int) (types.WindowsLicencePool, error) {
	var out types.WindowsLicencePool
	err := pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		p, err := lockWindowsPool(ctx, tx, id, revision)
		if err != nil {
			return err
		}
		if name != nil {
			p.Name = *name
		}
		if seats != nil {
			p.Seats = *seats
		}
		if err = p.Validate(); err != nil {
			return err
		}
		var held int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM windows_licence_holders WHERE pool_id = $1`, id).Scan(&held); err != nil {
			return fmt.Errorf("store: count Windows licence holders: %w", err)
		}
		if p.Seats < held {
			return fmt.Errorf("%w: %d holders", ErrWindowsLicenceSeatsBelowHolders, held)
		}
		out, err = scanWindowsPool(tx.QueryRow(ctx, `UPDATE windows_licence_pools
			SET name = $2, seats = $3, revision = revision + 1, updated_at = now() WHERE id = $1 RETURNING `+windowsPoolCols,
			id, p.Name, p.Seats))
		return windowsUniqueConflict(err, "a live Windows licence pool has that name")
	})
	return out, err
}

func (s PG) DeleteWindowsLicencePool(ctx context.Context, id uuid.UUID) (types.WindowsLicencePool, error) {
	var out types.WindowsLicencePool
	err := pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		if _, err := lockWindowsPool(ctx, tx, id, 0); err != nil {
			return err
		}
		if err := refuseLiveSeat(ctx, tx, `pool_id = $1`, id); err != nil {
			return err
		}
		var err error
		out, err = scanWindowsPool(tx.QueryRow(ctx, `UPDATE windows_licence_pools
			SET state = 'deleted', revision = revision + 1, updated_at = now() WHERE id = $1 RETURNING `+windowsPoolCols, id))
		return err
	})
	return out, err
}

func (s PG) ListWindowsLicenceHolders(ctx context.Context, poolID uuid.UUID) ([]types.WindowsLicenceHolder, error) {
	return collect(ctx, s.Pool, "list", "Windows licence holders",
		`SELECT `+windowsHolderCols+` FROM windows_licence_holders WHERE pool_id = $1 ORDER BY subject`, []any{poolID}, scanWindowsHolder)
}

func (s PG) AddWindowsLicenceHolder(ctx context.Context, poolID uuid.UUID, subject, by string) (types.WindowsLicenceHolder, error) {
	var out types.WindowsLicenceHolder
	if types.NormalizeWindowsLicenceSubject(subject) == "" {
		return out, errors.New("name the person who holds the seat")
	}
	err := pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		p, err := lockWindowsPool(ctx, tx, poolID, 0)
		if err != nil {
			return err
		}
		var held int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM windows_licence_holders WHERE pool_id = $1`, poolID).Scan(&held); err != nil {
			return fmt.Errorf("store: count Windows licence holders: %w", err)
		}
		if held >= p.Seats {
			return fmt.Errorf("%w: %d of %d", ErrWindowsLicencePoolFull, held, p.Seats)
		}
		out, err = scanWindowsHolder(tx.QueryRow(ctx, `INSERT INTO windows_licence_holders (id, pool_id, subject, added_by)
			VALUES ($1, $2, pg_catalog.lower(pg_catalog.btrim($3)), $4) RETURNING `+windowsHolderCols, uuid.New(), poolID, subject, by))
		return windowsUniqueConflict(err, "the person already holds a seat in this pool")
	})
	return out, err
}

// RemoveWindowsLicenceHolder locks only the holder row: a take holds its image, pool and holder rows FOR SHARE in
// that order, so a writer that also waited for the pool row could deadlock with it.
func (s PG) RemoveWindowsLicenceHolder(ctx context.Context, poolID uuid.UUID, subject string) (types.WindowsLicenceHolder, error) {
	var out types.WindowsLicenceHolder
	err := pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		h, err := scanWindowsHolder(tx.QueryRow(ctx, `SELECT `+windowsHolderCols+` FROM windows_licence_holders
			WHERE pool_id = $1 AND lower(subject) = pg_catalog.lower(pg_catalog.btrim($2)) FOR UPDATE`, poolID, subject))
		if err != nil {
			return err
		}
		if err = refuseLiveSeat(ctx, tx, `holder_id = $1`, h.ID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `DELETE FROM windows_licence_holders WHERE id = $1`, h.ID); err != nil {
			return fmt.Errorf("store: remove Windows licence holder: %w", err)
		}
		out = h
		return nil
	})
	return out, err
}

func (s PG) ListWindowsLicenceLeases(ctx context.Context, poolID uuid.UUID) ([]types.WindowsLicenceLease, error) {
	return collect(ctx, s.Pool, "list", "Windows licence seats",
		`SELECT run_id, pool_id, holder_id, image_id, taken_at FROM windows_licence_leases WHERE pool_id = $1 ORDER BY taken_at, run_id`,
		[]any{poolID}, func(row pgx.Row) (types.WindowsLicenceLease, error) {
			var l types.WindowsLicenceLease
			if err := row.Scan(&l.RunID, &l.PoolID, &l.HolderID, &l.ImageID, &l.TakenAt); err != nil {
				return l, fmt.Errorf("store: scan Windows licence seat: %w", err)
			}
			return l, nil
		})
}

func (s PG) ListWindowsImages(ctx context.Context) ([]types.WindowsImage, error) {
	return collect(ctx, s.Pool, "list", "Windows images",
		`SELECT `+windowsImageCols+` FROM windows_images ORDER BY lower(name), id`, nil, scanWindowsImage)
}

func (s PG) GetWindowsImage(ctx context.Context, id uuid.UUID) (types.WindowsImage, error) {
	return scanWindowsImage(s.Pool.QueryRow(ctx, `SELECT `+windowsImageCols+` FROM windows_images WHERE id = $1`, id))
}

func (s PG) RegisterWindowsImage(ctx context.Context, img types.WindowsImage) (types.WindowsImage, error) {
	if err := img.Validate(); err != nil {
		return types.WindowsImage{}, err
	}
	if img.ID == uuid.Nil {
		img.ID = uuid.New()
	}
	var out types.WindowsImage
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var one int
		if err := tx.QueryRow(ctx, `SELECT 1 FROM windows_licence_pools WHERE id = $1 AND state = 'active' FOR SHARE`,
			img.LicencePoolID).Scan(&one); err != nil {
			return windowsNotFound(err, "Windows licence pool")
		}
		var err error
		out, err = scanWindowsImage(tx.QueryRow(ctx, `INSERT INTO windows_images (id, name, ref, licence_pool_id, created_by)
			VALUES ($1, $2, $3, $4, $5) RETURNING `+windowsImageCols, img.ID, img.Name, img.Ref, img.LicencePoolID, img.CreatedBy))
		return windowsUniqueConflict(err, "a live Windows image has that name or ref")
	})
	return out, err
}

// lockWindowsImage locks an active image's row for an admin write and checks its revision (0 skips the check).
func lockWindowsImage(ctx context.Context, tx pgx.Tx, id uuid.UUID, revision int64) (types.WindowsImage, error) {
	img, err := scanWindowsImage(tx.QueryRow(ctx, `SELECT `+windowsImageCols+` FROM windows_images
		WHERE id = $1 AND state = 'active' FOR UPDATE`, id))
	if err == nil && revision != 0 && img.Revision != revision {
		err = ErrWindowsLicenceStale
	}
	return img, err
}

func (s PG) UpdateWindowsImage(ctx context.Context, id uuid.UUID, revision int64, name *string, poolID *uuid.UUID) (types.WindowsImage, error) {
	var out types.WindowsImage
	err := pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		img, err := lockWindowsImage(ctx, tx, id, revision)
		if err != nil {
			return err
		}
		if name != nil {
			img.Name = *name
		}
		if poolID != nil && *poolID != img.LicencePoolID {
			if err = refuseLiveSeat(ctx, tx, `image_id = $1`, id); err != nil {
				return err
			}
			var one int
			if err = tx.QueryRow(ctx, `SELECT 1 FROM windows_licence_pools WHERE id = $1 AND state = 'active' FOR SHARE`,
				*poolID).Scan(&one); err != nil {
				return windowsNotFound(err, "Windows licence pool")
			}
			img.LicencePoolID = *poolID
		}
		if err = img.Validate(); err != nil {
			return err
		}
		out, err = scanWindowsImage(tx.QueryRow(ctx, `UPDATE windows_images SET name = $2, licence_pool_id = $3,
			revision = revision + 1, updated_at = now() WHERE id = $1 RETURNING `+windowsImageCols, id, img.Name, img.LicencePoolID))
		return windowsUniqueConflict(err, "a live Windows image has that name")
	})
	return out, err
}

func (s PG) RetireWindowsImage(ctx context.Context, id uuid.UUID) (types.WindowsImage, error) {
	var out types.WindowsImage
	err := pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		if _, err := lockWindowsImage(ctx, tx, id, 0); err != nil {
			return err
		}
		if err := refuseLiveSeat(ctx, tx, `image_id = $1`, id); err != nil {
			return err
		}
		var err error
		out, err = scanWindowsImage(tx.QueryRow(ctx, `UPDATE windows_images
			SET state = 'retired', revision = revision + 1, updated_at = now() WHERE id = $1 RETURNING `+windowsImageCols, id))
		return err
	})
	return out, err
}

func (s PG) WindowsLaunchCheck(ctx context.Context, ref, subject string) (WindowsLaunch, error) {
	var l WindowsLaunch
	var err error
	if l.Image, err = scanWindowsImage(s.Pool.QueryRow(ctx, `SELECT `+windowsImageCols+` FROM windows_images
		WHERE ref = $1 AND state = 'active'`, ref)); err != nil {
		if errors.Is(err, ErrNotFound) {
			return l, ErrWindowsImageUnregistered
		}
		return l, err
	}
	if l.Pool, err = s.GetWindowsLicencePool(ctx, l.Image.LicencePoolID); err != nil || l.Pool.State != types.WindowsLicencePoolActive {
		if err == nil || errors.Is(err, ErrNotFound) {
			err = ErrWindowsLicencePoolInactive
		}
		return l, err
	}
	if l.Holder, err = scanWindowsHolder(s.Pool.QueryRow(ctx, `SELECT `+windowsHolderCols+` FROM windows_licence_holders
		WHERE pool_id = $1 AND lower(subject) = pg_catalog.lower(pg_catalog.btrim($2))`, l.Pool.ID, subject)); err != nil {
		if errors.Is(err, ErrNotFound) {
			err = ErrWindowsLicenceNotHolder
		}
		return l, err
	}
	run, err := liveSeat(ctx, s.Pool, `holder_id = $1`, l.Holder.ID)
	if err == nil && run != uuid.Nil {
		err = &WindowsLicenceInUseError{RunID: run}
	}
	return l, err
}

func (s PG) DeleteWindowsLicenceHoldersOf(ctx context.Context, subjects []string) (int64, error) {
	var n int64
	err := pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		var err error
		n, err = deleteWindowsLicenceHolders(ctx, tx, lowerForms(subjects))
		return err
	})
	return n, err
}

// deleteWindowsLicenceHolders deletes every seat the lower-cased subjects hold in any pool. A seat a run still
// holds refuses it, naming the run, before anything is deleted: the run's end frees the seat first (a leaver's
// suspension kills their runs), and the caller retries. The holder rows are locked first, so a take that is
// in flight commits its lease before the check, or waits and then finds no holder.
func deleteWindowsLicenceHolders(ctx context.Context, tx pgx.Tx, subjects []string) (int64, error) {
	if len(subjects) == 0 {
		return 0, nil
	}
	rows, err := tx.Query(ctx, `SELECT id FROM windows_licence_holders WHERE lower(subject) = ANY($1::text[]) FOR UPDATE`, subjects)
	if err != nil {
		return 0, fmt.Errorf("store: lock Windows licence holders: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	if err = refuseLiveSeat(ctx, tx, `holder_id = ANY($1::uuid[])`, ids); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM windows_licence_holders WHERE id = ANY($1::uuid[])`, ids)
	if err != nil {
		return 0, fmt.Errorf("store: delete Windows licence holders: %w", err)
	}
	return tag.RowsAffected(), nil
}
