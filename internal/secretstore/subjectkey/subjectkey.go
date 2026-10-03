// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package subjectkey keeps one 32-byte key per (owner, purpose, generation) in
// Postgres (the principal_keys table), each wrapped under its key domain's KEK
// through the kek.KEK seam, so the local key, Vault Transit and Azure Key Vault
// all serve it unchanged. A person's credential rows, the run masking copies
// ('cred') and sealed audit fields ('audit-seal') sit under keys that can be
// destroyed one subject at a time.
//
// The package is always live: it does not read WARDYN_PRINCIPAL_KEYS, which
// governs only whether credential rows are written under these keys.
//
// SECURITY: a revocation is durable and checked on every use. Manager.Key reads
// the generation's destroyed_at each time, so a replica whose cache is warm
// stops decrypting at its next use after Destroy, with no notification, and a
// Postgres that does not answer fails the use. The cache saves the KEK round
// trip, never that check. Clearing a key's bytes on eviction is best effort:
// Go's runtime may already have copied them.
package subjectkey

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The purposes a subject key serves; the table's CHECK holds the same two.
const (
	PurposeCred      = "cred"
	PurposeAuditSeal = "audit-seal"
)

// DomainDefault is the deployment's credential KEK. Until key domains exist,
// every principal key is in it.
const DomainDefault = "default"

// destroyActor is the audit actor of principal_key.destroyed; the person or
// route that asked for the erase is recorded by its own event.
const destroyActor = "wardyn/subjectkey"

// ErrDataLoss is a use of a generation whose key was destroyed (or whose row is
// gone): what it sealed is unrecoverable. Definitive, never transient.
var ErrDataLoss = errors.New("subject key destroyed: what it sealed is unrecoverable")

// ErrOperatorOwner refuses owner "": the operator namespace holds the boot
// keys, which are never subject keys (compare secretstore.EraseOwner).
var ErrOperatorOwner = errors.New("subjectkey: the operator namespace has no subject key")

// Resolver finds the KEK of a key domain. Writer is the KEK a new generation in
// domain is wrapped under; Reader is the KEK that opens a wrap naming kekID.
type Resolver struct {
	Writer func(domain string) (kek.KEK, error)
	Reader func(domain, kekID string) (kek.KEK, error)
}

// Manager reads, creates and destroys subject keys over one pool. Safe for
// concurrent use; its cache is per process.
type Manager struct {
	pool  *pgxpool.Pool
	keks  Resolver
	cache *cache
}

// New returns a Manager over pool whose keys are wrapped as keks resolves.
func New(pool *pgxpool.Pool, keks Resolver) *Manager {
	return &Manager{pool: pool, keks: keks, cache: newCache(cacheMax, cacheTTL)}
}

func check(owner, purpose string) error {
	if owner == "" {
		return ErrOperatorOwner
	}
	if purpose != PurposeCred && purpose != PurposeAuditSeal {
		return fmt.Errorf("subjectkey: unknown purpose %q", purpose)
	}
	return nil
}

// unavailable marks a Postgres failure transient to the caller: the use fails,
// and a caller that rides outages on a last-good value may.
func unavailable(op string, err error) error {
	return fmt.Errorf("subjectkey: %s: %w: %w", op, secretstore.ErrUnavailable, err)
}

// createAttempts bounds the read-create-read loop of Current; a lap is lost
// only to another writer's create or a Destroy, so a few is plenty.
const createAttempts = 4

// Current returns the live generation's version and a copy of its key, creating
// generation max+1 first when there is none (a first write, or the first after
// a Destroy). The caller owns the copy and clears it.
//
// Nothing is sealed under a generated key until its row is committed and read
// back: the create is INSERT ... ON CONFLICT DO NOTHING against the one-live
// index, and the key returned is always the one read back and unwrapped from
// the winning row, so a concurrent loser discards its own key.
func (m *Manager) Current(ctx context.Context, owner, purpose string) (int, []byte, error) {
	if err := check(owner, purpose); err != nil {
		return 0, nil, err
	}
	for range createAttempts {
		var version int
		err := m.pool.QueryRow(ctx,
			`SELECT version FROM principal_keys WHERE owner=$1 AND purpose=$2 AND destroyed_at IS NULL`, owner, purpose).Scan(&version)
		switch {
		case err == nil:
			key, err := m.Key(ctx, owner, purpose, version)
			if errors.Is(err, ErrDataLoss) {
				continue // destroyed since the read: the next lap creates the next generation
			}
			return version, key, err
		case errors.Is(err, pgx.ErrNoRows):
			if err := m.create(ctx, owner, purpose); err != nil {
				return 0, nil, err
			}
		default:
			return 0, nil, unavailable("read the live generation", err)
		}
	}
	return 0, nil, fmt.Errorf("subjectkey: no stable generation for (owner=%q, purpose=%q) after %d attempts", owner, purpose, createAttempts)
}

