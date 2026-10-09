// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// MigrateLocal is the -migrate-secrets target that seals rows locally.
const MigrateLocal = "local"

// MigrateResult is what Migrate did.
type MigrateResult struct {
	// Moved is how many rows moved.
	Moved int
	// SoftDeleted is how many old external copies the store deleted but did
	// not purge (azurekv, purge withheld or failed): the organisation can
	// still recover them, and -reconcile lists them.
	SoftDeleted int
}

// Migrate moves every row not already at target ("local", or the configured
// external store's name), one row per transaction (design §2.3a.9). For each
// row it reads the value through its current location, writes it to the
// target, flips the row, and removes the old copy. onRead is called once per
// value read, for the audit.
//
// Safe while a daemon serves: prepared material is revalidated under the
// row's lock. A changed row is retried, a deleted or already-moved row skipped,
// and an external write refuses to overwrite a value already at the target path. Idempotent and resumable: it
// aborts on the first row it can't move, naming it, with every earlier row
// committed.
func (s *Store) Migrate(ctx context.Context, target string, onRead func(owner, name string)) (MigrateResult, error) {
	var res MigrateResult
	if target == MigrateLocal && s.kek == nil && !s.serviceWrites {
		return res, fmt.Errorf("pg secretstore: migrating to local needs WARDYN_AGE_KEY or a WARDYN_KEK key service (transit or azurekv)")
	}
	if target != MigrateLocal && !s.reachable(target) {
		return res, fmt.Errorf("pg secretstore: migration target %q is not configured", target)
	}
	rows, err := s.pool.Query(ctx, `SELECT owned_by, name, enc_version, kek_id FROM secrets ORDER BY owned_by, name`)
	if err != nil {
		return res, fmt.Errorf("pg secretstore: migrate select: %w", err)
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (envelope, error) {
		var e envelope
		err := r.Scan(&e.ownedBy, &e.name, &e.version, &e.kekID)
		return e, err
	})
	if err != nil {
		return res, fmt.Errorf("pg secretstore: migrate scan: %w", err)
	}
	for _, e := range all {
		if s.atTarget(target, e) {
			continue
		}
		ok, soft, err := s.migrateRow(ctx, target, e.ownedBy, e.name, onRead)
		if err != nil {
			return res, fmt.Errorf("pg secretstore: migration to %s ABORTED at %s after moving %d rows (each moved row is committed; fix this row and re-run): %w",
				target, rowRef(e.ownedBy, e.name), res.Moved, err)
		}
		if ok {
			res.Moved++
		}
		if soft {
			res.SoftDeleted++
		}
	}
	return res, nil
}

func (s *Store) atTarget(target string, e envelope) bool {
	if target == MigrateLocal {
		// Any local row: moving between local keys is -rewrap's, not this.
		return e.version == encVersion || e.version == pkVersion
	}
	store, _ := splitRef(e.kekID)
	return e.version == extVersion && store == target
}

// Key and value reads precede the connection hold. The session lock protects
// reusable external objects, while a full locked snapshot comparison protects
// the row from local writers, erasure, expiry and another migration.
func (s *Store) migrateRow(ctx context.Context, target, owner, name string, onRead func(owner, name string)) (bool, bool, error) {
	ctx, cancel := s.bounded(ctx)
	defer cancel()
	for {
		e, err := readRowSnapshot(ctx, s.pool, owner, name, false)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && s.atTarget(target, e.envelope)) {
			return false, false, nil
		}
		if err != nil {
			return false, false, err
		}
		after, err := s.prepareMigration(ctx, target, e, onRead)
		if err != nil {
			return false, false, err
		}
		loc, err := s.commitMigration(ctx, target, e, after)
		clear(after.ct)
		if err != nil && loc != "" {
			// Commit may have reached the server despite a lost reply. The
			// locked cleanup leaves any object the current row still names.
			if _, derr := s.removeOldExternal(context.WithoutCancel(ctx), owner, name, target, loc); derr != nil {
				return false, false, fmt.Errorf("%w; the value written to %s could not be removed again (%v) — `wardynd -reconcile` lists it", err, target, derr)
			}
		}
		if errors.Is(err, secretstore.ErrRevisionChanged) {
			continue
		}
		if err != nil || target != MigrateLocal {
			return err == nil, false, err
		}
		store, old := splitRef(e.kekID)
		soft, err := s.removeOldExternal(ctx, owner, name, store, old)
		if err != nil {
			return false, false, fmt.Errorf("the row now holds the value locally, but its old copy in %s was not removed: %w — remove it there (`wardynd -reconcile` lists it)", store, err)
		}
		return true, soft, nil
	}
}

