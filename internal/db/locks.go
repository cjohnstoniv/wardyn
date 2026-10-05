// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// Cross-replica locks for work that must not run twice: redeeming a rotating
// refresh token, a revive of one run, a read-modify-write of one document.
//
// Unlike AdvisoryLockKeyed (kept for the sign-in supersede lock, whose
// documented fail-open behaviour is unchanged), these locks
//
//   - never proceed unlocked: no connection to hold the lock on, or a database
//     that cannot be asked, is a refusal (the caller answers 503 and the work
//     is retried), never a pass-through;
//   - are ordered: a hold taken while another is held must rank after it in
//     LockOrder, or the call fails with ErrLockOrder, so two replicas can never
//     each hold what the other waits for;
//   - nest on ONE connection: a lock taken while the context already carries a
//     hold reuses that hold's connection, so nesting never pins a second one;
//   - watch their connection: a session-level lock dies with its connection (a
//     failover, an idle-timeout, an operator's pg_terminate_backend), and the
//     context the guarded work receives is cancelled with ErrLockLost when that
//     happens.
//
// Each hold dials its own connection rather than borrowing from the request
// pool: a pooled connection that ever returned still holding a session lock
// would hand the lock to an unrelated request, and borrowing from the request
// pool let slow guarded work starve the queries it guards. The number of
// concurrent holds is bounded by LockPoolMaxConns (docs/ENV.md).

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The classid half of each ordered lock's two-argument key. Distinct from every
// other class in this package (LoginSupersedeLockClass, SecretRowLockClass,
// PushPathListLockClass) and from the one-argument keys, which carry a
// different objsubid.
const (
	// RunOpLockClass serializes the operations that act on one run's proxy and
	// broker credentials from a row they read earlier (a revive, the lease
	// sweep's stop). obj = the run id.
	RunOpLockClass int32 = 0x57524F50 // ASCII "WROP"
	// ADORunTokenLockClass serializes the mint and revoke of one run's Azure
	// DevOps run token. Reserved for ha-l2.4, which takes it; named here so the
	// order below is one list.
	ADORunTokenLockClass int32 = 0x57415254 // ASCII "WART"
	// AWSSSOLockClass is the AWS SSO refresh single-flight: obj = the
	// secret-store namespace the credential lives in.
	AWSSSOLockClass int32 = 0x57415753 // ASCII "WAWS"
	// ADOSignInLockClass is the Entra sign-in refresh single-flight (Azure
	// DevOps and Azure AI Foundry): obj = person and provider row.
	ADOSignInLockClass int32 = 0x5741444F // ASCII "WADO"
	// SiteConfigLockClass serializes the read-modify-write writers of the one
	// site-config document.
	SiteConfigLockClass int32 = 0x57534346 // ASCII "WSCF"
	// CapEnforcementLockClass serializes writers of the capability-enforcement
	// document.
	CapEnforcementLockClass int32 = 0x57434150 // ASCII "WCAP"
	// AuditSweepLockClass admits one audit-chain verify sweep at a time. It is
	// taken last and only ever tried, never waited for.
	AuditSweepLockClass int32 = 0x57415357 // ASCII "WASW"
)

// LockOrder is the one documented order. A lock may be taken while holding
// others only if its class appears LATER in this list than every class already
// held (the same key again is re-entrant and always allowed).
//
//	run operation, then the Azure DevOps run-token mint/revoke, then the AWS
//	SSO refresh, then the Azure DevOps sign-in refresh (the order the credential
//	erase takes its two), then the document locks, and the audit sweep last.
var LockOrder = []int32{
	RunOpLockClass,
	ADORunTokenLockClass,
	AWSSSOLockClass,
	ADOSignInLockClass,
	SiteConfigLockClass,
	CapEnforcementLockClass,
	AuditSweepLockClass,
}

// Lock failures. A refusal (ErrLockBusy, ErrLockNoCapacity, ErrLockUnavailable)
// means the guarded work did not run and may be retried; ErrLockOrder is a
// programming error.
var (
	// ErrLockBusy: the lock is held elsewhere and the wait budget ran out (or,
	// for TryLock's caller, ok is false and this is not returned).
	ErrLockBusy = errors.New("db: the lock is held elsewhere")
	// ErrLockNoCapacity: every connection the lock pool may hold is in use.
	ErrLockNoCapacity = errors.New("db: the lock pool has no free connection")
	// ErrLockUnavailable: the database could not be asked.
	ErrLockUnavailable = errors.New("db: the lock could not be taken")
	// ErrLockOrder: a lock was taken out of LockOrder.
	ErrLockOrder = errors.New("db: lock taken out of order")
	// ErrLockLost is the cause of the context a hold hands out, once the
	// connection holding the lock is gone.
	ErrLockLost = errors.New("db: the lock was lost with its connection")
)

