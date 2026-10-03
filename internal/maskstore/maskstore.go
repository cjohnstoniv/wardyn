// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package maskstore is the Postgres side of the shared masking registry
// (migration 0120): it implements secretmask.Backend, so every replica masks
// with the same committed corpus of per-run values and per-owner credential
// values, and a replica that cannot prove its copy is current fails closed.
//
// Each value is sealed with kek.Seal under its OWNER's 'cred' subject key
// (package subjectkey), so destroying that key leaves the rows undecryptable.
// A run's dispatch-time renderings are not here: they are the run's masking
// manifest (package maskmanifest), and a replica's corpus for a run is the
// manifest plus its per_run rows here.
//
// Writes commit before they return. A registering or evicting transaction takes
// its generation with UPDATE mask_gen SET gen = gen + 1 RETURNING gen, whose
// row lock is held to commit, so commit order is generation order and a reader
// that has read generation g has read every generation below it. A reader
// keeps a cursor, reads every row above it, and a consumer about to mask a
// chunk first waits for a read that began after the chunk arrived (Fresh).
// NOTIFY only hints that a read is due: every guarantee holds with no
// notification delivered.
package maskstore

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/maskmanifest"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
)

const (
	bucketRun    = "per_run"
	bucketGlobal = "owner_global"

	aadLabel    = "wardyn/mask-values/v1"
	digestLabel = "wardyn/mask-values/digest/v1"

	// Channel is the NOTIFY channel a commit announces its generation on: the
	// number only, never a value.
	Channel = "wardyn_mask"

	// TombstoneHorizon is how long a tombstone stays in the table. A replica
	// that was away longer than this reloads the whole table instead of reading
	// a gap.
	TombstoneHorizon = time.Hour

	// writeTimeout bounds one registration: a Postgres that does not answer
	// fails it, which fails the injection.
	writeTimeout = 10 * time.Second
)

// ErrFenced is a registration for a run whose person is being erased.
var ErrFenced = maskmanifest.ErrFenced

// Keys is the subject-key service values are sealed under.
type Keys = maskmanifest.Keys

// Store is one process's view of the shared corpus. Safe for concurrent use.
type Store struct {
	pool *pgxpool.Pool
	keys Keys
	reg  *secretmask.Registry
	sync syncer
}

var _ secretmask.Backend = (*Store)(nil)

// New returns the Store over pool, sealing with keys, and attaches it to reg as
// its Backend. Call Start to keep it fresh in the background.
func New(pool *pgxpool.Pool, keys Keys, reg *secretmask.Registry) *Store {
	s := &Store{pool: pool, keys: keys, reg: reg}
	s.sync.rows = map[uuid.UUID]*ref{}
	reg.SetBackend(s)
	return s
}

func aad(bucket string, id uuid.UUID, owner, scope string, version int) []byte {
	return kek.Encode(aadLabel, bucket, id.String(), owner, scope, strconv.Itoa(version))
}

// digestOf is the row's identity: an HMAC under the sealing key, so two
// replicas writing one value find one row and the table holds nothing that
// identifies the value once the key is gone.
func digestOf(key []byte, bucket, owner, scope string, version int, value []byte) []byte {
	derived := hmac.New(sha256.New, key)
	derived.Write([]byte(digestLabel))
	mac := hmac.New(sha256.New, derived.Sum(nil))
	mac.Write(kek.Encode(bucket, owner, scope, strconv.Itoa(version)))
	mac.Write(value)
	return mac.Sum(nil)
}

// sealed is one value ready to write.
type sealed struct {
	id     uuid.UUID
	digest []byte
	blob   []byte
	put    secretmask.GlobalPut
}

func seal(key []byte, version int, bucket, owner, scope string, put secretmask.GlobalPut) (sealed, error) {
	id := uuid.New()
	blob, err := kek.Seal(key, put.Value, aad(bucket, id, owner, scope, version))
	if err != nil {
		return sealed{}, fmt.Errorf("maskstore: seal a value: %w", err)
	}
	return sealed{id: id, digest: digestOf(key, bucket, owner, scope, version, put.Value), blob: blob, put: put}, nil
}