func (s *Store) prepareMigration(ctx context.Context, target string, e rowSnapshot, onRead func(owner, name string)) (sealedRow, error) {
	if e.version == 0 {
		return sealedRow{}, fmt.Errorf("is a pre-envelope (v0) row — boot this wardynd once to convert it, then re-run")
	}
	plain, err := s.open(ctx, e.envelope)
	if err != nil {
		return sealedRow{}, err
	}
	onRead(e.ownedBy, e.name)
	if target != MigrateLocal {
		return sealedRow{version: extVersion, ct: plain}, nil
	}
	defer clear(plain)
	return s.sealRow(ctx, e.ownedBy, e.name, plain)
}

func (s *Store) commitMigration(ctx context.Context, target string, before rowSnapshot, after sealedRow) (string, error) {
	conn, release, err := s.lockExternalRow(ctx, before.ownedBy, before.name)
	if err != nil {
		return "", err
	}
	defer release()
	cur, err := readRowSnapshot(ctx, conn, before.ownedBy, before.name, false)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && cur.revision != before.revision) {
		return "", secretstore.ErrRevisionChanged
	}
	if err != nil {
		return "", err
	}
	var loc string
	if target != MigrateLocal {
		loc, err = s.ext.Put(ctx, before.ownedBy, before.name, "", after.ct, true)
		if err != nil {
			return "", fmt.Errorf("write to %s: %w", target, err)
		}
		after = sealedRow{version: extVersion, kekID: target + ":" + loc}
	}
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return loc, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := replaceSnapshot(ctx, tx, before, after); err != nil {
		return loc, err
	}
	return loc, tx.Commit(ctx)
}

// Delete only while the row's session lock excludes external writers. A
// replacement may reuse the very object we intended to remove, so recheck it.
func (s *Store) removeOldExternal(ctx context.Context, owner, name, store, loc string) (bool, error) {
	ctx, cancel := s.bounded(ctx)
	defer cancel()
	conn, release, err := s.lockExternalRow(ctx, owner, name)
	if err != nil {
		return false, err
	}
	defer release()
	var version int16
	var kekID string
	err = conn.QueryRow(ctx, `SELECT enc_version, kek_id FROM secrets WHERE owned_by=$1 AND name=$2`, owner, name).Scan(&version, &kekID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("lock: %w", err)
	}
	if err == nil && version == extVersion {
		if curStore, curLoc := splitRef(kekID); curStore == store && secretstore.RefObject(curLoc) == secretstore.RefObject(loc) {
			return false, nil
		}
	}
	dctx, rep := secretstore.WithDeleteReport(ctx)
	if err := s.ext.Delete(dctx, owner, name, loc); err != nil {
		return false, err
	}
	return rep.Store != "" && !rep.Purged, nil
}

// flipRow rewrites one locked row to its new location. The caller commits.
func flipRow(ctx context.Context, tx pgx.Tx, owner, name string, version int16, kekID string, wrapped, ct []byte) error {
	if wrapped == nil {
		wrapped, ct = []byte{}, []byte{}
	}
	if _, err := tx.Exec(ctx,
		`UPDATE secrets SET enc_version=$3, kek_id=$4, wrapped_dek=$5, ciphertext=$6, updated_at=now() WHERE owned_by=$1 AND name=$2`,
		owner, name, version, kekID, wrapped, ct,
	); err != nil {
		return fmt.Errorf("update: %w", err)
	}
	return nil
}