// LockRefused reports whether err is a refusal the caller answers with a
// retry rather than a fault of its own.
func LockRefused(err error) bool {
	return errors.Is(err, ErrLockBusy) || errors.Is(err, ErrLockNoCapacity) || errors.Is(err, ErrLockUnavailable)
}

// LockWait is the TOTAL budget a blocking lock waits before it refuses.
// Generous: a refresh holder's own worst case is two token calls and a retry
// delay, about 21 seconds, and a caller whose token already expired has
// nothing else to do but wait for it.
var LockWait = 30 * time.Second

// LockPoolMaxConns bounds the connections one process holds for locks at once;
// a lock beyond it waits LockPoolAcquireWait and is then refused. Nested locks
// share their caller's connection, so this bounds concurrent guarded
// operations, not locks.
var LockPoolMaxConns = 8

// LockPoolAcquireWait bounds the wait for one of the LockPoolMaxConns slots.
var LockPoolAcquireWait = 2 * time.Second

// LockWatchInterval is how often a hold checks that its connection still holds
// every lock it is meant to.
var LockWatchInterval = time.Second

// lockTimeoutMargin is how far a lock statement's context outlives the
// server-side lock_timeout it carries.
const lockTimeoutMargin = 2 * time.Second

// LockKey names one lock: its class (one of the *LockClass constants) and the
// object it guards, folded to 32 bits.
type LockKey struct{ Class, Obj int32 }

// NewLockKey keys class to parts, which are joined unambiguously and folded
// with crc32. A collision is harmless: it only serializes two unrelated
// objects. No parts is the class's single document lock.
func NewLockKey(class int32, parts ...string) LockKey {
	if len(parts) == 0 {
		return LockKey{Class: class}
	}
	return LockKey{Class: class, Obj: int32(crc32.ChecksumIEEE([]byte(strings.Join(parts, "\x00"))))}
}

func (k LockKey) String() string { return fmt.Sprintf("(%#x,%d)", uint32(k.Class), k.Obj) }

// Locker takes the ordered locks. PG-backed lockers serialize across replicas;
// the local one serializes within a process (a store with no database).
type Locker interface {
	// Lock waits up to wait for k. The returned context is ctx's, carrying the
	// hold, and is cancelled when the lock is lost or released; the guarded
	// work must use it.
	Lock(ctx context.Context, k LockKey, wait time.Duration) (context.Context, func(), error)
	// TryLock takes k only if it is free: ok is false, with a nil error, when
	// it is held elsewhere.
	TryLock(ctx context.Context, k LockKey) (context.Context, func(), bool, error)
}

func lockRank(class int32) int { return slices.Index(LockOrder, class) }

// holds is the set of locks a context carries. Immutable: nesting makes a new
// one, so goroutines sharing a parent context never see each other's holds.
type holds struct {
	keys []LockKey
	conn *lockConn // nil for the local locker
}

type holdsKey struct{}

func heldFrom(ctx context.Context) *holds {
	h, _ := ctx.Value(holdsKey{}).(*holds)
	return h
}

// HeldLocks lists the locks ctx carries, outermost first (tests, diagnostics).
func HeldLocks(ctx context.Context) []LockKey {
	if h := heldFrom(ctx); h != nil {
		return slices.Clone(h.keys)
	}
	return nil
}

// checkLockOrder reports whether k may be taken while held is held:
// reentrant=true means k is already held (nothing more to take).
func checkLockOrder(held []LockKey, k LockKey) (reentrant bool, err error) {
	if slices.Contains(held, k) {
		return true, nil
	}
	rank := lockRank(k.Class)
	if rank < 0 {
		return false, fmt.Errorf("%w: class %#x is not in LockOrder", ErrLockOrder, uint32(k.Class))
	}
	for _, h := range held {
		if lockRank(h.Class) >= rank {
			return false, fmt.Errorf("%w: %v taken while holding %v", ErrLockOrder, k, h)
		}
	}
	return false, nil
}