// commit runs fn in one READ COMMITTED transaction that holds the generation
// lock from its first statement to its commit, and announces the generation
// after. fn gets the generation its rows carry.
func (s *Store) commit(ctx context.Context, fn func(tx pgx.Tx, gen int64) error) (int64, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("maskstore: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var gen int64
	if err := tx.QueryRow(ctx, `UPDATE mask_gen SET gen = gen + 1 RETURNING gen`).Scan(&gen); err != nil {
		return 0, fmt.Errorf("maskstore: take a generation: %w", err)
	}
	if err := fn(tx, gen); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, Channel, strconv.FormatInt(gen, 10)); err != nil {
		return 0, fmt.Errorf("maskstore: notify: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("maskstore: commit: %w", err)
	}
	return gen, nil
}

// PutRun commits value as run runID's, sealed under the run owner's key (the
// owner its manifest names). A run with no manifest was dispatched before
// manifests existed: it is kept in this process only, as it stays uncovered. A
// fenced run refuses.
func (s *Store) PutRun(runID uuid.UUID, value []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	var owner string
	var fenced bool
	err := s.pool.QueryRow(ctx, `SELECT owner, fenced_at IS NOT NULL FROM run_mask_manifest WHERE run_id=$1`, runID).Scan(&owner, &fenced)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("maskstore: read the run's manifest: %w", err)
	case fenced:
		return ErrFenced
	}
	version, key, err := s.keys.Current(ctx, owner, subjectkey.PurposeCred)
	if err != nil {
		return fmt.Errorf("maskstore: the owner's key: %w", err)
	}
	defer clear(key)
	row, err := seal(key, version, bucketRun, owner, runID.String(), secretmask.GlobalPut{Value: value})
	if err != nil {
		return err
	}
	var existing uuid.UUID
	err = s.pool.QueryRow(ctx, `SELECT id FROM mask_values WHERE bucket=$1 AND digest=$2 AND NOT tombstone`, bucketRun, row.digest).Scan(&existing)
	if err == nil {
		return nil // another replica committed this value already: its row is the record
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("maskstore: look for the value: %w", err)
	}
	gen, err := s.commit(ctx, func(tx pgx.Tx, gen int64) error {
		// FOR SHARE keeps an erasure's fence (an UPDATE of this row) from
		// committing between this check and this commit.
		if err := tx.QueryRow(ctx, `SELECT fenced_at IS NOT NULL FROM run_mask_manifest WHERE run_id=$1 FOR SHARE`, runID).Scan(&fenced); err != nil {
			return fmt.Errorf("maskstore: lock the run's manifest: %w", err)
		}
		if fenced {
			return ErrFenced
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO mask_values (id, bucket, owner, run_id, digest, key_version, sealed, gen)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT DO NOTHING`,
			row.id, bucketRun, owner, runID, row.digest, version, row.blob, gen)
		if err != nil {
			return fmt.Errorf("maskstore: write a value: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.note(&ref{id: row.id, gen: gen, bucket: bucketRun, runID: runID, value: append([]byte(nil), value...), current: true})
	return nil
}

// PutGlobal commits values as credential (owner, name)'s current values. An
// operator-namespace credential (owner "") has no subject key and is kept in
// this process only: none exists today, and one would be sealed under a
// platform key that this release does not add.
func (s *Store) PutGlobal(owner, name string, values []secretmask.GlobalPut, merge bool, now time.Time) error {
	if owner == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	version, key, err := s.keys.Current(ctx, owner, subjectkey.PurposeCred)
	if err != nil {
		return fmt.Errorf("maskstore: the owner's key: %w", err)
	}
	defer clear(key)
	rows := make([]sealed, 0, len(values))
	keep := make([][]byte, 0, len(values))
	for _, p := range values {
		r, err := seal(key, version, bucketGlobal, owner, name, p)
		if err != nil {
			return err
		}
		rows = append(rows, r)
		keep = append(keep, r.digest)
	}
	var noted []*ref
	gen, err := s.commit(ctx, func(tx pgx.Tx, gen int64) error {
		for _, r := range rows {
			id, err := putGlobalRow(ctx, tx, owner, name, version, gen, merge, r)
			if err != nil {
				return err
			}
			noted = append(noted, &ref{id: id, bucket: bucketGlobal, owner: owner, name: name,
				value: append([]byte(nil), r.put.Value...), until: r.put.Until, current: true})
		}
		if merge {
			return nil
		}
		// Every other current value of the credential is retired, at its own
		// expiry when that is later (a cached copy may still be served until it
		// expires).
		_, err := tx.Exec(ctx,
			`UPDATE mask_values SET retired_at = GREATEST($1::timestamptz, COALESCE(until, $1::timestamptz)), gen = $2, updated_at = now()
			 WHERE bucket = $3 AND owner = $4 AND name = $5 AND NOT tombstone AND retired_at IS NULL AND NOT (digest = ANY($6))`,
			now, gen, bucketGlobal, owner, name, keep)
		if err != nil {
			return fmt.Errorf("maskstore: retire the replaced values: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, r := range noted {
		r.gen = gen
		s.note(r)
	}
	return nil
}

// putGlobalRow writes one current value: a new row, or the existing row of the
// same value made current again with its expiry. It returns the row's id.
func putGlobalRow(ctx context.Context, tx pgx.Tx, owner, name string, version int, gen int64, merge bool, r sealed) (uuid.UUID, error) {
	var id uuid.UUID
	var until, retired *time.Time
	err := tx.QueryRow(ctx, `SELECT id, until, retired_at FROM mask_values WHERE bucket=$1 AND digest=$2 AND NOT tombstone FOR UPDATE`,
		bucketGlobal, r.digest).Scan(&id, &until, &retired)
	var want *time.Time
	if !r.put.Until.IsZero() {
		u := r.put.Until.UTC()
		want = &u
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		id = r.id
		_, err = tx.Exec(ctx,
			`INSERT INTO mask_values (id, bucket, owner, name, digest, key_version, sealed, until, gen)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			id, bucketGlobal, owner, name, r.digest, version, r.blob, want, gen)
		if err != nil {
			return uuid.Nil, fmt.Errorf("maskstore: write a value: %w", err)
		}
		return id, nil
	case err != nil:
		return uuid.Nil, fmt.Errorf("maskstore: look for the value: %w", err)
	}
	if merge {
		want = laterExpiry(until, want)
	}
	if retired == nil && sameTime(until, want) {
		return id, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE mask_values SET until = $1, retired_at = NULL, gen = $2, updated_at = now() WHERE id = $3`, want, gen, id); err != nil {
		return uuid.Nil, fmt.Errorf("maskstore: renew a value: %w", err)
	}
	return id, nil
}

// laterExpiry is the later of two expiries, where nil means none.
func laterExpiry(a, b *time.Time) *time.Time {
	if a == nil || b == nil {
		return nil
	}
	if a.After(*b) {
		return a
	}
	return b
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// tombstoneSQL is the one statement shape that evicts: no ciphertext, no digest,
// no owner or name survive in the row.
const tombstoneSet = `tombstone = true, sealed = NULL, digest = NULL, owner = '', name = '', until = NULL, gen = $1, updated_at = now()`

// EvictGlobal tombstones the credential's current values. A replica keeps one
// masked in its cache until its own grace passes (retired_at is set to now).
func (s *Store) EvictGlobal(owner, name string, now time.Time) error {
	if owner == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	var any bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM mask_values WHERE bucket=$1 AND owner=$2 AND name=$3 AND NOT tombstone AND retired_at IS NULL)`,
		bucketGlobal, owner, name).Scan(&any); err != nil {
		return fmt.Errorf("maskstore: look for the credential's values: %w", err)
	}
	if !any {
		return nil
	}
	_, err := s.commit(ctx, func(tx pgx.Tx, gen int64) error {
		_, err := tx.Exec(ctx,
			`UPDATE mask_values SET `+tombstoneSet+`, retired_at = $2
			 WHERE bucket=$3 AND owner=$4 AND name=$5 AND NOT tombstone AND retired_at IS NULL`,
			gen, now, bucketGlobal, owner, name)
		if err != nil {
			return fmt.Errorf("maskstore: evict the credential's values: %w", err)
		}
		return nil
	})
	return err
}

// dueCond selects the credential values SweepGlobals evicts: retired before the
// cutoff, or still current but expired before it. bucket and cutoff are the
// numbers of the statement's parameters that carry them.
func dueCond(bucket, cutoff int) string {
	return fmt.Sprintf(`bucket = $%[1]d AND NOT tombstone AND ((retired_at IS NOT NULL AND retired_at < $%[2]d) OR (retired_at IS NULL AND until IS NOT NULL AND until < $%[2]d))`, bucket, cutoff)
}

// SweepGlobals tombstones the credential values retired, or expired, before
// cutoff, and prunes the tombstones past the horizon.
func (s *Store) SweepGlobals(ctx context.Context, cutoff time.Time) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM mask_values WHERE `+dueCond(1, 2), bucketGlobal, cutoff).Scan(&n); err != nil {
		return 0, fmt.Errorf("maskstore: count the values due: %w", err)
	}
	if n > 0 {
		_, err := s.commit(ctx, func(tx pgx.Tx, gen int64) error {
			tag, err := tx.Exec(ctx, `UPDATE mask_values SET `+tombstoneSet+`, retired_at = NULL WHERE `+dueCond(2, 3), gen, bucketGlobal, cutoff)
			if err != nil {
				return fmt.Errorf("maskstore: sweep the credential values: %w", err)
			}
			n = int(tag.RowsAffected())
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	return n, s.prune(ctx)
}

// prune deletes the tombstones past the horizon and records the highest
// generation it deleted, in one transaction, so a reader's snapshot sees both
// or neither.
func (s *Store) prune(ctx context.Context) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("maskstore: begin prune: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var top *int64
	if err := tx.QueryRow(ctx,
		`WITH gone AS (DELETE FROM mask_values WHERE tombstone AND updated_at < now() - make_interval(secs => $1) RETURNING gen)
		 SELECT max(gen) FROM gone`, TombstoneHorizon.Seconds()).Scan(&top); err != nil {
		return fmt.Errorf("maskstore: prune tombstones: %w", err)
	}
	if top == nil {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE mask_gen SET pruned = GREATEST(pruned, $1)`, *top); err != nil {
		return fmt.Errorf("maskstore: record the prune horizon: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("maskstore: commit prune: %w", err)
	}
	return nil
}

