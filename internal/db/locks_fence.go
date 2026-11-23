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
	"errors"
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
	// fenceHeldWait caps LockHeld's wait for a busy fence connection.
	fenceHeldWait = 3 * fenceStmtWait
)

// fenceStmts is the statements the fence path runs on a connection. None waits for a lock.
type fenceStmts interface {
	try(sctx context.Context, k LockKey) (bool, error)
	unlock(sctx context.Context, k LockKey) (bool, error)
	verify(sctx context.Context, keys []LockKey) error
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

	mu    sync.Mutex // guards conns, dials and errs; never held while waiting for a key or a dial
	conns [LockFenceConns]*lockConn
	dials [LockFenceConns]chan struct{} // closed when the slot's running dial ends; nil when none runs
	errs  [LockFenceConns]error         // the slot's last dial's error
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

// dialFence dials one fence connection with the lock connections' keepalives, all within dctx's deadline.
func (f *fenceSet) dialFence(dctx context.Context) (*lockConn, error) {
	if f.dial != nil {
		return f.dial(dctx)
	}
	conn, err := pgx.ConnectConfig(dctx, f.cfg)
	if err != nil {
		return nil, err
	}
	end := time.Now().Add(fenceStmtWait)
	if d, ok := dctx.Deadline(); ok && d.Before(end) {
		end = d
	}
	sctx, cancel := context.WithDeadline(context.Background(), end)
	defer cancel()
	setLockKeepalives(sctx, func(sctx context.Context, sql string) error { _, err := conn.Exec(sctx, sql); return err })
	if conn.IsClosed() {
		return nil, fmt.Errorf("the connection broke while its keepalives were set")
	}
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
	c, err := f.pick(ctx, deadline, try)
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

// pick returns the next fence connection round robin. A missing, closed or lost one is dialled by dialSlot, one
// dial per slot at a time with f.mu free, and the take waits for that dial until its own deadline (a try, which has
// none, for fenceDialWait) or until ctx ends, so a start never waits past its wait for a connection. A dial outlives
// the take that started it, and its connection serves the next take.
func (f *fenceSet) pick(ctx context.Context, deadline time.Time, try bool) (*lockConn, error) {
	i := (f.next.Add(1) - 1) % LockFenceConns
	end := time.Now().Add(fenceDialWait)
	if !try && deadline.Before(end) {
		end = deadline
	}
	c, done, dead := f.slot(i)
	if dead != nil {
		dead.close()
	}
	if c != nil {
		return c, nil
	}
	timer := time.NewTimer(time.Until(end))
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		return nil, fmt.Errorf("%w: connect: no fence connection within the wait", ErrLockUnavailable)
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: connect: %w", ErrLockUnavailable, ctx.Err())
	}
	f.mu.Lock()
	c, err := f.conns[i], f.errs[i]
	f.mu.Unlock()
	if c == nil || c.lost.Err() != nil || c.isClosed() {
		if err == nil {
			err = fmt.Errorf("the connection was lost as it was dialled")
		}
		return nil, fmt.Errorf("%w: connect: %w", ErrLockUnavailable, err)
	}
	return c, nil
}

// slot returns slot i's live connection, or else the channel its dial closes, starting that dial when none runs.
// dead is a closed or lost connection taken out of the slot, for the caller to close with f.mu free.
func (f *fenceSet) slot(i uint32) (c *lockConn, done <-chan struct{}, dead *lockConn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cur := f.conns[i]; cur != nil {
		if cur.lost.Err() == nil && !cur.isClosed() {
			return cur, nil, nil
		}
		f.conns[i], dead = nil, cur
	}
	if f.dials[i] == nil {
		ch := make(chan struct{})
		f.dials[i] = ch
		go f.dialSlot(i, ch)
	}
	return nil, f.dials[i], dead
}

// dialSlot dials slot i's connection within fenceDialWait, fills the slot and closes done.
func (f *fenceSet) dialSlot(i uint32, done chan struct{}) {
	dctx, cancel := context.WithTimeout(context.Background(), fenceDialWait)
	c, err := f.dialFence(dctx)
	cancel()
	f.mu.Lock()
	f.conns[i], f.errs[i], f.dials[i] = c, err, nil
	f.mu.Unlock()
	close(done)
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

// fenceHeld is LockHeld for a hold of keys on fence connection c, which other holds share. A busy connection is
// waited for, not read as lost at once: every statement on it holds its mutex for fenceStmtWait at most, and a
// connection that has died is lost (c.lost) by the statement that found it so. The wait is capped at fenceHeldWait;
// past it the answer is lost (the caller refuses), and c is left alone. The query then runs under a fenceStmtWait
// deadline of its own. A query that fails has broken the shared connection, so it fails c and every hold on it; a
// key the session no longer holds (a released hold) ends this answer only.
func (c *lockConn) fenceHeld(keys []LockKey) error {
	if !c.lockWithin(fenceHeldWait) {
		if err := context.Cause(c.lost); err != nil {
			return fmt.Errorf("%w: %w", ErrLockLost, err)
		}
		return fmt.Errorf("%w: the start lock connection stayed busy past %s", ErrLockLost, fenceHeldWait)
	}
	if c.closed || c.lost.Err() != nil {
		c.mu.Unlock()
		return fmt.Errorf("%w: the start lock connection was lost", ErrLockLost)
	}
	sctx, cancel := context.WithTimeout(context.Background(), fenceStmtWait)
	err := c.stmts.verify(sctx, keys)
	cancel()
	c.mu.Unlock()
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errKeyNotHeld):
		return fmt.Errorf("%w: %w", ErrLockLost, err)
	}
	c.fail(err)
	return fmt.Errorf("%w: %w", ErrLockLost, err)
}

// lockWithin takes c's mutex within d, giving up early when c is lost, and reports whether it holds it. It queues
// with a plain Lock on a goroutine, so a stream of statements cannot starve it as a TryLock poll can; once it has
// given up, that goroutine unlocks the mutex as soon as it gets it.
func (c *lockConn) lockWithin(d time.Duration) bool {
	got, gaveUp := make(chan struct{}), make(chan struct{})
	go func() {
		c.mu.Lock()
		select {
		case got <- struct{}{}:
		case <-gaveUp:
			c.mu.Unlock()
		}
	}()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-got:
		return true
	case <-t.C:
	case <-c.lost.Done():
	}
	close(gaveUp)
	return false
}

// fail ends every hold on c with ErrLockLost and closes it.
func (c *lockConn) fail(err error) {
	c.lose(fmt.Errorf("%w: %w", ErrLockLost, err))
	c.close()
}
