// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package maskmanifest keeps each run's masking manifest in Postgres: the exact
// bytes of every rendering of a secret the run received, committed at dispatch
// before the sandbox can see a value (migration 0109). It is what lets a
// restarted, or a second, wardynd mask a run it did not dispatch, and what lets
// a door that relays or persists a run's output prove its corpus is whole
// (Covered) instead of passing bytes through.
//
// Re-resolving a run's secrets by name after a restart would be wrong: a
// secret rotated after dispatch resolves to its new value while the sandbox
// still holds the old one. The manifest holds what the run was given.
//
// Each value is sealed with kek.Seal under the run owner's 'cred' subject key
// (package subjectkey), so destroying that key leaves the rows undecryptable.
// The AAD binds the run id, the value's ordinal and the key version.
//
// SECURITY: coverage is decided in Postgres at every admission, never from this
// process's cache alone. Covered reads the manifest row each call and reloads
// the values when its revision moved, so a value appended on another replica is
// masked here from the next admission on, and a fence set anywhere is honoured
// here at once. Any failure to prove coverage answers false.
package maskmanifest

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
)

var (
	// ErrNoManifest is an Append or Complete on a run that has no manifest: a
	// run dispatched before manifests existed, which stays honestly uncovered.
	ErrNoManifest = errors.New("maskmanifest: the run has no masking manifest")
	// ErrFenced is an Append or Complete on a run whose manifest is fenced
	// (its subject is being erased).
	ErrFenced = errors.New("maskmanifest: the run's masking manifest is fenced")
	// ErrNoOwner refuses a manifest with no owner: the key a value is sealed
	// under is the owner's.
	ErrNoOwner = errors.New("maskmanifest: a masking manifest needs the run owner")
)

// Keys is the subject-key service a value is sealed under; *subjectkey.Manager
// is the production one.
type Keys interface {
	Current(ctx context.Context, owner, purpose string) (int, []byte, error)
	Key(ctx context.Context, owner, purpose string, version int) ([]byte, error)
}

var _ Keys = (*subjectkey.Manager)(nil)

// aadLabel is the first AAD field, so an AAD for this purpose never parses as
// another purpose's (kek.Encode).
const aadLabel = "wardyn/run-mask-manifest/v1"

func aad(runID uuid.UUID, ordinal, version int) []byte {
	return kek.Encode(aadLabel, runID.String(), strconv.Itoa(ordinal), strconv.Itoa(version))
}

// Manifests is one process's view of the manifests in a database. Safe for
// concurrent use.
type Manifests struct {
	pool *pgxpool.Pool
	keys Keys
	reg  *secretmask.Registry

	mu    sync.Mutex
	cache map[uuid.UUID]*entry
}

// entry is this process's copy of one run's manifest: the revision it was read
// at, whether it was complete, and a digest of each value it holds (never the
// value itself: the registry holds those).
type entry struct {
	rev      int
	complete bool
	have     map[[sha256.Size]byte]struct{}
}

// New returns Manifests over pool whose values are sealed with keys and, once
// opened, registered with reg.
func New(pool *pgxpool.Pool, keys Keys, reg *secretmask.Registry) *Manifests {
	return &Manifests{pool: pool, keys: keys, reg: reg, cache: map[uuid.UUID]*entry{}}
}

// Start creates runID's manifest, incomplete, owned by owner (the run's secret
// subject). Idempotent: an existing manifest is left as it is.
func (m *Manifests) Start(ctx context.Context, runID uuid.UUID, owner string) error {
	if owner == "" {
		return ErrNoOwner
	}
	if _, err := m.pool.Exec(ctx,
		`INSERT INTO run_mask_manifest (run_id, owner) VALUES ($1, $2) ON CONFLICT (run_id) DO NOTHING`, runID, owner); err != nil {
		return fmt.Errorf("maskmanifest: start: %w", err)
	}
	return nil
}