// PersistedRuns lists the runs that have a masking manifest or a live per-run
// value.
func (s *Store) PersistedRuns(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT run_id FROM run_mask_manifest UNION SELECT run_id FROM mask_values WHERE bucket = $1 AND NOT tombstone`, bucketRun)
	if err != nil {
		return nil, fmt.Errorf("maskstore: list the runs with masking state: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("maskstore: list the runs with masking state: %w", err)
	}
	return ids, nil
}

// PurgeRuns tombstones the per-run values of runs and deletes their masking
// manifests (and, by cascade, the manifest values), in one transaction.
func (s *Store) PurgeRuns(ctx context.Context, runs []uuid.UUID) error {
	if len(runs) == 0 {
		return nil
	}
	_, err := s.commit(ctx, func(tx pgx.Tx, gen int64) error {
		if _, err := tx.Exec(ctx,
			`UPDATE mask_values SET `+tombstoneSet+`, retired_at = NULL WHERE bucket = $2 AND run_id = ANY($3) AND NOT tombstone`,
			gen, bucketRun, runs); err != nil {
			return fmt.Errorf("maskstore: tombstone the runs' values: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM run_mask_manifest WHERE run_id = ANY($1)`, runs); err != nil {
			return fmt.Errorf("maskstore: delete the runs' manifests: %w", err)
		}
		return nil
	})
	return err
}

