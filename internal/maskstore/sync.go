// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package maskstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
)

const (
	// minGap is the least time between two reads of the table by one replica:
	// a burst of chunks shares reads.
	minGap = 50 * time.Millisecond
	// idleRead is how often a replica with no traffic reads the table anyway,
	// so tombstones and erasures reach it without a notification.
	idleRead = 5 * time.Second
	// listenRetry is the wait before the listener reconnects.
	listenRetry = 2 * time.Second
)

// ref is what this process knows about one committed row: enough to apply a
// later change to it, or to drop it, without opening it again.
type ref struct {
	id        uuid.UUID
	gen       int64
	bucket    string
	runID     uuid.UUID
	owner     string
	name      string
	value     []byte
	until     time.Time
	retiredAt time.Time
	current   bool
}

// syncer is the read side: a cursor over the generations, the refs of every
// row applied, and the coordination that coalesces reads.
type syncer struct {
	mu     sync.Mutex
	cursor int64
	loaded bool
	rows   map[uuid.UUID]*ref

	running   bool
	done      chan struct{}
	lastStart time.Time
	okStart   time.Time

	kick chan struct{}
	// idle, when set, runs after each background read.
	idle func(context.Context)

	// listening is true while the LISTEN connection is up.
	listening atomic.Bool
}

// note records a row this process just committed, so a later tombstone for it
// can drop the value it holds without opening anything. A newer generation of
// a row this process already knows keeps its value.
func (s *Store) note(r *ref) {
	s.sync.mu.Lock()
	defer s.sync.mu.Unlock()
	if old := s.sync.rows[r.id]; old != nil && old.gen >= r.gen {
		return
	}
	s.sync.rows[r.id] = r
}

