// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// The fence path for a run's start locks (RunStartLockClass). A start lock taken through Lock or TryLock with no hold
// on its context dials no connection and takes no LockPoolMaxConns slot: every such hold shares LockFenceConns
// dedicated connections per locker, so many starts on distinct runs never refuse each other for capacity.
//
//   - In process, a key is held by one holder at a time (the gate): a session's advisory lock is re-entrant, so two
//     holders on one connection would otherwise both "hold" it.
//   - In Postgres the hold is the same (int4 class, int4 obj) pair the dialled path takes, so a fence hold, a writer's
//     queued hold (LockQueued) and an older replica's start exclude each other.
//   - A take only ever tries, and polls with a backoff while the key is held elsewhere: no statement on a shared
//     connection waits for a lock, so one contended key never stalls another's take.
//   - No statement on a fence connection carries a caller's context. pgx answers a cancelled statement by breaking
//     its connection, which on a shared connection would end every other hold on it; each statement gets a deadline
//     of its own (fenceStmtWait), and the caller's context is honoured only between statements.
//   - A statement that fails, an unlock the database refuses, or a session the watcher finds gone fails the
//     connection: every hold on it ends with ErrLockLost, and the next take dials a new one.

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

// LockFenceConns is how many connections one locker keeps for run start locks.
const LockFenceConns = 2

const (
	// fenceStmtWait bounds one statement on a fence connection. A statement past it has broken its connection.
	fenceStmtWait = 2 * time.Second
	// fenceDialWait bounds the dial of a fence connection.
	fenceDialWait = 10 * time.Second
	// The backoff between tries of a key held elsewhere: the first wait, doubling up to the last.
	fenceBackoffFirst = 25 * time.Millisecond
	fenceBackoffMax   = 200 * time.Millisecond
	// fenceDefaultWait is a Lock's wait when it names none, as for the dialled path.
	fenceDefaultWait = 10 * time.Second
)

// fenceStmts is the statements the fence path runs on a connection. Neither waits for a lock.
type fenceStmts interface {
	try(sctx context.Context, k LockKey) (bool, error)
	unlock(sctx context.Context, k LockKey) (bool, error)
}

func (c *lockConn) try(sctx context.Context, k LockKey) (bool, error) {
	var got bool
	err := c.conn.QueryRow(sctx, `SELECT pg_try_advisory_lock($1, $2)`, k.Class, k.Obj).Scan(&got)
	return got, err
}

func (c *lockConn) unlock(sctx context.Context, k LockKey) (bool, error) {
	var freed bool
	err := c.conn.QueryRow(sctx, `SELECT pg_advisory_unlock($1, $2)`, k.Class, k.Obj).Scan(&freed)
	return freed, err
}

// fenceSet is one locker's fence connections and its in-process gate.
type fenceSet struct {
	cfg *pgx.ConnConfig
	// dial makes a fence connection; nil dials cfg. Tests set a fake.
	dial func(ctx context.Context) (*lockConn, error)

	mu    sync.Mutex // guards conns; never held while waiting for a key
	conns [LockFenceConns]*lockConn
	next  atomic.Uint32

	gateMu sync.Mutex
	gate   map[LockKey]*gateEntry
}

// gateEntry is one key's turn in the gate: ch holds the holder's token, and refs counts the takes waiting for it or
// holding it, so the entry is dropped when the last one leaves.
type gateEntry struct {
	ch   chan struct{}
	refs int
}

// newFenceConn wraps conn, whose statements are stmts, as a fence connection: once it is lost it is closed.
func newFenceConn(conn *pgx.Conn, stmts fenceStmts) *lockConn {
	c := &lockConn{conn: conn, fence: true, stmts: stmts}
	c.lost, c.lose = context.WithCancelCause(context.Background())
	context.AfterFunc(c.lost, c.close)
	return c
}

