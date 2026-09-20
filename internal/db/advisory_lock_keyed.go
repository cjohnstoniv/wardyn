// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The BLOCKING, two-argument advisory lock: one helper and the one key space
// that uses it. Split from db.go along the same seam its test file already had
// (advisory_lock_pg_test.go) — db.go keeps the pool, the migration loop and the
// process-lifetime keys taken with the non-blocking TryAdvisoryLock.

package db

import (
	"context"
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
// wait on each other, and taken with the BLOCKING AdvisoryLockKeyed rather than
// TryAdvisoryLock: the loser of a sign-in race must run AFTER the winner, not
// instead of it — skipping the supersede is the defect.
//
// A SEPARATE key space from every int64 key above, not a near-miss of one:
// Postgres records the two-argument form with objsubid 1 and the one-argument
// form with objsubid 2, so the two forms cannot conflict whatever the numbers
// are. Any stable value works.
const LoginSupersedeLockClass int32 = 0x574C474E // ASCII "WLGN"

// LoginSupersedeLockWait bounds how long a sign-in waits for its
// LoginSupersedeLockClass key before giving up and proceeding UNLOCKED — every
// call site fails open, because nobody may be refused a sign-in over a busy
// lock.
//
// The bound is load-bearing, not decoration. AdvisoryLockKeyed borrows a pool
// connection for the whole hold, and the guarded work — the supersede reads,
// CreateRun, the audit stamp — runs on a SECOND one, so at the documented
// pool_max_conns minimum of 3 (docs/OPERATIONS.md) three launches waiting
// without a bound would hold every connection while each waits for a fourth
// that can never come. A wait that expires releases its connection and the
// launch proceeds as it did in 0.7.8.
//
// 5s, the same value and the same reasoning as AuditChainLockTimeout: the
// legitimate wait here is one other launch doing a handful of indexed
// statements, so 5s absorbs a deep queue before it ever gives up on a lock it
// would have got.
var LoginSupersedeLockWait = 5 * time.Second

// AdvisoryLockKeyed takes the SESSION-level TWO-argument advisory lock
// (class, obj) on a connection borrowed from pool, WAITING up to wait for a
// current holder to let go and returning an error if it does not. It is
// TryAdvisoryLock's blocking sibling — same acquire and release shape, same
// warning about the borrowed connection — for work that must be SERIALIZED
// rather than skipped.
//
// The wait is enforced server-side by lock_timeout, which fires only on a lock
// WAIT (never on a slow-but-progressing statement) and reports the
// distinguishable SQLSTATE 55P03; AuditChainLockTimeout's note carries the
// measurement. SET LOCAL is transaction-scoped, so the acquire runs inside a
// short transaction for the sole purpose of scoping that setting — nothing
// leaks onto the pooled connection. The LOCK is session-level and therefore
// outlives the commit, which is the whole point: the caller's guarded work is
// several independent statements AFTER this returns, and a transaction-scoped
// lock could not cover them.
//
// Call the returned release (deferred, on every path) to unlock and hand the
// connection back — skipping it strands a pooled conn for the life of the
// process. Only for work of BOUNDED duration, and like TryAdvisoryLock this
// needs pool_max_conns >= 2: the caller's own queries need a SECOND conn while
// this one is held.
func AdvisoryLockKeyed(ctx context.Context, pool *pgxpool.Pool, class, obj int32, wait time.Duration) (release func(), err error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("db: acquire advisory lock conn: %w", err)
	}
	unlock := func() {
		// Unlock on a background context: ctx is typically cancelled at shutdown,
		// exactly when releasing matters most. Best-effort — the lock also dies
		// with the session when the conn is finally closed.
		conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1, $2)`, class, obj) //nolint:errcheck // best-effort release
		conn.Release()
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		conn.Release()
		return nil, fmt.Errorf("db: begin advisory lock tx: %w", err)
	}
	// SET takes no bind parameters, so the value is formatted in — an integer
	// derived from the caller's duration, never caller text.
	if _, err := tx.Exec(ctx, fmt.Sprintf(`SET LOCAL lock_timeout = '%dms'`, wait.Milliseconds())); err != nil {
		tx.Rollback(ctx) //nolint:errcheck // the conn is released either way
		conn.Release()
		return nil, fmt.Errorf("db: set advisory lock timeout: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_lock($1, $2)`, class, obj); err != nil {
		tx.Rollback(ctx) //nolint:errcheck // the conn is released either way
		conn.Release()
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