// EraseOwner tombstones every value committed under owner, then reports how
// many rows still hold ciphertext for it (a registration that raced the erase
// can leave one: the caller retries until it is zero), and drops what this
// process cached.
func (s *Store) EraseOwner(ctx context.Context, owner string) (int, error) {
	if owner == "" {
		return 0, errors.New("maskstore: erasing the operator namespace is not a person's erasure")
	}
	var any bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM mask_values WHERE owner=$1 AND NOT tombstone)`, owner).Scan(&any); err != nil {
		return 0, fmt.Errorf("maskstore: look for the person's values: %w", err)
	}
	if any {
		_, err := s.commit(ctx, func(tx pgx.Tx, gen int64) error {
			if _, err := tx.Exec(ctx, `UPDATE mask_values SET `+tombstoneSet+`, retired_at = NULL WHERE owner = $2 AND NOT tombstone`, gen, owner); err != nil {
				return fmt.Errorf("maskstore: erase the person's values: %w", err)
			}
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	// This replica drops its copies now; the others at their next read.
	if err := s.Fresh(ctx, time.Now()); err != nil {
		return 0, err
	}
	var left int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM mask_values WHERE owner=$1 AND NOT tombstone`, owner).Scan(&left); err != nil {
		return 0, fmt.Errorf("maskstore: recount the person's values: %w", err)
	}
	return left, nil
}