func withHolds(ctx context.Context, h *holds) context.Context {
	return context.WithValue(ctx, holdsKey{}, h)
}

// PGLocker is the Postgres-backed Locker.
type PGLocker struct {
	cfg *pgx.ConnConfig
	sem chan struct{}
}

var pgLockers sync.Map // *pgxpool.Pool -> *PGLocker

// LockerFor returns the process's one PGLocker for pool, created on first use
// from the pool's own connection settings. Every store.PG over one pool shares
// it, so LockPoolMaxConns bounds the process, not each copy of the store.
func LockerFor(pool *pgxpool.Pool) *PGLocker {
	if l, ok := pgLockers.Load(pool); ok {
		return l.(*PGLocker)
	}
	l, _ := pgLockers.LoadOrStore(pool, NewPGLocker(pool, LockPoolMaxConns))
	return l.(*PGLocker)
}

// NewPGLocker is a locker of its own with size connection slots (tests).
func NewPGLocker(pool *pgxpool.Pool, size int) *PGLocker {
	return &PGLocker{cfg: pool.Config().ConnConfig.Copy(), sem: make(chan struct{}, size)}
}

// lockConn is one dedicated connection and the locks it holds.
type lockConn struct {
	mu     sync.Mutex // serializes statements; the watcher only ever tries it
	conn   *pgx.Conn
	keys   []LockKey
	closed bool
}

func (c *lockConn) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	bg, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Unlock first so the lock is free the moment the call returns, not when
	// the server notices the socket is gone.
	c.conn.Exec(bg, `SELECT pg_advisory_unlock_all()`) //nolint:errcheck // best-effort; closing the session drops them anyway
	c.conn.Close(bg)                                   //nolint:errcheck // best-effort
}

