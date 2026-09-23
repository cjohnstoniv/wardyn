// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The BLOCKING, two-argument advisory lock: one helper and the one key space
// that uses it. Split from db.go along the same seam its test file already had
// (advisory_lock_pg_test.go) — db.go keeps the pool, the migration loop and the
// process-lifetime keys taken with the non-blocking TryAdvisoryLock.

package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// LoginSupersedeLockClass is the classid half of a TWO-argument advisory lock
// key — pg_advisory_lock(int4,int4) — whose objid is one person's actor string
// folded to an int32 (store.LoginLocker). It serializes ONE person's concurrent
// sign-in launches: the supersede pass, the run insert and the second supersede
// pass are independent statements, so without it two launches each read the
// other as not-yet-existing, both survive, and the person is left with two live
// sandboxes each holding a captured AWS SSO session (harnesscred_supersede.go
// names the interleaving). Per-person, so two different people's sign-ins never
// contend on the KEY, and taken with the BLOCKING AdvisoryLockKeyed rather than
// TryAdvisoryLock: the loser of a sign-in race must run AFTER the winner, not
// instead of it — skipping the supersede is the defect.
//
// A SEPARATE key space from every int64 key in db.go, not a near-miss of one.
// Measured on Postgres 16 rather than assumed — `SELECT locktype, classid,
// objid, objsubid FROM pg_locks WHERE locktype='advisory'` after taking one of
// each reports the one-argument bigint form with objsubid 1 and this
// two-argument form with objsubid 2 — so the two forms cannot conflict whatever
// the numbers are. Any stable value works.
const LoginSupersedeLockClass int32 = 0x574C474E // ASCII "WLGN"

// LoginSupersedeLockWait is the TOTAL budget one caller spends trying to take a
// keyed lock — the in-process slot, the pool connection and the lock itself —
// before giving up. A caller that runs out of it is REFUSED (retry), not let
// through unlocked: a wait that expires is the concurrent burst the lock exists
// to serialize (#505). Only ErrAdvisoryLockNoCapacity proceeds unlocked.
//
// 5s, the same value and the same reasoning as AuditChainLockTimeout: the
// legitimate wait here is one other launch doing a handful of indexed
// statements, so 5s absorbs a deep queue before it ever gives up on a lock it
// would have got.
var LoginSupersedeLockWait = 5 * time.Second

// ErrAdvisoryLockNoCapacity is AdvisoryLockKeyed's one STRUCTURAL refusal: the
// pool cannot spare advisoryLockFreeConnsNeeded connections, which on a pool at
// the documented floor (2) is every call, not a burst. Its caller proceeds
// unlocked and says so on the audit trail; every OTHER error is a wait or a
// database fault, and its caller refuses.
var ErrAdvisoryLockNoCapacity = errors.New("db: pool cannot spare a connection for an advisory lock")

// advisoryLockAcquireWait bounds the POOL ACQUIRE specifically, and it is short
// on purpose.
//
// Neither lock_timeout nor the total budget's tail covers this: a pool with no
// free connection does not ERROR on Acquire, it BLOCKS until the context ends,
// so without a bound of its own an exhausted pool turns an optional lock into a
// stall — and wardynd sets no http.Server WriteTimeout and mounts no
// TimeoutHandler (cmd/wardynd/boot_serve.go), so a request context dies only
// when the client disconnects. The capacity check below has just observed spare
// connections, so a wait here means a burst arrived in between; the lock is
// optional, and the honest move is to give it up at once rather than queue for
// it while holding the in-process slot.
const advisoryLockAcquireWait = 250 * time.Millisecond

// advisoryLockFreeConnsNeeded is how many connections the pool must have to
// spare before a keyed lock is taken at all: ONE for the hold, and at least one
// for the guarded work, which needs the pool AGAIN while the hold is live
// (api.launchHarnessLoginRun reads the caller's live runs and inserts the new
// one between acquire and release).
//
// Without this check the lock self-deadlocks at the pool size operators are
// actually told to use. docs/ENV.md's WARDYN_PG_DSN row asks for "at least 2,
// and at least 4 with the ground-truth rotator enabled", and that same row is
// what makes the arithmetic tight: the single-instance lock holds one
// connection for the whole process lifetime, the ground-truth rotator's leader
// election holds another once acquired, and the lifecycle reaper borrows one
// per tick. At pool_max_conns=2 on a serving daemon exactly one connection is
// left — a hold would take it and the guarded work would then block on Acquire
// with nothing to wait for. Below the threshold this reports
// ErrAdvisoryLockNoCapacity instead, and the caller audits it and proceeds
// unlocked: a 2-connection deployment is left exactly as unserialized as it
// was before this lock existed — on the record — rather than wedged by it.
//
// A snapshot, and racy by nature — another goroutine may take the connection a
// microsecond later. That is acceptable only because of the two guards around
// it: advisoryLockGate caps this process's own holds at one, so the check never
// races its own siblings, and advisoryLockAcquireWait bounds the loser of any
// other race to a quarter second.
const advisoryLockFreeConnsNeeded = 2