// Append seals each value (longer than secretmask.MinLen, as the registry
// ignores the rest) into runID's manifest, in one transaction, and registers it.
// It returns only once the rows are committed, so a caller returns a credential
// only after its renderings are on record; an error means they are not, and
// the credential must not be handed out. ErrNoManifest and ErrFenced are
// distinguished.
func (m *Manifests) Append(ctx context.Context, runID uuid.UUID, values ...[]byte) error {
	var todo [][]byte
	m.mu.Lock()
	e := m.cache[runID]
	for _, v := range values {
		if len(v) < secretmask.MinLen {
			continue
		}
		if e != nil {
			if _, dup := e.have[sha256.Sum256(v)]; dup {
				continue
			}
		}
		todo = append(todo, v)
	}
	m.mu.Unlock()
	if len(todo) == 0 {
		return nil
	}
	var owner string
	var fenced bool
	err := m.pool.QueryRow(ctx, `SELECT owner, fenced_at IS NOT NULL FROM run_mask_manifest WHERE run_id=$1`, runID).Scan(&owner, &fenced)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNoManifest
	case err != nil:
		return fmt.Errorf("maskmanifest: read the manifest: %w", err)
	case fenced:
		return ErrFenced
	}
	version, key, err := m.keys.Current(ctx, owner, subjectkey.PurposeCred)
	if err != nil {
		return fmt.Errorf("maskmanifest: the owner's key: %w", err)
	}
	defer clear(key)

	rev, err := m.insert(ctx, runID, owner, version, key, todo)
	if err != nil {
		return err
	}
	// The values are registered before any caller can hand the credential out.
	for _, v := range todo {
		m.reg.AddLocal(runID, v)
	}
	m.note(runID, rev, false, todo)
	return nil
}

// insert commits todo under the run's row lock and returns the new revision.
func (m *Manifests) insert(ctx context.Context, runID uuid.UUID, owner string, version int, key []byte, todo [][]byte) (int, error) {
	tx, err := m.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("maskmanifest: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var fenced bool
	if err := tx.QueryRow(ctx, `SELECT fenced_at IS NOT NULL FROM run_mask_manifest WHERE run_id=$1 FOR UPDATE`, runID).Scan(&fenced); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrNoManifest
		}
		return 0, fmt.Errorf("maskmanifest: lock the manifest: %w", err)
	}
	if fenced {
		return 0, ErrFenced
	}
	var next int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(ordinal) + 1, 0) FROM run_mask_values WHERE run_id=$1`, runID).Scan(&next); err != nil {
		return 0, fmt.Errorf("maskmanifest: next ordinal: %w", err)
	}
	for i, v := range todo {
		sealed, err := kek.Seal(key, v, aad(runID, next+i, version))
		if err != nil {
			return 0, fmt.Errorf("maskmanifest: seal a value: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO run_mask_values (run_id, ordinal, owner, key_version, sealed) VALUES ($1, $2, $3, $4, $5)`,
			runID, next+i, owner, version, sealed); err != nil {
			return 0, fmt.Errorf("maskmanifest: write a value: %w", err)
		}
	}
	var rev int
	if err := tx.QueryRow(ctx, `UPDATE run_mask_manifest SET revision = revision + 1 WHERE run_id=$1 RETURNING revision`, runID).Scan(&rev); err != nil {
		return 0, fmt.Errorf("maskmanifest: move the revision: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("maskmanifest: commit: %w", err)
	}
	return rev, nil
}

// Complete marks runID's manifest complete, last: every value is committed. A
// manifest is never complete before this call.
func (m *Manifests) Complete(ctx context.Context, runID uuid.UUID) error {
	var rev int
	err := m.pool.QueryRow(ctx,
		`UPDATE run_mask_manifest SET complete = true, revision = revision + 1 WHERE run_id=$1 AND fenced_at IS NULL RETURNING revision`, runID).Scan(&rev)
	if errors.Is(err, pgx.ErrNoRows) {
		var fenced bool
		switch qerr := m.pool.QueryRow(ctx, `SELECT fenced_at IS NOT NULL FROM run_mask_manifest WHERE run_id=$1`, runID).Scan(&fenced); {
		case errors.Is(qerr, pgx.ErrNoRows):
			return ErrNoManifest
		case qerr != nil:
			return fmt.Errorf("maskmanifest: complete: %w", qerr)
		}
		return ErrFenced
	}
	if err != nil {
		return fmt.Errorf("maskmanifest: complete: %w", err)
	}
	m.note(runID, rev, true, nil)
	return nil
}

