// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// extVersion is the store-mode row (credential-storage design §2.2, §2.3a): a
// POINTER. The value lives in the external store; kek_id is
// "<store>:<ref>", and wrapped_dek and ciphertext are empty. Wardyn does no
// cryptography for such a row.
//
// The ordering is what keeps the two systems safe to disagree: Put writes the
// store before the row, Delete removes the value before the row. A failure in
// between leaves at worst a value no row points to (-reconcile reports it) or
// a row whose value is gone, which reads as a definitive refusal, never as
// someone else's value and never as not-found (rules 17, 18).
const extVersion = 2

// splitRef splits a pointer row's kek_id into its store and ref.
func splitRef(kekID string) (store, ref string) {
	store, ref, ok := strings.Cut(kekID, ":")
	if !ok {
		return kekID, ""
	}
	return store, ref
}

// reachable reports whether this store's external client is the one a pointer
// row names.
func (s *Store) reachable(store string) bool {
	return s.ext != nil && s.ext.Name() == store
}

// openExternal reads the value a pointer row names. A pointer to a store this
// wardynd cannot reach refuses by name (rule 15); the external client derives
// the ref from the row and checks the value's own binding (rule 16).
func (s *Store) openExternal(ctx context.Context, e envelope) ([]byte, error) {
	ref := rowRef(e.ownedBy, e.name)
	store, loc := splitRef(e.kekID)
	if !s.reachable(store) {
		return nil, fmt.Errorf("pg secretstore: %s is stored in %q, which this wardynd is not configured to reach", ref, store)
	}
	v, err := s.ext.Get(ctx, e.ownedBy, e.name, loc)
	if err != nil {
		return nil, extErr(ref, err)
	}
	return v, nil
}

// extErr wraps an external failure for the row. The row EXISTS, so an absent
// value is a lost credential: the not-found sentinel is dropped, so that no
// caller (loadOrCreateSecret above all) ever mints over it (rule 17).
func extErr(ref string, err error) error {
	if errors.Is(err, secretstore.ErrNotFound) {
		return fmt.Errorf("pg secretstore: %s refused: %s", ref, err.Error())
	}
	return fmt.Errorf("pg secretstore: %s: %w", ref, err)
}