// dialFence dials one fence connection with the lock connections' keepalives.
func (f *fenceSet) dialFence(dctx context.Context) (*lockConn, error) {
	if f.dial != nil {
		return f.dial(dctx)
	}
	conn, err := pgx.ConnectConfig(dctx, f.cfg)
	if err != nil {
		return nil, err
	}
	sctx, cancel := context.WithTimeout(context.Background(), fenceStmtWait)
	defer cancel()
	setLockKeepalives(sctx, func(sctx context.Context, sql string) error { _, err := conn.Exec(sctx, sql); return err })
	c := newFenceConn(conn, nil)
	c.stmts = c
	return c, nil
}

// take is a top-level Lock (try false) or TryLock (try true) of a start key: the gate, a connection round robin,
// then the try and poll. The returned context ends with ErrLockLost when the connection is lost, and at release.
func (f *fenceSet) take(ctx context.Context, k LockKey, wait time.Duration, try bool) (context.Context, func(), bool, error) {
	if !try && wait <= 0 {
		wait = fenceDefaultWait
	}
	deadline := time.Now().Add(wait)
	e, ok, err := f.enter(ctx, k, deadline, try)
	if err != nil || !ok {
		return ctx, nil, false, err
	}
	c, err := f.pick(ctx)
	if err != nil {
		f.leave(k, e, true)
		return ctx, nil, false, err
	}
	if got, err := f.poll(ctx, c, k, deadline, try); err != nil || !got {
		f.leave(k, e, true)
		return ctx, nil, false, err
	}
	c.watchOnce.Do(func() {
		if c.conn != nil {
			go c.watch(c.lose, nil, LockWatchInterval)
		}
	})
	hctx, hcancel := context.WithCancelCause(withHolds(ctx, &holds{keys: []LockKey{k}, conn: c}))
	stopLost := context.AfterFunc(c.lost, func() { hcancel(context.Cause(c.lost)) })
	var once sync.Once
	release := func() {
		once.Do(func() {
			stopLost()
			f.unlockKey(c, k)
			f.leave(k, e, true)
			hcancel(context.Canceled)
		})
	}
	return hctx, release, true, nil
}

// nest takes a further start key k on the fence connection ctx's hold h is on: the gate, then the try and poll on
// that connection. The hold ends with h's.
func (f *fenceSet) nest(ctx context.Context, h *holds, k LockKey, wait time.Duration, try bool) (context.Context, func(), bool, error) {
	if !try && wait <= 0 {
		wait = fenceDefaultWait
	}
	deadline := time.Now().Add(wait)
	e, ok, err := f.enter(ctx, k, deadline, try)
	if err != nil || !ok {
		return ctx, nil, false, err
	}
	c := h.conn
	if got, err := f.poll(ctx, c, k, deadline, try); err != nil || !got {
		f.leave(k, e, true)
		return ctx, nil, false, err
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			f.unlockKey(c, k)
			f.leave(k, e, true)
		})
	}
	return withHolds(ctx, &holds{keys: append(slices.Clone(h.keys), k), conn: c}), release, true, nil
}

// enter waits for k's turn in the gate until deadline (not at all for try), honouring ctx. ok is false, with a nil
// error, for a try that found k held in this process.
func (f *fenceSet) enter(ctx context.Context, k LockKey, deadline time.Time, try bool) (_ *gateEntry, ok bool, _ error) {
	f.gateMu.Lock()
	if f.gate == nil {
		f.gate = map[LockKey]*gateEntry{}
	}
	e := f.gate[k]
	if e == nil {
		e = &gateEntry{ch: make(chan struct{}, 1)}
		f.gate[k] = e
	}
	e.refs++
	f.gateMu.Unlock()
	if try {
		select {
		case e.ch <- struct{}{}:
			return e, true, nil
		default:
			f.leave(k, e, false)
			return nil, false, nil
		}
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case e.ch <- struct{}{}:
		return e, true, nil
	case <-timer.C:
		f.leave(k, e, false)
		return nil, false, fmt.Errorf("%w: %v", ErrLockBusy, k)
	case <-ctx.Done():
		f.leave(k, e, false)
		return nil, false, fmt.Errorf("%w: waiting for %v: %w", ErrLockBusy, k, ctx.Err())
	}
}