// note records that this process wrote revision rev with the values added. The
// copy stays exact only when rev follows the revision it held; a gap means
// another replica wrote in between, so the entry is dropped and the next
// admission reloads.
func (m *Manifests) note(runID uuid.UUID, rev int, complete bool, added [][]byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.cache[runID]
	if e == nil {
		if rev > 1 {
			return // written before this process held a copy: the next admission loads
		}
		e = &entry{have: map[[sha256.Size]byte]struct{}{}}
		m.cache[runID] = e
	}
	if e.rev != rev-1 {
		delete(m.cache, runID)
		return
	}
	e.rev = rev
	e.complete = e.complete || complete
	for _, v := range added {
		e.have[sha256.Sum256(v)] = struct{}{}
	}
}

// Covered reports whether runID's corpus is provably whole here: its manifest
// row is present, complete and not fenced in Postgres right now, and this
// process holds every value at the row's revision. A Postgres that does not
// answer, a value that does not open, or a missing row is false. Values are
// loaded into the registry as a side effect, so a covered run is masked.
func (m *Manifests) Covered(ctx context.Context, runID uuid.UUID) bool {
	var owner string
	var complete, fenced bool
	var rev int
	err := m.pool.QueryRow(ctx,
		`SELECT owner, complete, fenced_at IS NOT NULL, revision FROM run_mask_manifest WHERE run_id=$1`, runID).Scan(&owner, &complete, &fenced, &rev)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		m.gone(runID)
		return false
	case err != nil:
		return false
	case fenced:
		m.Forget(runID)
		return false
	case !complete:
		return false
	}
	m.mu.Lock()
	e := m.cache[runID]
	fresh := e != nil && e.complete && e.rev == rev
	m.mu.Unlock()
	return fresh || m.load(ctx, runID, owner)
}

// load reads runID's manifest and values in one snapshot, opens them, registers
// them and records the revision. False when any step fails or the row is not a
// complete, unfenced manifest.
func (m *Manifests) load(ctx context.Context, runID uuid.UUID, owner string) bool {
	tx, err := m.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return false
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var complete, fenced bool
	var rev int
	if err := tx.QueryRow(ctx, `SELECT complete, fenced_at IS NOT NULL, revision FROM run_mask_manifest WHERE run_id=$1`, runID).Scan(&complete, &fenced, &rev); err != nil {
		return false
	}
	if fenced {
		m.Forget(runID)
		return false
	}
	if !complete {
		return false
	}
	rows, err := tx.Query(ctx, `SELECT ordinal, key_version, sealed FROM run_mask_values WHERE run_id=$1 ORDER BY ordinal`, runID)
	if err != nil {
		return false
	}
	type sealedRow struct {
		ordinal, version int
		sealed           []byte
	}
	stored, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (sealedRow, error) {
		var s sealedRow
		return s, r.Scan(&s.ordinal, &s.version, &s.sealed)
	})
	if err != nil {
		return false
	}
	keys := map[int][]byte{}
	defer func() {
		for _, k := range keys {
			clear(k)
		}
	}()
	plain := make([][]byte, 0, len(stored))
	for _, s := range stored {
		k, ok := keys[s.version]
		if !ok {
			k, err = m.keys.Key(ctx, owner, subjectkey.PurposeCred, s.version)
			if err != nil {
				if errors.Is(err, subjectkey.ErrDataLoss) {
					m.Forget(runID) // the owner's key is destroyed: nothing here can open again
				}
				return false
			}
			keys[s.version] = k
		}
		v, err := kek.Open(k, s.sealed, aad(runID, s.ordinal, s.version))
		if err != nil {
			return false
		}
		plain = append(plain, v)
	}
	e := &entry{rev: rev, complete: true, have: make(map[[sha256.Size]byte]struct{}, len(plain))}
	for _, v := range plain {
		m.reg.AddLocal(runID, v)
		e.have[sha256.Sum256(v)] = struct{}{}
		clear(v)
	}
	m.mu.Lock()
	if cur := m.cache[runID]; cur == nil || cur.rev <= rev {
		m.cache[runID] = e
	}
	m.mu.Unlock()
	return true
}

