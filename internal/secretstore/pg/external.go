// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// extVersion is the store-mode row (credential-storage design §2.2, §2.3a): a
// POINTER. The value lives in the external store; kek_id is "<store>:<ref>",
// and wrapped_dek/ciphertext are empty. Wardyn does no cryptography for such
// a row.
//
// The ordering keeps the two systems safe to disagree: Put writes the store
// before the row, Delete removes the value before the row. A failure in
// between leaves at worst a value no row points to (-reconcile reports it)
// or a row whose value is gone, which reads as a definitive refusal, never
// as someone else's value and never as not-found (rules 17, 18).
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
// it, holding the row's lock throughout: two Puts for one row never interleave
// their store writes (with azurekv that could leave no version enabled). It
// answers once the row write is done. If the row cannot be written, a value in
// an object the row never pointed to is removed again rather than orphaned.
// When the row moves to a new object (azurekv's generation rollover), the old
// one is removed after the row points away from it. Both removals run after
// the Put returns (afterPut).
func (s *Store) putExternal(ctx context.Context, name string, value []byte) error {
	done, err := startPut(s.pool)
	if err != nil {
		return fmt.Errorf("pg secretstore: put %s: %w", rowRef(s.owner, name), err)
	}
	defer done()
	ctx, cancel := s.bounded(ctx)
	defer cancel()
	ref := rowRef(s.owner, name)
	tx, err := beginReadCommitted(ctx, s.pool)
	if err != nil {
		return fmt.Errorf("pg secretstore: put %s: begin: %w", ref, err)
	}
	// The rollback runs on ctx, as every call here does. Once ctx has ended it
	// fails at once and pgx closes the connection, which ends the transaction on
	// the server, so it never outlasts the Put's bound.
	defer func() { _ = tx.Rollback(ctx) }()
	// The counter row's share lock comes before the row's advisory lock, the
	// order DeleteEverywhere takes them in: an erase queued for the counter
	// row would otherwise leave this Put holding the advisory lock while it
	// waits behind the erase, which waits for a delete that waits for this Put.
	if gen, fenced := s.generationFence(ctx); fenced {
		if err := s.checkGeneration(ctx, tx, gen); err != nil {
			return err
		}
	}
	if err := lockRow(ctx, tx, s.owner, name); err != nil {
		return fmt.Errorf("pg secretstore: put %s: %w", ref, err)
	}
	if rev, guarded := secretstore.IfRevisionFrom(ctx); guarded {
		if err := s.checkRevision(ctx, tx, name, rev); err != nil {
			return err
		}
	}
	prev, err := s.currentRef(ctx, tx, name)
	if err != nil {
		return err
	}
	loc, err := s.ext.Put(ctx, s.owner, name, prev, value, false)
	if err != nil {
		return fmt.Errorf("pg secretstore: put %s to %s: %w", ref, s.ext.Name(), err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO secrets (owned_by, name, enc_version, kek_id, wrapped_dek, ciphertext, expires_at)
		VALUES ($1, $2, $3, $4, ''::bytea, ''::bytea, $5)
		ON CONFLICT (owned_by, name) DO UPDATE
			SET enc_version=$3, kek_id=$4, wrapped_dek=''::bytea, ciphertext=''::bytea, expires_at=$5, updated_at=now(), value_rev=DEFAULT`,
		s.owner, name, extVersion, s.ext.Name()+":"+loc, expiresAt(ctx),
	)
	commitFailed := false
	if err == nil {
		err = tx.Commit(ctx)
		commitFailed = err != nil
	}
	newObject := secretstore.RefObject(loc) != secretstore.RefObject(prev)
	if err == nil {
		if prev != "" && newObject {
			s.afterPut(ctx, name, func(ctx context.Context) { s.removeOld(ctx, name, prev) })
		}
		return nil
	}
	if !newObject {
		// The object the row already points to now holds the new value, and
		// the row reads it: the write took effect behind a failure.
		return fmt.Errorf("pg secretstore: put %s: the new value is live in %s (the row already points to it), but updating the row failed: %w: %w", ref, s.ext.Name(), secretstore.ErrRowNotWritten, err)
	}
	if commitFailed {
		// A failed COMMIT may still have reached the server (its reply lost, or
		// the bounded context ending mid-commit), so only the row can say
		// whether loc is live, and the Put does not wait to learn it. Each
		// removal rechecks under the row's lock and leaves an object the row
		// names, as the migration's cleanup does: the row names loc if the
		// commit landed, prev if it did not, and neither if a Delete queued on
		// the lock ran first, which leaves both for removal.
		s.afterPut(ctx, name, func(ctx context.Context) {
			s.removeOld(ctx, name, loc)
			if prev != "" {
				s.removeOld(ctx, name, prev)
			}
		})
		return fmt.Errorf("pg secretstore: put %s: the value reached %s, but whether the row did is unknown (%w: %w); it is removed after this call unless the row points to it, and if that removal is skipped, cut or fails, `wardynd -reconcile` lists it", ref, s.ext.Name(), secretstore.ErrRowNotWritten, err)
	}
	// The row's lock ends with this Put, so a later Put may reuse loc's object
	// before the removal runs: removeOld leaves an object the row names.
	s.afterPut(ctx, name, func(ctx context.Context) { s.removeOld(ctx, name, loc) })
	return fmt.Errorf("pg secretstore: put %s: the value reached %s but the row did not, so it is removed again after this call; if that removal is skipped, cut or fails, `wardynd -reconcile` lists it: %w: %w", ref, s.ext.Name(), secretstore.ErrRowNotWritten, err)
}

// cleanUps counts the clean-ups afterPut started in this process that have
// not ended; afterPut caps it. Ending cut ends every one still running
// (WaitForCleanUps, once its ctx ends first); cutMu guards the swap to a
// fresh cut.
var (
	cleanUps    atomic.Int64
	cutMu       sync.Mutex
	cut, cutAll = context.WithCancel(context.Background())
)

// puts counts the Puts that may leave a clean-up (putExternal, putOverPointer)
// from their start until they have handed it to afterPut, and closing holds
// the pools CloseAfterCleanUps has stopped waiting for; putsMu guards both. A
// clean-up starts only inside a counted Put, so a look that sees no Put and no
// clean-up has seen every clean-up a Put can still start.
var (
	putsMu  sync.Mutex
	puts    int64
	closing = map[interface{ Close() }]bool{}
)

// errStoreClosing refuses a Put on a pool that is closing, before it writes
// the secret.
var errStoreClosing = errors.New("the secret store is closing, so the secret was not written")

// startPut counts a Put that may leave a clean-up until the Put calls the
// returned func, which it defers, so the count ends after its afterPut. Once
// CloseAfterCleanUps has stopped waiting for pool, it refuses the Put instead:
// the close would not wait for its clean-up, which would fail on the closed
// pool and leave the value it was removing.
func startPut(pool interface{ Close() }) (func(), error) {
	putsMu.Lock()
	defer putsMu.Unlock()
	if closing[pool] {
		return nil, errStoreClosing
	}
	puts++
	return func() {
		putsMu.Lock()
		puts--
		putsMu.Unlock()
	}, nil
}

// cleanUpCap is how many clean-ups may run at once beside a pool of pool's
// size. Each holds one pooled connection, and the row's lock, for up to
// bounded's six per-call timeouts plus a 2 s release, and its Put no longer
// waits for it, so nothing else slows the next one. A quarter of the pool, and
// at least one: at the default of 10 connections (db.DefaultPoolMaxConns), 2
// run at once and 8 stay for requests, the sweeps and the Puts themselves; at
// 4, 1 runs and 3 stay. 0 (no pool) sets no cap.
func cleanUpCap(pool *pgxpool.Pool) int64 {
	if pool == nil {
		return 0
	}
	return max(1, int64(pool.Stat().MaxConns())/4)
}

// afterPut runs cleanUp once the Put of name that calls it has answered, on a
// context detached from the caller's: the caller's cancellation does not stop
// it, and the locks the caller holds around the Put do not wait for it. It is
// bounded on its own, at bounded's six per-call timeouts, plus the 2 s release
// of a row lock it holds. A cut or a process exit ends it early, and with
// cleanUpLimit clean-ups already running it is skipped, never waited for: in
// each case the value it was removing stays, and `wardynd -reconcile` lists it
// if no row points to it. A panic in it is logged and ends that clean-up only:
// no caller's recovery covers this goroutine, so it would otherwise end the
// process.
func (s *Store) afterPut(ctx context.Context, name string, cleanUp func(context.Context)) {
	if n := cleanUps.Add(1); s.cleanUpLimit > 0 && n > s.cleanUpLimit {
		cleanUps.Add(-1)
		slog.Warn("pg secretstore: too many clean-ups are running, so this Put's is skipped; the value it would remove stays, and `wardynd -reconcile` lists it if no row points to it",
			slog.String("secret", rowRef(s.owner, name)), slog.Int64("limit", s.cleanUpLimit))
		return
	}
	ctx, cancel := s.bounded(context.WithoutCancel(ctx))
	cutMu.Lock()
	stop := context.AfterFunc(cut, cancel)
	cutMu.Unlock()
	go func() {
		defer cleanUps.Add(-1)
		defer stop()
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("pg secretstore: a Put's clean-up panicked; the value it was removing stays, and `wardynd -reconcile` lists it if no row points to it",
					slog.Any("panic", r))
			}
		}()
		cleanUp(ctx)
	}()
}

// WaitForCleanUps returns nil once no Put that may leave a clean-up is running
// (startPut) and no clean-up a Put left is running. If ctx ends first, it cuts
// every clean-up still running and returns ctx's error. Puts may run while it
// waits. wardynd's shutdown calls it beside the wait for its background work;
// tests call it before they read what a clean-up removes.
func WaitForCleanUps(ctx context.Context) error { return waitForCleanUps(ctx, nil) }

// waitForCleanUps is WaitForCleanUps that, given a pool, marks it closing as it
// returns: under putsMu, with its last look at the counts when it returns nil,
// so no Put on pool starts after that look (startPut refuses it).
func waitForCleanUps(ctx context.Context, pool interface{ Close() }) error {
	for {
		putsMu.Lock()
		idle := puts == 0 && cleanUps.Load() == 0
		if idle && pool != nil {
			closing[pool] = true
		}
		putsMu.Unlock()
		if idle {
			return nil
		}
		select {
		case <-ctx.Done():
			if pool != nil {
				putsMu.Lock()
				closing[pool] = true
				putsMu.Unlock()
			}
			cutMu.Lock()
			cutAll()
			cut, cutAll = context.WithCancel(context.Background())
			cutMu.Unlock()
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// CloseAfterCleanUps waits up to bound for the Puts still running and the
// clean-ups Puts left, cutting any clean-up still running then, and closes
// pool. Once it stops waiting, a Put on pool that may leave a clean-up is
// refused before it writes the secret. A command that may exit soon after a
// Put closes its pool this way: pool.Close waits only for a clean-up that
// already holds a connection, and one that has not taken it yet, or one a Put
// still running would start, would fail on the closed pool and leave its value.
func CloseAfterCleanUps(pool interface{ Close() }, bound time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	if err := waitForCleanUps(ctx, pool); err != nil {
		slog.Warn("pg secretstore: closing the pool with a Put or its clean-up still running; a clean-up still running was cut and may have left a value behind (`wardynd -reconcile` lists remaining values that no row uses)",
			slog.Duration("bound", bound))
	}
	pool.Close()
}

// bounded bounds one store-mode write, including the wait for the row's
// lock, at six times the per-call timeout: it holds a pooled connection and
// lock for its whole store conversation, which an outage (retries,
// Retry-After, purge waits) would otherwise stretch to minutes.
func (s *Store) bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	t := s.extTimeout
	if t <= 0 {
		t = 5 * time.Second
	}
	return context.WithTimeout(ctx, 6*t)
}

// lockRow takes the row's store-mode write lock (db.SecretRowLockClass) for
// the rest of tx. owner/name are text with no NUL, so the key bytes are
// unambiguous; a crc32 collision only serialises two rows; two
// DeleteEverywhere calls could deadlock through one, which Postgres detects
// and one retries.
func lockRow(ctx context.Context, tx pgx.Tx, owner, name string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`, db.SecretRowLockClass, rowLockKey(owner, name)); err != nil {
		return fmt.Errorf("lock the row: %w", err)
	}
	return nil
}

// rowLockKey is lockRow's second advisory-lock key for the row.
func rowLockKey(owner, name string) int32 {
	return int32(crc32.ChecksumIEEE([]byte(owner + "\x00" + name)))
}

// removeOld removes the object ref names, after a Put has answered, unless the
// row names it. A failure is logged, not returned: the object is one
// `wardynd -reconcile` lists if no row points to it. It goes through
// removeOldExternal, which deletes under the row's lock and leaves an object
// the row names: a later Put may have reused the object since this Put
// released the lock.
func (s *Store) removeOld(ctx context.Context, name, ref string) {
	if _, _, err := s.removeOldExternal(ctx, s.owner, name, s.ext.Name(), ref); err != nil {
		slog.Warn("pg secretstore: a value a Put left behind was not removed; `wardynd -reconcile` lists it if no row points to it",
			slog.String("secret", rowRef(s.owner, name)), slog.String("store", s.ext.Name()), slog.Any("err", err))
	}
}

// currentRef is the ref this view's own row points to in the configured
// external store, or "" when it has no such row.
func (s *Store) currentRef(ctx context.Context, tx pgx.Tx, name string) (string, error) {
	var kekID string
	err := tx.QueryRow(ctx,
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

// deleteLocked deletes one row inside tx and reports whether there was one.
// It takes the row's store-mode write lock first — the lock putExternal
// holds — so a concurrent Put waits for tx and lands after it, never between
// the value's removal and the row's. A pointer row loses its external value
// before the row; a pointer into an unreachable store is refused, since
// removing only the row would leave the value behind. On any error the
// caller rolls tx back, and the row stays.
func (s *Store) deleteLocked(ctx context.Context, tx pgx.Tx, owner, name string) (bool, error) {
	ref := rowRef(owner, name)
	if err := lockRow(ctx, tx, owner, name); err != nil {
		return false, fmt.Errorf("pg secretstore: delete %s: %w", ref, err)
	}
	var version int16
	var kekID string
	err := tx.QueryRow(ctx,
		`SELECT enc_version, kek_id FROM secrets WHERE owned_by=$1 AND name=$2 FOR UPDATE`, owner, name,
	).Scan(&version, &kekID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("pg secretstore: delete %s: %w", ref, err)
	}
	// A principal-key row holds nothing outside the table: deleting it is a
	// plain row delete, and a crypto-erasure only once the owner's key is
	// destroyed after it (secretstore.EraseOwner).
	if version == pkVersion {
		secretstore.ReportDelete(ctx, secretstore.DeleteReport{PrincipalKey: true})
	}
	if version == extVersion {
		store, loc := splitRef(kekID)
		if !s.reachable(store) {
			return false, fmt.Errorf("pg secretstore: delete %s: it is stored in %q, which this wardynd is not configured to reach; deleting only the row would leave the value behind", ref, store)
		}
		if err := s.ext.Delete(ctx, owner, name, loc); err != nil {
			return false, fmt.Errorf("pg secretstore: delete %s from %s (the row is kept): %w", ref, store, err)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM secrets WHERE owned_by=$1 AND name=$2`, owner, name); err != nil {
		return false, fmt.Errorf("pg secretstore: delete %s: %w", ref, err)
	}
	return true, nil
}

// ReconcileReport is what -reconcile found. No list is ever acted on:
// reconcile reports, an operator decides.
type ReconcileReport struct {
	// Checked is how many pointer rows were checked against the store.
	Checked int
	// Dangling names each pointer row whose value is absent, mis-bound or in a
	// store this wardynd cannot reach, with the reason.
	Dangling []string
	// Orphans are values in the store that no row points to.
	Orphans []secretstore.ExternalEntry
	// SoftDeleted are values the store deleted but can still recover, that no
	// row points to — reported, not drift: nothing reads them, but the
	// organisation can recover them.
	SoftDeleted []secretstore.ExternalEntry
}

// Reconcile lists both sides of store mode and reports where they disagree
// (design §2.3a.1). It reads store metadata only, never a value, and deletes
// nothing. A transient store failure aborts it: a half-checked report would
// read as drift that isn't there.
//
// It may run beside a serving daemon, whose saves and deletes land between its
// reads of the rows and of the store. So each disagreement is read again under
// its row's lock, which every such write holds from its store change to its
// row change: a row whose value is gone is reported only if that row still
// points at a missing value under the lock, and a value no row points to only
// if no row points to it under the lock and the store still lists it after
// every such check. Each recheck runs once; nothing is retried.
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
	var suspects []envelope
	for _, e := range all {
		store, loc := splitRef(e.kekID)
		if s.reachable(store) {
			rep.Checked++
			// A row claims the path its owner and name DERIVE, never the one it
			// records: a forged pointer cannot hide another value from the orphan
			// list. It claims it whether or not the value is live: a soft-deleted
			// value behind a row is dangling, not an orphan as well.
			if want, err := s.ext.Ref(e.ownedBy, e.name, loc); err == nil {
				pointed[want] = true
			}
		}
		why, err := s.dangling(ctx, e)
		if err != nil {
			return rep, err
		}
		if why != "" {
			suspects = append(suspects, e)
		}
	}
	found, err := s.ext.Walk(ctx)
	if err != nil {
		return rep, fmt.Errorf("pg secretstore: reconcile list %s: %w", s.ext.Name(), err)
	}
	var strays []secretstore.ExternalEntry
	for _, f := range found {
		switch {
		case pointed[secretstore.RefObject(f.Ref)]:
		case f.SoftDeleted:
			rep.SoftDeleted = append(rep.SoftDeleted, f)
		default:
			strays = append(strays, f)
		}
	}
	for _, e := range suspects {
		why, err := s.recheckDangling(ctx, e.ownedBy, e.name)
		if err != nil {
			return rep, err
		}
		if why != "" {
			rep.Dangling = append(rep.Dangling, why)
		}
	}
	var unnamed []secretstore.ExternalEntry
	for _, f := range strays {
		named, err := s.namedUnderLock(ctx, f)
		if err != nil {
			return rep, err
		}
		if !named {
			unnamed = append(unnamed, f)
		}
	}
	if len(unnamed) == 0 {
		return rep, nil
	}
	// A value a failed save wrote, or a replaced one, is removed again by its
	// writer: only a value the store still lists is reported.
	again, err := s.ext.Walk(ctx)
	if err != nil {
		return rep, fmt.Errorf("pg secretstore: reconcile list %s: %w", s.ext.Name(), err)
	}
	listed := map[string]bool{}
	for _, f := range again {
		if !f.SoftDeleted {
			listed[secretstore.RefObject(f.Ref)] = true
		}
	}
	for _, f := range unnamed {
		if listed[secretstore.RefObject(f.Ref)] {
			rep.Orphans = append(rep.Orphans, f)
		}
	}
	return rep, nil
}

// dangling says why the pointer row e has no usable value, or "" when it has
// one. A store that does not answer aborts the reconcile.
func (s *Store) dangling(ctx context.Context, e envelope) (string, error) {
	ref := rowRef(e.ownedBy, e.name)
	store, loc := splitRef(e.kekID)
	if !s.reachable(store) {
		return fmt.Sprintf("%s: stored in %q, which this wardynd is not configured to reach", ref, store), nil
	}
	err := s.ext.Check(ctx, e.ownedBy, e.name, loc)
	switch {
	case errors.Is(err, secretstore.ErrUnavailable):
		return "", fmt.Errorf("pg secretstore: reconcile aborted at %s: %w", ref, err)
	case err != nil:
		return fmt.Sprintf("%s: %v", ref, err), nil
	}
	return "", nil
}

// recheckDangling reads the row (owner, name) again under its lock, so no save
// or delete of it is part way, and says why it is dangling now, or "" when it
// is gone, no longer a pointer, or its value is there.
func (s *Store) recheckDangling(ctx context.Context, owner, name string) (string, error) {
	ctx, cancel := s.bounded(ctx)
	defer cancel()
	conn, release, err := s.lockExternalRow(ctx, owner, name)
	if err != nil {
		return "", fmt.Errorf("pg secretstore: reconcile recheck %s: %w", rowRef(owner, name), err)
	}
	defer release()
	e := envelope{ownedBy: owner, name: name}
	err = conn.QueryRow(ctx, `SELECT kek_id FROM secrets WHERE owned_by=$1 AND name=$2 AND enc_version=$3`, owner, name, extVersion).Scan(&e.kekID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("pg secretstore: reconcile recheck %s: %w", rowRef(owner, name), err)
	}
	return s.dangling(ctx, e)
}

// namedUnderLock reports whether the row the listed value f names, read under
// that row's lock, now points to f: a save that wrote f has then finished.
func (s *Store) namedUnderLock(ctx context.Context, f secretstore.ExternalEntry) (bool, error) {
	ctx, cancel := s.bounded(ctx)
	defer cancel()
	conn, release, err := s.lockExternalRow(ctx, f.Owner, f.Name)
	if err != nil {
		return false, fmt.Errorf("pg secretstore: reconcile recheck %s: %w", rowRef(f.Owner, f.Name), err)
	}
	defer release()
	var kekID string
	err = conn.QueryRow(ctx, `SELECT kek_id FROM secrets WHERE owned_by=$1 AND name=$2 AND enc_version=$3`, f.Owner, f.Name, extVersion).Scan(&kekID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("pg secretstore: reconcile recheck %s: %w", rowRef(f.Owner, f.Name), err)
	}
	store, loc := splitRef(kekID)
	if !s.reachable(store) {
		return false, nil
	}
	want, err := s.ext.Ref(f.Owner, f.Name, loc)
	return err == nil && secretstore.RefObject(want) == secretstore.RefObject(f.Ref), nil
}

// UnderivedPointers counts the pointer rows into the configured external store
// whose recorded ref that store no longer derives from the row, and returns one
// such ref: rows written under another prefix, mount or address, every read of
// which is refused as a moved pointer. 0 with no external store.
func (s *Store) UnderivedPointers(ctx context.Context) (n int, example string, err error) {
	if s.ext == nil {
		return 0, "", nil
	}
	rows, err := s.pool.Query(ctx, `SELECT owned_by, name, kek_id FROM secrets WHERE enc_version=$1`, extVersion)
	if err != nil {
		return 0, "", fmt.Errorf("pg secretstore: read the pointer rows: %w", err)
	}
	all, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ Owner, Name, KekID string }])
	if err != nil {
		return 0, "", fmt.Errorf("pg secretstore: read the pointer rows: %w", err)
	}
	for _, r := range all {
		store, ref := splitRef(r.KekID)
		if store != s.ext.Name() {
			continue
		}
		if got, err := s.ext.Ref(r.Owner, r.Name, ref); err != nil || secretstore.RefObject(got) != secretstore.RefObject(ref) {
			if n == 0 {
				example = ref
			}
			n++
		}
	}
	return n, example, nil
}