// advisoryLockGate caps a PROCESS at one keyed-lock hold at a time, so the
// connections this pins never scale with sign-in concurrency: one, or none,
// whatever the traffic.
//
// It is about CONNECTIONS, not keys, which is why it is one slot shared by
// every key rather than one slot per key. Different people's sign-ins fold to
// different objids and never contend on the lock itself, so before this gate
// existed N simultaneous sign-ins pinned N connections and starved the very
// queries they were guarding: at pool_max_conns=3 two people were enough to
// wedge every database-backed request in the daemon, not just their own.
// Cross-replica correctness is untouched — the database lock is still what
// serializes two wardynd instances, and this only decides how many of THIS
// process's goroutines may hold one at a time.
//
// Waiting here holds nothing but a channel slot, which is the whole point: the
// shape it replaces queued on the database while pinning a connection. Callers
// must not take a keyed lock re-entrantly — one goroutine holding the slot and
// asking for it again would wait out its own budget. Nothing does: the sign-in
// launch and the credential capture each take it exactly once, and the
// capture's other lock (api.lockAWSSSOOwner) is an in-process mutex on a
// different key, always taken second.
var advisoryLockGate = make(chan struct{}, 1)

// AdvisoryLockKeyed takes the SESSION-level TWO-argument advisory lock
// (class, obj) on a connection borrowed from pool, waiting up to wait in TOTAL
// and returning an error if it cannot. It is TryAdvisoryLock's blocking sibling
// — same acquire and release shape — for work that must be SERIALIZED rather
// than skipped.
//
// Every arm is bounded — the in-process slot (another hold did not finish
// inside the budget), pool capacity (checked, then bounded at 250ms), and the
// lock wait itself. ErrAdvisoryLockNoCapacity (the capacity check) is the one
// error a caller may treat as "proceed unlocked"; every other one means the
// work was not serialized and must not run.
//
// WHAT IT PINS, plainly: exactly one pool connection, for the duration of one
// hold, and at most one per process at any moment (advisoryLockGate). None at
// all when the pool cannot spare advisoryLockFreeConnsNeeded.
//
// The lock wait is ALSO enforced server-side by lock_timeout, which fires only
// on a lock WAIT (never on a slow-but-progressing statement) and reports the
// distinguishable SQLSTATE 55P03; AuditChainLockTimeout's note carries the
// measurement. SET LOCAL is transaction-scoped, so the acquire runs inside a
// short transaction for the sole purpose of scoping that setting — nothing
// leaks onto the pooled connection. The LOCK is session-level and therefore
// outlives the commit, which is the whole point: the caller's guarded work is
// several independent statements AFTER this returns, and a transaction-scoped
// lock could not cover them.
//
// Call the returned release (deferred, on every path) to unlock, hand the
// connection back and free the in-process slot — skipping it strands both for
// the life of the process. Only for work of BOUNDED duration.
func AdvisoryLockKeyed(ctx context.Context, pool *pgxpool.Pool, class, obj int32, wait time.Duration) (release func(), err error) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	select {
	case advisoryLockGate <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("db: wait for the in-process advisory lock slot: %w", ctx.Err())
	}
	ungate := func() { <-advisoryLockGate }

	if st := pool.Stat(); st.MaxConns()-st.AcquiredConns() < advisoryLockFreeConnsNeeded {
		ungate()
		return nil, fmt.Errorf("%w (max_conns %d, acquired %d, need %d free)",
			ErrAdvisoryLockNoCapacity, st.MaxConns(), st.AcquiredConns(), advisoryLockFreeConnsNeeded)
	}

	// Bounded separately from the total budget — see advisoryLockAcquireWait.
	actx, acancel := context.WithTimeout(ctx, advisoryLockAcquireWait)
	defer acancel()
	conn, err := pool.Acquire(actx)
	if err != nil {
		ungate()
		return nil, fmt.Errorf("db: acquire advisory lock conn: %w", err)
	}
	unlock := func() {
		// Unlock on a background context: ctx is typically cancelled at shutdown,
		// exactly when releasing matters most. Best-effort — the lock also dies
		// with the session when the conn is finally closed.
		conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1, $2)`, class, obj) //nolint:errcheck // best-effort release
		conn.Release()
		ungate()
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		conn.Release()
		ungate()
		return nil, fmt.Errorf("db: begin advisory lock tx: %w", err)
	}
	// SET takes no bind parameters, so the value is formatted in — an integer
	// derived from the caller's duration, never caller text.
	if _, err := tx.Exec(ctx, fmt.Sprintf(`SET LOCAL lock_timeout = '%dms'`, wait.Milliseconds())); err != nil {
		tx.Rollback(ctx) //nolint:errcheck // the conn is released either way
		conn.Release()
		ungate()
		return nil, fmt.Errorf("db: set advisory lock timeout: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_lock($1, $2)`, class, obj); err != nil {
		tx.Rollback(ctx) //nolint:errcheck // the conn is released either way
		conn.Release()
		ungate()
		return nil, fmt.Errorf("db: advisory lock (%d,%d): %w", class, obj, err)
	}
	if err := tx.Commit(ctx); err != nil {
		// The lock is held by now (it is session-level, so a failed commit does
		// not drop it) — unlock rather than leak it with the connection.
		unlock()
		return nil, fmt.Errorf("db: commit advisory lock tx: %w", err)
	}
	return unlock, nil
}