func (c *lockConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// take runs the lock statement on the connection, mu held by the caller.
func (c *lockConn) take(ctx context.Context, k LockKey, try bool, wait time.Duration) (bool, error) {
	if try {
		var got bool
		if err := c.conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, $2)`, k.Class, k.Obj).Scan(&got); err != nil {
			return false, fmt.Errorf("%w: %w", ErrLockUnavailable, err)
		}
		return got, nil
	}
	// SET takes no bind parameters; the value is an integer derived from wait.
	if _, err := c.conn.Exec(ctx, fmt.Sprintf(`SET lock_timeout = '%dms'`, max(wait.Milliseconds(), 1))); err != nil {
		return false, fmt.Errorf("%w: set lock timeout: %w", ErrLockUnavailable, err)
	}
	if _, err := c.conn.Exec(ctx, `SELECT pg_advisory_lock($1, $2)`, k.Class, k.Obj); err != nil {
		var pe interface{ SQLState() string }
		switch {
		case errors.As(err, &pe) && pe.SQLState() == "55P03": // lock_not_available: lock_timeout fired
			return false, fmt.Errorf("%w: %v", ErrLockBusy, k)
		case ctx.Err() != nil:
			return false, fmt.Errorf("%w: waiting for %v: %w", ErrLockBusy, k, ctx.Err())
		}
		return false, fmt.Errorf("%w: %w", ErrLockUnavailable, err)
	}
	return true, nil
}

// verify reports whether the session still holds every one of keys.
func (c *lockConn) verify(ctx context.Context, keys []LockKey) error {
	rows, err := c.conn.Query(ctx, `SELECT classid::bigint, objid::bigint FROM pg_locks
		WHERE locktype = 'advisory' AND pid = pg_backend_pid() AND objsubid = 2 AND granted`)
	if err != nil {
		return err
	}
	type pair struct{ class, obj int64 }
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (pair, error) {
		var p pair
		return p, r.Scan(&p.class, &p.obj)
	})
	if err != nil {
		return err
	}
	for _, k := range keys {
		if !slices.Contains(got, pair{int64(uint32(k.Class)), int64(uint32(k.Obj))}) {
			return fmt.Errorf("session no longer holds %v", k)
		}
	}
	return nil
}

// LockHeld asks the database, on the hold's own connection, whether every lock
// ctx carries is still held: nil only when it is, ErrLockLost when it is not
// or the session cannot be asked. It answers whether or not ctx has ended, and
// that is its use: a hold's context ended first by its parent keeps that cause
// for good, so a lock lost afterwards never shows in it. Work that must not
// act once another replica may hold the lock asks here immediately before and
// after acting. A session-level lock lost with its session is never held by
// that session again, so a nil after acting means the lock was held
// throughout. A local hold cannot be lost and is always held.
func LockHeld(ctx context.Context) error {
	h := heldFrom(ctx)
	if h == nil {
		return fmt.Errorf("%w: the context carries no lock", ErrLockLost)
	}
	if h.conn == nil {
		return nil
	}
	c := h.conn
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("%w: the hold was released", ErrLockLost)
	}
	qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*LockWatchInterval+time.Second)
	defer cancel()
	if err := c.verify(qctx, h.keys); err != nil {
		return fmt.Errorf("%w: %w", ErrLockLost, err)
	}
	return nil
}

func (l *PGLocker) Lock(ctx context.Context, k LockKey, wait time.Duration) (context.Context, func(), error) {
	lctx, release, _, err := l.lock(ctx, k, wait, false)
	return lctx, release, err
}

func (l *PGLocker) TryLock(ctx context.Context, k LockKey) (context.Context, func(), bool, error) {
	return l.lock(ctx, k, 0, true)
}

func (l *PGLocker) lock(ctx context.Context, k LockKey, wait time.Duration, try bool) (context.Context, func(), bool, error) {
	if h := heldFrom(ctx); h != nil && h.conn != nil && !h.conn.isClosed() {
		return l.nest(ctx, h, k, wait, try)
	}
	// A hold whose connection is gone, or the local locker's, is not a hold
	// of this locker: start from nothing.
	if _, err := checkLockOrder(nil, k); err != nil {
		return ctx, nil, false, err
	}

	acquire := time.NewTimer(LockPoolAcquireWait)
	defer acquire.Stop()
	select {
	case l.sem <- struct{}{}:
	case <-acquire.C:
		return ctx, nil, false, fmt.Errorf("%w (%d in use)", ErrLockNoCapacity, cap(l.sem))
	case <-ctx.Done():
		return ctx, nil, false, fmt.Errorf("%w: waiting for a lock connection: %w", ErrLockUnavailable, ctx.Err())
	}
	free := func() { <-l.sem }

	budget := wait
	if try || budget <= 0 {
		budget = 10 * time.Second
	}
	// The context outlives lock_timeout by a margin, so the server's own
	// refusal (a healthy connection) is what ends a wait, not a cancelled
	// statement.
	dctx, cancel := context.WithTimeout(ctx, budget+lockTimeoutMargin)
	defer cancel()
	conn, err := pgx.ConnectConfig(dctx, l.cfg)
	if err != nil {
		free()
		return ctx, nil, false, fmt.Errorf("%w: connect: %w", ErrLockUnavailable, err)
	}
	lc := &lockConn{conn: conn}
	lc.mu.Lock()
	got, err := lc.take(dctx, k, try, budget)
	if err == nil && got {
		lc.keys = []LockKey{k}
	}
	lc.mu.Unlock()
	if err != nil || !got {
		lc.close()
		free()
		return ctx, nil, false, err
	}

	hctx, hcancel := context.WithCancelCause(withHolds(ctx, &holds{keys: []LockKey{k}, conn: lc}))
	stop := make(chan struct{})
	go lc.watch(hcancel, stop, LockWatchInterval)
	var once sync.Once
	release := func() {
		once.Do(func() {
			close(stop)
			lc.close()
			free()
			hcancel(context.Canceled)
		})
	}
	return hctx, release, true, nil
}

// nest takes k on the caller's own connection.
func (l *PGLocker) nest(ctx context.Context, h *holds, k LockKey, wait time.Duration, try bool) (context.Context, func(), bool, error) {
	reentrant, err := checkLockOrder(h.keys, k)
	if err != nil {
		return ctx, nil, false, err
	}
	if reentrant {
		return ctx, func() {}, true, nil
	}
	budget := wait
	if try || budget <= 0 {
		budget = 10 * time.Second
	}
	nctx, cancel := context.WithTimeout(ctx, budget+lockTimeoutMargin)
	defer cancel()
	lc := h.conn
	lc.mu.Lock()
	defer lc.mu.Unlock()
	if lc.closed {
		return ctx, nil, false, fmt.Errorf("%w: the enclosing lock was released or lost", ErrLockUnavailable)
	}
	got, err := lc.take(nctx, k, try, budget)
	if err != nil {
		// A statement that failed for any reason but the wait means the
		// session is suspect; the watcher confirms and cancels.
		return ctx, nil, false, err
	}
	if !got {
		return ctx, nil, false, nil
	}
	lc.keys = append(lc.keys, k)
	var once sync.Once
	release := func() {
		once.Do(func() {
			lc.mu.Lock()
			defer lc.mu.Unlock()
			if lc.closed {
				return
			}
			bg, bcancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer bcancel()
			lc.conn.Exec(bg, `SELECT pg_advisory_unlock($1, $2)`, k.Class, k.Obj) //nolint:errcheck // best-effort; the session ending drops it
			if i := slices.Index(lc.keys, k); i >= 0 {
				lc.keys = slices.Delete(lc.keys, i, i+1)
			}
		})
	}
	return withHolds(ctx, &holds{keys: append(slices.Clone(h.keys), k), conn: lc}), release, true, nil
}

// watch cancels the hold's context with ErrLockLost when the session stops
// holding what it took. A statement already in flight on the connection (a
// nested lock waiting) is skipped: its own error reports a dead session, and
// the next beat looks again.
func (c *lockConn) watch(cancel context.CancelCauseFunc, stop <-chan struct{}, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		if !c.mu.TryLock() {
			continue
		}
		if c.closed {
			c.mu.Unlock()
			return
		}
		bg, bcancel := context.WithTimeout(context.Background(), 2*every+time.Second)
		err := c.verify(bg, c.keys)
		bcancel()
		c.mu.Unlock()
		if err != nil {
			cancel(fmt.Errorf("%w: %w", ErrLockLost, err))
			return
		}
	}
}

// LocalLocker is the in-process Locker for a store with no database: the same
// order, nesting and refusal rules, serializing within one process only.
type LocalLocker struct {
	mu sync.Mutex
	m  map[LockKey]*localEntry
}

type localEntry struct {
	ch   chan struct{}
	refs int
}

// NewLocalLocker returns an empty LocalLocker.
func NewLocalLocker() *LocalLocker { return &LocalLocker{m: map[LockKey]*localEntry{}} }

func (l *LocalLocker) entry(k LockKey) *localEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.m[k]
	if e == nil {
		e = &localEntry{ch: make(chan struct{}, 1)}
		l.m[k] = e
	}
	e.refs++
	return e
}

func (l *LocalLocker) drop(k LockKey, e *localEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e.refs--; e.refs == 0 {
		delete(l.m, k)
	}
}

func (l *LocalLocker) Lock(ctx context.Context, k LockKey, wait time.Duration) (context.Context, func(), error) {
	lctx, release, _, err := l.lock(ctx, k, wait, false)
	return lctx, release, err
}

func (l *LocalLocker) TryLock(ctx context.Context, k LockKey) (context.Context, func(), bool, error) {
	return l.lock(ctx, k, 0, true)
}

func (l *LocalLocker) lock(ctx context.Context, k LockKey, wait time.Duration, try bool) (context.Context, func(), bool, error) {
	var held []LockKey
	if h := heldFrom(ctx); h != nil && h.conn == nil {
		held = h.keys
	}
	reentrant, err := checkLockOrder(held, k)
	if err != nil {
		return ctx, nil, false, err
	}
	if reentrant {
		return ctx, func() {}, true, nil
	}
	e := l.entry(k)
	if try {
		select {
		case e.ch <- struct{}{}:
		default:
			l.drop(k, e)
			return ctx, nil, false, nil
		}
	} else {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case e.ch <- struct{}{}:
		case <-timer.C:
			l.drop(k, e)
			return ctx, nil, false, fmt.Errorf("%w: %v", ErrLockBusy, k)
		case <-ctx.Done():
			l.drop(k, e)
			return ctx, nil, false, fmt.Errorf("%w: waiting for %v: %w", ErrLockBusy, k, ctx.Err())
		}
	}
	// Cancelled at release, as the PG locker's is: a caller that keeps using
	// the context after unlocking must fail here, in a test, as it would there.
	hctx, hcancel := context.WithCancelCause(withHolds(ctx, &holds{keys: append(slices.Clone(held), k)}))
	var once sync.Once
	release := func() {
		once.Do(func() {
			<-e.ch
			l.drop(k, e)
			hcancel(context.Canceled)
		})
	}
	return hctx, release, true, nil
}