// leave gives k's turn back (when held) and drops the entry once no take waits for it or holds it.
func (f *fenceSet) leave(k LockKey, e *gateEntry, held bool) {
	if held {
		<-e.ch
	}
	f.gateMu.Lock()
	defer f.gateMu.Unlock()
	if e.refs--; e.refs == 0 {
		delete(f.gate, k)
	}
}

// pick returns the next fence connection round robin, dialling it when it is missing, closed or lost.
func (f *fenceSet) pick(ctx context.Context) (*lockConn, error) {
	i := (f.next.Add(1) - 1) % LockFenceConns
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.conns[i]; c != nil {
		if c.lost.Err() == nil && !c.isClosed() {
			return c, nil
		}
		c.close()
	}
	dctx, cancel := context.WithTimeout(ctx, fenceDialWait)
	defer cancel()
	c, err := f.dialFence(dctx)
	if err != nil {
		return nil, fmt.Errorf("%w: connect: %w", ErrLockUnavailable, err)
	}
	f.conns[i] = c
	return c, nil
}

// poll tries k on c until it is taken or deadline passes, sleeping the backoff between tries with c's mutex free,
// so a contended key holds up another take, a release or the watcher on c for one statement at most.
func (f *fenceSet) poll(ctx context.Context, c *lockConn, k LockKey, deadline time.Time, try bool) (bool, error) {
	backoff := fenceBackoffFirst
	for {
		got, err := c.fenceTry(k)
		if err != nil || got || try {
			return got, err
		}
		left := time.Until(deadline)
		if left <= 0 {
			return false, fmt.Errorf("%w: %v", ErrLockBusy, k)
		}
		t := time.NewTimer(min(backoff, left))
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return false, fmt.Errorf("%w: waiting for %v: %w", ErrLockBusy, k, ctx.Err())
		}
		backoff = nextFenceBackoff(backoff)
	}
}

// nextFenceBackoff is the wait after d: double, up to fenceBackoffMax.
func nextFenceBackoff(d time.Duration) time.Duration { return min(2*d, fenceBackoffMax) }

// fenceTry runs one try of k on c, holding c's mutex for that statement only; a taken key joins c's keys in the
// same hold. A statement that fails has broken the connection, so it fails c.
func (c *lockConn) fenceTry(k LockKey) (bool, error) {
	c.mu.Lock()
	if c.closed || c.lost.Err() != nil {
		c.mu.Unlock()
		return false, fmt.Errorf("%w: the start lock connection was lost", ErrLockUnavailable)
	}
	sctx, cancel := context.WithTimeout(context.Background(), fenceStmtWait)
	got, err := c.stmts.try(sctx, k)
	cancel()
	if err == nil && got {
		c.keys = append(c.keys, k)
	}
	c.mu.Unlock()
	if err != nil {
		c.fail(err)
		return false, fmt.Errorf("%w: %w", ErrLockUnavailable, err)
	}
	return got, nil
}

// unlockKey releases k on c under a statement deadline of its own. A false or an error fails c: every hold on it
// ends with ErrLockLost.
func (f *fenceSet) unlockKey(c *lockConn, k LockKey) {
	c.mu.Lock()
	if c.closed || c.lost.Err() != nil {
		c.mu.Unlock()
		return
	}
	sctx, cancel := context.WithTimeout(context.Background(), fenceStmtWait)
	freed, err := c.stmts.unlock(sctx, k)
	cancel()
	if i := slices.Index(c.keys, k); i >= 0 {
		c.keys = slices.Delete(c.keys, i, i+1)
	}
	c.mu.Unlock()
	if err == nil && !freed {
		err = fmt.Errorf("the session no longer held %v", k)
	}
	if err != nil {
		c.fail(err)
	}
}

// fail ends every hold on c with ErrLockLost and closes it.
func (c *lockConn) fail(err error) {
	c.lose(fmt.Errorf("%w: %w", ErrLockLost, err))
	c.close()
}