// putExternal writes the value to the external store, then points the row at
// it. If the row cannot be written, a value the row never pointed to is
// removed again rather than orphaned.
func (s *Store) putExternal(ctx context.Context, name string, value []byte) error {
	ref := rowRef(s.owner, name)
	prev, err := s.currentRef(ctx, name)
	if err != nil {
		return err
	}
	loc, err := s.ext.Put(ctx, s.owner, name, prev, value, false)
	if err != nil {
		return fmt.Errorf("pg secretstore: put %s to %s: %w", ref, s.ext.Name(), err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO secrets (owned_by, name, enc_version, kek_id, wrapped_dek, ciphertext)
		VALUES ($1, $2, $3, $4, ''::bytea, ''::bytea)
		ON CONFLICT (owned_by, name) DO UPDATE
			SET enc_version=$3, kek_id=$4, wrapped_dek=''::bytea, ciphertext=''::bytea, updated_at=now()`,
		s.owner, name, extVersion, s.ext.Name()+":"+loc,
	)
	if err == nil {
		return nil
	}
	if loc != prev {
		if derr := s.ext.Delete(context.WithoutCancel(ctx), s.owner, name, loc); derr != nil {
			return fmt.Errorf("pg secretstore: put %s: the value reached %s but the row did not (%w), and removing the value failed too (%v) — `wardynd -reconcile` lists it", ref, s.ext.Name(), err, derr)
		}
	}
	return fmt.Errorf("pg secretstore: put %s: the value reached %s but the row did not, so it was removed again: %w", ref, s.ext.Name(), err)
}

// currentRef is the ref this view's own row points to in the configured
// external store, or "" when it has no such row.
func (s *Store) currentRef(ctx context.Context, name string) (string, error) {
	var kekID string
	err := s.pool.QueryRow(ctx,
		`SELECT kek_id FROM secrets WHERE owned_by=$1 AND name=$2 AND enc_version=$3`,
		s.owner, name, extVersion,
	).Scan(&kekID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("pg secretstore: read %s: %w", rowRef(s.owner, name), err)
	}
	if store, ref := splitRef(kekID); s.reachable(store) {
		return ref, nil
	}
	return "", nil
}

// deleteExternal removes the external value behind this view's own pointer
// row, if it has one, before the caller deletes the row. A pointer into a
// store this wardynd cannot reach is refused: removing only the row would
// leave the value behind with nothing pointing at it.
func (s *Store) deleteExternal(ctx context.Context, name string) error {
	var kekID string
	err := s.pool.QueryRow(ctx,
		`SELECT kek_id FROM secrets WHERE owned_by=$1 AND name=$2 AND enc_version=$3`,
		s.owner, name, extVersion,
	).Scan(&kekID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	ref := rowRef(s.owner, name)
	if err != nil {
		return fmt.Errorf("pg secretstore: delete %s: %w", ref, err)
	}
	store, loc := splitRef(kekID)
	if !s.reachable(store) {
		return fmt.Errorf("pg secretstore: delete %s: it is stored in %q, which this wardynd is not configured to reach; deleting only the row would leave the value behind", ref, store)
	}
	if err := s.ext.Delete(ctx, s.owner, name, loc); err != nil {
		return fmt.Errorf("pg secretstore: delete %s from %s (the row is kept): %w", ref, store, err)
	}
	return nil
}

// MigrateLocal is the -migrate-secrets target that seals rows locally.
const MigrateLocal = "local"

// Migrate moves every row not already at target ("local", or the configured
// external store's name) there, one row per transaction, and returns how many
// it moved (design §2.3a.9). For each row it reads the value through the row's
// current location, writes it to the target, flips the row, and removes the
// old copy. onRead is called once per value read, for the audit.
//
// Safe while a daemon serves: each row is locked while it moves, a row that
// reached the target meanwhile is skipped, and an external write refuses to
// overwrite a value already at the target path (a concurrent Put landing, or
// an orphan -reconcile lists). Idempotent and resumable: it aborts on the
// first row it cannot move, naming it, with every earlier row committed.
func (s *Store) Migrate(ctx context.Context, target string, onRead func(owner, name string)) (int, error) {
	if target == MigrateLocal && s.kek == nil {
		return 0, fmt.Errorf("pg secretstore: migrating to local needs WARDYN_AGE_KEY")
	}
	if target != MigrateLocal && !s.reachable(target) {
		return 0, fmt.Errorf("pg secretstore: migration target %q is not configured", target)
	}
	rows, err := s.pool.Query(ctx, `SELECT owned_by, name, enc_version, kek_id FROM secrets ORDER BY owned_by, name`)
	if err != nil {
		return 0, fmt.Errorf("pg secretstore: migrate select: %w", err)
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (envelope, error) {
		var e envelope
		err := r.Scan(&e.ownedBy, &e.name, &e.version, &e.kekID)
		return e, err
	})
	if err != nil {
		return 0, fmt.Errorf("pg secretstore: migrate scan: %w", err)
	}
	moved := 0
	for _, e := range all {
		if s.atTarget(target, e) {
			continue
		}
		ok, err := s.migrateRow(ctx, target, e.ownedBy, e.name, onRead)
		if err != nil {
			return moved, fmt.Errorf("pg secretstore: migration to %s ABORTED at %s after moving %d rows (each moved row is committed; fix this row and re-run): %w",
				target, rowRef(e.ownedBy, e.name), moved, err)
		}
		if ok {
			moved++
		}
	}
	return moved, nil
}

func (s *Store) atTarget(target string, e envelope) bool {
	if target == MigrateLocal {
		return e.version == encVersion && e.kekID == s.kek.ID()
	}
	store, _ := splitRef(e.kekID)
	return e.version == extVersion && store == target
}

// migrateRow moves one row under a row lock. The old external copy is removed
// only AFTER the row flip commits: a failure there leaves an orphan the error
// names, never a row pointing at nothing.
func (s *Store) migrateRow(ctx context.Context, target, owner, name string, onRead func(owner, name string)) (bool, error) {
	tx, err := beginReadCommitted(ctx, s.pool)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	e := envelope{ownedBy: owner, name: name}
	err = tx.QueryRow(ctx,
		`SELECT enc_version, kek_id, wrapped_dek, ciphertext FROM secrets WHERE owned_by=$1 AND name=$2 FOR UPDATE`,
		owner, name,
	).Scan(&e.version, &e.kekID, &e.wrapped, &e.ct)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && s.atTarget(target, e)) {
		return false, nil // deleted, or moved by a concurrent Put
	}
	if err != nil {
		return false, fmt.Errorf("lock: %w", err)
	}
	if e.version == 0 {
		return false, fmt.Errorf("is a pre-envelope (v0) row — boot this wardynd once to convert it, then re-run")
	}
	plain, err := s.open(ctx, e)
	if err != nil {
		return false, err
	}
	onRead(owner, name)

	if target != MigrateLocal {
		loc, err := s.ext.Put(ctx, owner, name, "", plain, true)
		if err != nil {
			return false, fmt.Errorf("write to %s: %w", target, err)
		}
		if err := flipRow(ctx, tx, owner, name, extVersion, target+":"+loc, nil, nil); err != nil {
			if derr := s.ext.Delete(context.WithoutCancel(ctx), owner, name, loc); derr != nil {
				return false, fmt.Errorf("%w; the value written to %s could not be removed again (%v) — `wardynd -reconcile` lists it", err, target, derr)
			}
			return false, err
		}
		return true, nil
	}

	wrapped, ct, err := seal(ctx, s.kek, owner, name, plain)
	if err != nil {
		return false, err
	}
	if err := flipRow(ctx, tx, owner, name, encVersion, s.kek.ID(), wrapped, ct); err != nil {
		return false, err
	}
	store, loc := splitRef(e.kekID)
	if err := s.ext.Delete(ctx, owner, name, loc); err != nil {
		return false, fmt.Errorf("the row now holds the value locally, but its old copy in %s was not removed: %w — remove it there (`wardynd -reconcile` lists it)", store, err)
	}
	return true, nil
}

// flipRow rewrites one locked row to its new location and commits.
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
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// ReconcileReport is what -reconcile found. Neither list is ever acted on:
// reconcile reports, an operator decides.
type ReconcileReport struct {
	// Checked is how many pointer rows were checked against the store.
	Checked int
	// Dangling names each pointer row whose value is absent, mis-bound or in a
	// store this wardynd cannot reach, with the reason.
	Dangling []string
	// Orphans are values in the store that no row points to.
	Orphans []secretstore.ExternalEntry
}

// Reconcile lists both sides of store mode and reports where they disagree
// (design §2.3a.1). It reads store metadata only, never a value, and deletes
// nothing. A transient store failure aborts it: a half-checked report would
// read as drift that is not there.
func (s *Store) Reconcile(ctx context.Context) (ReconcileReport, error) {
	var rep ReconcileReport
	if s.ext == nil {
		return rep, fmt.Errorf("pg secretstore: reconcile needs an external store configured")
	}
	rows, err := s.pool.Query(ctx, `SELECT owned_by, name, kek_id FROM secrets WHERE enc_version=$1 ORDER BY owned_by, name`, extVersion)
	if err != nil {
		return rep, fmt.Errorf("pg secretstore: reconcile select: %w", err)
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (envelope, error) {
		var e envelope
		err := r.Scan(&e.ownedBy, &e.name, &e.kekID)
		return e, err
	})
	if err != nil {
		return rep, fmt.Errorf("pg secretstore: reconcile scan: %w", err)
	}
	pointed := map[string]bool{}
	for _, e := range all {
		ref := rowRef(e.ownedBy, e.name)
		store, loc := splitRef(e.kekID)
		if !s.reachable(store) {
			rep.Dangling = append(rep.Dangling, fmt.Sprintf("%s: stored in %q, which this wardynd is not configured to reach", ref, store))
			continue
		}
		rep.Checked++
		// A row points at loc whether or not its value is live: a soft-deleted
		// value behind a row is dangling, not an orphan as well.
		pointed[loc] = true
		err := s.ext.Check(ctx, e.ownedBy, e.name, loc)
		switch {
		case errors.Is(err, secretstore.ErrUnavailable):
			return rep, fmt.Errorf("pg secretstore: reconcile aborted at %s: %w", ref, err)
		case err != nil:
			rep.Dangling = append(rep.Dangling, fmt.Sprintf("%s: %v", ref, err))
		}
	}
	found, err := s.ext.Walk(ctx)
	if err != nil {
		return rep, fmt.Errorf("pg secretstore: reconcile list %s: %w", s.ext.Name(), err)
	}
	for _, f := range found {
		if !pointed[f.Ref] {
			rep.Orphans = append(rep.Orphans, f)
		}
	}
	return rep, nil
}