// Fresh returns nil once this process has read the whole table in a read that
// began after arrived, so every value committed before the bytes arrived is in
// the registry. Reads are shared and at most one per minGap; a read that is
// already running when the bytes arrived does not count, because it may have
// started before a commit the bytes depend on. It fails when Postgres does not
// answer: the caller then cannot vouch for its masking.
func (s *Store) Fresh(ctx context.Context, arrived time.Time) error {
	sy := &s.sync
	for {
		sy.mu.Lock()
		if !sy.okStart.Before(arrived) && !sy.okStart.IsZero() {
			sy.mu.Unlock()
			return nil
		}
		if sy.running {
			done := sy.done
			sy.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if wait := minGap - time.Since(sy.lastStart); wait > 0 {
			sy.mu.Unlock()
			select {
			case <-time.After(wait):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		sy.running = true
		sy.done = make(chan struct{})
		done := sy.done
		start := time.Now()
		sy.lastStart = start
		sy.mu.Unlock()

		err := s.read(ctx)

		sy.mu.Lock()
		sy.running = false
		if err == nil {
			sy.okStart = start
		}
		close(done)
		sy.mu.Unlock()
		if err != nil {
			return err
		}
	}
}

// Synced returns nil while this replica can vouch for its copy of the shared
// corpus: its LISTEN connection is up, and one read of the table that began
// after the generation was read has brought its cursor to that generation. A
// commit announced a moment ago does not fail it, because that read applies it;
// a replica that cannot reach Postgres, or cannot apply what it finds, does.
// It is the /setup/status mask_registry_shared check, not a door: the doors use
// Fresh.
func (s *Store) Synced(ctx context.Context) error {
	if !s.sync.listening.Load() {
		return errors.New("the connection that listens for secret-masking changes is down")
	}
	var gen int64
	if err := s.pool.QueryRow(ctx, `SELECT gen FROM mask_gen`).Scan(&gen); err != nil {
		return fmt.Errorf("read the masking generation: %w", err)
	}
	if err := s.Fresh(ctx, time.Now()); err != nil {
		return err
	}
	s.sync.mu.Lock()
	cursor := s.sync.cursor
	s.sync.mu.Unlock()
	if cursor < gen {
		return fmt.Errorf("this replica is at masking generation %d, behind %d", cursor, gen)
	}
	return nil
}

// row is one mask_values row as a read returns it.
type row struct {
	id        uuid.UUID
	bucket    string
	owner     string
	runID     *uuid.UUID
	name      string
	version   *int
	sealed    []byte
	until     *time.Time
	retiredAt *time.Time
	tombstone bool
	gen       int64
}

// read is one read of the table above the cursor, applied to the registry. One
// statement is one snapshot, and mask_gen.gen in it bounds the committed prefix
// the rows belong to, so the cursor can move to it even when no row came back.
func (s *Store) read(ctx context.Context) error {
	sy := &s.sync
	sy.mu.Lock()
	cursor, loaded := sy.cursor, sy.loaded
	sy.mu.Unlock()
	if !loaded {
		cursor = 0
	}
	top, pruned, rows, err := s.fetch(ctx, cursor)
	if err != nil {
		return err
	}
	full := !loaded
	if pruned > cursor && loaded {
		// The tombstones this replica has not seen were deleted: reload it all.
		if top, _, rows, err = s.fetch(ctx, 0); err != nil {
			return err
		}
		full = true
	}
	if err := s.apply(ctx, rows, top, full); err != nil {
		return err
	}
	sy.mu.Lock()
	sy.cursor, sy.loaded = top, true
	sy.mu.Unlock()
	return nil
}

// fetch reads the generation, the prune horizon and every row above cursor.
func (s *Store) fetch(ctx context.Context, cursor int64) (top, pruned int64, rows []row, err error) {
	rs, err := s.pool.Query(ctx,
		`SELECT g.gen, g.pruned, v.id, v.bucket, v.owner, v.run_id, v.name, v.key_version, v.sealed, v.until, v.retired_at, v.tombstone, v.gen
		 FROM mask_gen g LEFT JOIN mask_values v ON v.gen > $1 ORDER BY v.gen`, cursor)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("maskstore: read the corpus: %w", err)
	}
	defer rs.Close()
	for rs.Next() {
		var id *uuid.UUID
		var bucket, owner, name *string
		var tomb *bool
		var vgen *int64
		var r row
		if err := rs.Scan(&top, &pruned, &id, &bucket, &owner, &r.runID, &name, &r.version, &r.sealed, &r.until, &r.retiredAt, &tomb, &vgen); err != nil {
			return 0, 0, nil, fmt.Errorf("maskstore: read the corpus: %w", err)
		}
		if id == nil {
			continue // the generation row with no value above the cursor
		}
		r.id, r.bucket, r.owner, r.name, r.tombstone, r.gen = *id, *bucket, *owner, *name, *tomb, *vgen
		rows = append(rows, r)
	}
	if err := rs.Err(); err != nil {
		return 0, 0, nil, fmt.Errorf("maskstore: read the corpus: %w", err)
	}
	return top, pruned, rows, nil
}

// apply puts rows into the registry. A row this process already knows is
// updated from the ref; only an unknown row is opened, under its owner's key,
// and a key that is destroyed or does not unwrap, or a blob that does not open,
// is skipped (nothing can ever mask it) once the runs it masks are fenced
// (unopenable). A transient failure (the store is unavailable, the context
// ends) aborts the read, so the cursor stays and the caller fails closed. A full
// read also drops what the table no longer has, but only refs at or below top,
// the generation its snapshot covers: a ref above it was committed here after
// the snapshot, the next read applies it, and dropping it would unmask its value.
func (s *Store) apply(ctx context.Context, rows []row, top int64, full bool) error {
	keys := map[keyID][]byte{}
	defer func() {
		for _, k := range keys {
			clear(k)
		}
	}()
	seen := make(map[uuid.UUID]bool, len(rows))
	for _, r := range rows {
		if !r.tombstone {
			seen[r.id] = true
		}
		if err := s.applyRow(ctx, r, keys); err != nil {
			return err
		}
	}
	if !full {
		return nil
	}
	s.sync.mu.Lock()
	var gone []*ref
	for id, rf := range s.sync.rows {
		if !seen[id] && rf.gen <= top {
			gone = append(gone, rf)
			delete(s.sync.rows, id)
		}
	}
	s.sync.mu.Unlock()
	for _, rf := range gone {
		s.drop(rf)
	}
	return nil
}

type keyID struct {
	owner   string
	version int
}

func (s *Store) applyRow(ctx context.Context, r row, keys map[keyID][]byte) error {
	s.sync.mu.Lock()
	rf := s.sync.rows[r.id]
	s.sync.mu.Unlock()
	if r.tombstone {
		if rf == nil {
			return nil
		}
		s.sync.mu.Lock()
		delete(s.sync.rows, r.id)
		s.sync.mu.Unlock()
		if r.retiredAt != nil && rf.current && rf.bucket == bucketGlobal {
			s.reg.RetireGlobalValue(rf.owner, rf.name, rf.value, *r.retiredAt)
			return nil
		}
		s.drop(rf)
		return nil
	}
	if rf == nil {
		opened, err := s.open(ctx, r, keys)
		if err != nil || opened == nil {
			return err
		}
		rf = opened
	}
	rf.gen = r.gen
	rf.until, rf.retiredAt, rf.current = zeroIfNil(r.until), zeroIfNil(r.retiredAt), r.retiredAt == nil
	s.sync.mu.Lock()
	s.sync.rows[r.id] = rf
	s.sync.mu.Unlock()
	if rf.bucket == bucketRun {
		s.reg.AddLocal(rf.runID, rf.value)
		return nil
	}
	s.reg.ApplyGlobal(rf.owner, rf.name, rf.value, rf.until, rf.retiredAt)
	return nil
}

func zeroIfNil(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// open decrypts an unknown live row. nil, nil means it can never be opened and
// is skipped, once unopenable has fenced what it masked.
func (s *Store) open(ctx context.Context, r row, keys map[keyID][]byte) (*ref, error) {
	if r.version == nil || r.sealed == nil {
		return nil, nil
	}
	scope := r.name
	rf := &ref{id: r.id, bucket: r.bucket, owner: r.owner, name: r.name}
	if r.bucket == bucketRun {
		if r.runID == nil {
			return nil, nil
		}
		scope, rf.runID = r.runID.String(), *r.runID
	}
	id := keyID{r.owner, *r.version}
	key, ok := keys[id]
	if !ok {
		var err error
		key, err = s.keys.Key(ctx, r.owner, subjectkey.PurposeCred, *r.version)
		// The fence is irreversible, so it needs proof about the row: a destroyed generation, or a
		// wrap that does not open under its own key (kek.ErrCorrupt), which never heals by
		// retrying. Every other failure (unreachable, access refused, a key or version the
		// service does not hold or has retired, any answer nothing classifies) aborts the read:
		// consumers fail closed and it heals once the service or configuration does.
		switch {
		case errors.Is(err, subjectkey.ErrDataLoss):
			return nil, s.unopenable(ctx, r, "the owner's key is destroyed")
		case errors.Is(err, kek.ErrCorrupt):
			slog.ErrorContext(ctx, "maskstore: the owner's key does not open",
				slog.String("owner", r.owner), slog.Int("key_version", *r.version), slog.Any("err", err))
			return nil, s.unopenable(ctx, r, "the owner's key does not open")
		case err != nil:
			return nil, fmt.Errorf("maskstore: the owner's key: %w", err)
		}
		keys[id] = key
	}
	v, err := kek.Open(key, r.sealed, aad(r.bucket, r.id, r.owner, scope, *r.version))
	if err != nil {
		return nil, s.unopenable(ctx, r, "the value does not open under its key")
	}
	rf.value = v
	return rf, nil
}

// unopenable handles a row that can never be opened again. A retired value is
// skipped. A live one is missing from every replica's corpus, so the runs it
// masks fail closed: in one commit their manifests are fenced, as FenceSubject
// fences them (a per-run value fences its run, a credential value every run of
// its owner), and the row is tombstoned, so a later read does not fence the
// owner's later runs for it again. An error aborts the read and the cursor stays.
//
// r is this read's snapshot, which can be older than the key failure it found:
// a credentials erase may have retired the row, destroyed the key and let the
// owner start runs under the next key generation since. The fence is
// irreversible, so the commit first locks the row and fences only if it is
// still live at the generation this read saw; otherwise it changes nothing, and
// the read that reaches the row's newer generation decides it on that state.
func (s *Store) unopenable(ctx context.Context, r row, why string) error {
	if r.retiredAt != nil {
		slog.WarnContext(ctx, "maskstore: a retired masking value does not open; skipped", slog.String("id", r.id.String()), slog.String("why", why))
		return nil
	}
	where, arg := "owner = $1", any(r.owner)
	if r.bucket == bucketRun {
		where, arg = "run_id = $1", *r.runID
	}
	var fenced []uuid.UUID
	_, err := s.commit(ctx, func(tx pgx.Tx, gen int64) error {
		var live bool
		err := tx.QueryRow(ctx, `SELECT NOT tombstone AND retired_at IS NULL FROM mask_values WHERE id = $1 AND gen = $2 FOR UPDATE`, r.id, r.gen).Scan(&live)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return errRowMoved
		case err != nil:
			return fmt.Errorf("maskstore: lock an unopenable value: %w", err)
		case !live:
			return errRowMoved
		}
		rows, err := tx.Query(ctx,
			`UPDATE run_mask_manifest SET fenced_at = now(), revision = revision + 1 WHERE `+where+` AND fenced_at IS NULL RETURNING run_id`, arg)
		if err != nil {
			return fmt.Errorf("maskstore: fence the runs of an unopenable value: %w", err)
		}
		if fenced, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID]); err != nil {
			return fmt.Errorf("maskstore: fence the runs of an unopenable value: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM run_mask_values WHERE run_id = ANY($1)`, fenced); err != nil {
			return fmt.Errorf("maskstore: delete the fenced runs' manifest values: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE mask_values SET `+tombstoneSet+`, retired_at = NULL WHERE id = $2 AND NOT tombstone`, gen, r.id); err != nil {
			return fmt.Errorf("maskstore: tombstone an unopenable value: %w", err)
		}
		return nil
	})
	if errors.Is(err, errRowMoved) {
		slog.WarnContext(ctx, "maskstore: a masking value that does not open changed since this read; nothing fenced",
			slog.String("id", r.id.String()), slog.String("why", why))
		return nil
	}
	if err != nil {
		return err
	}
	slog.ErrorContext(ctx, "maskstore: a live masking value does not open; the runs it masks are fenced",
		slog.String("id", r.id.String()), slog.String("why", why), slog.Int("runs_fenced", len(fenced)))
	return nil
}

// errRowMoved rolls back unopenable's commit: the row is no longer the live
// generation the read found.
var errRowMoved = errors.New("maskstore: the value changed since it was read")

// drop removes the value a ref holds from the registry and clears it.
func (s *Store) drop(rf *ref) {
	if rf.bucket == bucketRun {
		s.reg.DropRunValue(rf.runID, rf.value)
	} else {
		s.reg.DropGlobalValue(rf.owner, rf.name, rf.value)
	}
	clear(rf.value)
}

// OnBackgroundRead sets f to run after every background read (not after the
// reads a consumer's Fresh makes): the hook for whatever else a replica should
// notice about other replicas' commits at that cadence. Call it before Start.
func (s *Store) OnBackgroundRead(f func(context.Context)) { s.sync.idle = f }

// Start keeps this replica fresh until ctx ends: it reads when a commit is
// announced, when the listener reconnects, and every idleRead regardless, so a
// replica converges with no notification delivered. It returns at once.
func (s *Store) Start(ctx context.Context) {
	s.sync.kick = make(chan struct{}, 1)
	go s.listen(ctx)
	go func() {
		t := time.NewTicker(idleRead)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			case <-s.sync.kick:
			}
			rctx, cancel := context.WithTimeout(ctx, freshRead)
			if err := s.Fresh(rctx, time.Now()); err != nil && ctx.Err() == nil {
				slog.WarnContext(ctx, "maskstore: the background read failed", slog.Any("err", err))
			} else if s.sync.idle != nil {
				s.sync.idle(rctx)
			}
			cancel()
		}
	}()
}

// freshRead bounds one background read.
const freshRead = 10 * time.Second

// listen holds one connection LISTENing on Channel, apart from the pool, and
// kicks a read on each notification and each reconnect. Losing it loses only
// promptness.
func (s *Store) listen(ctx context.Context) {
	for ctx.Err() == nil {
		s.listenOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(listenRetry):
		}
	}
}

func (s *Store) listenOnce(ctx context.Context) {
	conn, err := pgx.ConnectConfig(ctx, s.pool.Config().ConnConfig.Copy())
	if err != nil {
		return
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	if _, err := conn.Exec(ctx, "LISTEN "+Channel); err != nil {
		return
	}
	s.sync.listening.Store(true)
	defer s.sync.listening.Store(false)
	s.poke() // a read now: whatever was announced while this was down
	for {
		if _, err := conn.WaitForNotification(ctx); err != nil {
			return
		}
		s.poke()
	}
}

// poke asks the background reader for a read, without waiting for it.
func (s *Store) poke() {
	select {
	case s.sync.kick <- struct{}{}:
	default:
	}
}