// Held reports whether this process holds runID's complete manifest, without
// asking Postgres: the label an audit event carries (mask_scope), never a
// gate. A door asks Covered.
func (m *Manifests) Held(runID uuid.UUID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.cache[runID]
	return e != nil && e.complete
}

// gone drops what this process loaded from a manifest that no longer exists. A
// run that never had one keeps the values injection registered: they still
// mask its audit events, though the run stays uncovered.
func (m *Manifests) gone(runID uuid.UUID) {
	m.mu.Lock()
	_, held := m.cache[runID]
	m.mu.Unlock()
	if held {
		m.Forget(runID)
	}
}

// Forget drops this process's copy of runID's manifest and the values the
// registry holds for the run. It touches no row.
func (m *Manifests) Forget(runID uuid.UUID) {
	m.mu.Lock()
	delete(m.cache, runID)
	m.mu.Unlock()
	m.reg.Evict(runID)
}

// DropFenced forgets every manifest this process holds whose row is fenced or
// gone in Postgres, with the values the registry holds for it: how a replica
// that never served the run learns of an erasure issued elsewhere, without
// waiting for a door to ask. Postgres not answering forgets nothing.
func (m *Manifests) DropFenced(ctx context.Context) {
	m.mu.Lock()
	held := make([]uuid.UUID, 0, len(m.cache))
	for id := range m.cache {
		held = append(held, id)
	}
	m.mu.Unlock()
	if len(held) == 0 {
		return
	}
	rows, err := m.pool.Query(ctx, `SELECT run_id FROM run_mask_manifest WHERE run_id = ANY($1) AND fenced_at IS NULL`, held)
	if err != nil {
		return
	}
	live, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return
	}
	for _, id := range held {
		if !slices.Contains(live, id) {
			m.Forget(id)
		}
	}
}

// ForgetCache drops this process's copy of runID's manifest only, leaving the
// registry's values to its own sweep. The next Covered reloads.
func (m *Manifests) ForgetCache(runID uuid.UUID) {
	m.mu.Lock()
	delete(m.cache, runID)
	m.mu.Unlock()
}

// FenceSubject is the erasure primitive: in one transaction it fences every
// manifest owned by owner (the durable row every replica honours, so a door
// refuses and an in-flight consumer ends at its next check) and deletes the
// owner's value rows. It returns the runs it fenced and drops this process's
// copies; other replicas drop theirs when they next see the fence.
func (m *Manifests) FenceSubject(ctx context.Context, owner string) ([]uuid.UUID, error) {
	if owner == "" {
		return nil, ErrNoOwner
	}
	tx, err := m.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("maskmanifest: begin fence: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	rows, err := tx.Query(ctx,
		`UPDATE run_mask_manifest SET fenced_at = COALESCE(fenced_at, now()), revision = revision + 1 WHERE owner=$1 RETURNING run_id`, owner)
	if err != nil {
		return nil, fmt.Errorf("maskmanifest: fence: %w", err)
	}
	runs, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("maskmanifest: fence: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM run_mask_values WHERE owner=$1`, owner); err != nil {
		return nil, fmt.Errorf("maskmanifest: delete the subject's values: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("maskmanifest: commit fence: %w", err)
	}
	for _, id := range runs {
		m.Forget(id)
	}
	slices.SortFunc(runs, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	return runs, nil
}

// Watch returns a check for in-flight consumers: Covered, re-read from
// Postgres at most once per every, the last answer returned in between. A
// consumer calls it per chunk or beat and ends, or drops the chunk, on false.
func (m *Manifests) Watch(runID uuid.UUID, every time.Duration) func() bool {
	var mu sync.Mutex
	var last time.Time
	var ok bool
	var gen uint64
	return func() bool {
		mu.Lock()
		defer mu.Unlock()
		// A moved generation means values were added or dropped (an erasure's
		// tombstones among them) since the answer: read it again at once.
		if !last.IsZero() && time.Since(last) < every && m.reg.Generation() == gen {
			return ok
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		gen = m.reg.Generation()
		ok, last = m.Covered(ctx, runID), time.Now()
		return ok
	}
}