// create inserts generation max+1 of (owner, purpose) unless another writer's
// live row is already there. Either outcome is success; the caller re-reads.
func (m *Manager) create(ctx context.Context, owner, purpose string) error {
	var next int
	if err := m.pool.QueryRow(ctx,
		`SELECT COALESCE(max(version), 0) + 1 FROM principal_keys WHERE owner=$1 AND purpose=$2`, owner, purpose).Scan(&next); err != nil {
		return unavailable("read the next generation", err)
	}
	w, err := m.keks.Writer(DomainDefault)
	if err != nil {
		return fmt.Errorf("subjectkey: %w", err)
	}
	key := make([]byte, kek.DEKSize)
	defer clear(key)
	if _, err := rand.Read(key); err != nil {
		return fmt.Errorf("subjectkey: draw a key: %w", err)
	}
	wrapped, err := w.Wrap(ctx, key, kek.PrincipalBind(owner, purpose, next, DomainDefault))
	if err != nil {
		return fmt.Errorf("subjectkey: wrap generation %d of (owner=%q, purpose=%q): %w", next, owner, purpose, err)
	}
	_, err = m.pool.Exec(ctx, `
		INSERT INTO principal_keys (owner, purpose, version, domain, kek_id, wrapped_key)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (owner, purpose) WHERE destroyed_at IS NULL DO NOTHING`,
		owner, purpose, next, DomainDefault, w.ID(), wrapped)
	// The same version taken by a writer that has since been destroyed is a lost
	// race too: the next lap reads max again.
	var pgErr *pgconn.PgError
	if err != nil && !(errors.As(err, &pgErr) && pgErr.Code == "23505") {
		return unavailable("insert a generation", err)
	}
	return nil
}

// Key returns a copy of one generation's key, which the caller owns and clears.
// Every call reads the generation's destroyed_at first, cached or not: a set
// value evicts the entry and returns ErrDataLoss, and a Postgres that does not
// answer fails the call (secretstore.ErrUnavailable) rather than serve a key
// that may have been revoked.
func (m *Manager) Key(ctx context.Context, owner, purpose string, version int) ([]byte, error) {
	if err := check(owner, purpose); err != nil {
		return nil, err
	}
	id := keyID{owner, purpose, version}
	key, hit := m.cache.get(id)
	if !hit {
		return m.fill(ctx, id)
	}
	var destroyed bool
	err := m.pool.QueryRow(ctx,
		`SELECT destroyed_at IS NOT NULL FROM principal_keys WHERE owner=$1 AND purpose=$2 AND version=$3`, owner, purpose, version).Scan(&destroyed)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		destroyed = true // a row deleted under a warm cache is a revocation too
	case err != nil:
		clear(key)
		return nil, unavailable("check the generation", err)
	}
	if destroyed {
		clear(key)
		m.cache.evict(id)
		return nil, dataLoss(id)
	}
	return key, nil
}

func dataLoss(id keyID) error {
	return fmt.Errorf("subjectkey: generation %d of (owner=%q, purpose=%q): %w", id.version, id.owner, id.purpose, ErrDataLoss)
}

// fill reads one generation's row, unwraps its key and caches it.
func (m *Manager) fill(ctx context.Context, id keyID) ([]byte, error) {
	var destroyed bool
	var domain, kekID string
	var wrapped []byte
	err := m.pool.QueryRow(ctx,
		`SELECT destroyed_at IS NOT NULL, domain, kek_id, wrapped_key FROM principal_keys WHERE owner=$1 AND purpose=$2 AND version=$3`,
		id.owner, id.purpose, id.version).Scan(&destroyed, &domain, &kekID, &wrapped)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, dataLoss(id)
	case err != nil:
		return nil, unavailable("read the generation", err)
	case destroyed:
		return nil, dataLoss(id)
	}
	k, err := m.keks.Reader(domain, kekID)
	if err != nil {
		return nil, fmt.Errorf("subjectkey: generation %d of (owner=%q, purpose=%q): %w", id.version, id.owner, id.purpose, err)
	}
	key, err := k.Unwrap(ctx, wrapped, kek.PrincipalBind(id.owner, id.purpose, id.version, domain))
	if err != nil {
		return nil, fmt.Errorf("subjectkey: generation %d of (owner=%q, purpose=%q) does not unwrap (moved, forged, corrupted, or its key version retired): %w", id.version, id.owner, id.purpose, err)
	}
	m.cache.put(id, key)
	return key, nil
}

// Destroy tombstones every generation of (owner, purpose) in one transaction:
// destroyed_at is set, wrapped_key cleared, and principal_key.destroyed written
// with the owner, purpose and generation numbers, never key material. Rows are
// never deleted, so a version is never reused and the next write creates
// max+1. It returns the generations this call destroyed (none when the subject
// had no live key, which writes no event). The local cache is evicted only after
// the commit; other replicas stop at their next use (Key).
func (m *Manager) Destroy(ctx context.Context, owner, purpose string) ([]int, error) {
	if err := check(owner, purpose); err != nil {
		return nil, err
	}
	tx, err := m.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, unavailable("begin destroy", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	// A generation already destroyed is tombstoned (the table's CHECK ties
	// wrapped_key to destroyed_at), so only the live one is left to change.
	rows, err := tx.Query(ctx,
		`UPDATE principal_keys SET destroyed_at=now(), wrapped_key=NULL WHERE owner=$1 AND purpose=$2 AND destroyed_at IS NULL RETURNING version`, owner, purpose)
	if err != nil {
		return nil, unavailable("destroy", err)
	}
	gens, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		return nil, unavailable("destroy", err)
	}
	slices.Sort(gens)
	if len(gens) > 0 {
		data, _ := json.Marshal(map[string]any{"owner": owner, "purpose": purpose, "generations": gens})
		ev := types.AuditEvent{
			ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem, Actor: destroyActor,
			Action: "principal_key.destroyed", Target: owner, Outcome: "success", Data: data,
		}
		if err := store.InsertAuditEventTx(ctx, tx, &ev); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, unavailable("commit destroy", err)
	}
	m.cache.evictSubject(owner, purpose)
	return gens, nil
}
