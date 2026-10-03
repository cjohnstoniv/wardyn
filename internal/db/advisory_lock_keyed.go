// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The BLOCKING, two-argument advisory lock. db.go keeps the pool, the
// migration loop and the process-lifetime keys taken with TryAdvisoryLock.

package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// LoginSupersedeLockClass is the classid half of a TWO-argument advisory lock
// key (objid = one person's actor string folded to int32, store.LoginLocker).
// Serializes ONE person's concurrent sign-in launches: without it, two
// launches' independent supersede/insert/supersede statements can each read
// the other as not-yet-existing, leaving two live sandboxes each holding a
// captured AWS SSO session. Taken BLOCKING: the loser must run AFTER the
// winner, not skip the supersede. A separate key space from db.go's int64
// keys — the one- and two-argument lock forms carry different objsubid, so
// they cannot collide.
const LoginSupersedeLockClass int32 = 0x574C474E // ASCII "WLGN"

// SecretRowLockClass is the classid of the TRANSACTION-scoped two-argument
// lock secretstore/pg takes around a store-mode Put, keyed to one (owner,
// name) row: one writer per row at a time across replicas. Taken with
// pg_advisory_xact_lock inside the Put's own transaction, released by the
// commit that writes the row — not through AdvisoryLockKeyed.
const SecretRowLockClass int32 = 0x57534543 // ASCII "WSEC"

// PushPathListLockClass is the classid of the TRANSACTION-scoped two-argument
// lock store.PG.RecordPushPathList takes, keyed to one run: that run's list
// inserts serialize, so the per-run cap it counts is the cap it enforces.
const PushPathListLockClass int32 = 0x57505054 // ASCII "WPPT"

// RunCapLockClass is the classid of the TRANSACTION-scoped two-argument lock
// store.PG.CreateRunUnderCap takes (second key 0): one capped run insert at a
// time across replicas, so the count it checks is the cap it enforces.
const RunCapLockClass int32 = 0x57525243 // ASCII "WRRC"

// GovernanceGraphLockClass is the classid of the TRANSACTION-scoped two-argument lock
// store.PG.WriteGovernanceProfile and DeleteGovernanceProfile take (second key 0): one write to the
// governance profile graph at a time, so the cycle and depth checks they make are the ones the
// stored graph keeps.
const GovernanceGraphLockClass int32 = 0x57474750 // ASCII "WGGP"

// LoginSupersedeLockWait is the TOTAL budget one caller spends trying to take
// a keyed lock before being REFUSED (retry) rather than let through unlocked;
// only ErrAdvisoryLockNoCapacity proceeds unlocked. Matches
// AuditChainLockTimeout: enough to absorb a deep queue of a rival launch's
// handful of indexed statements.
var LoginSupersedeLockWait = 5 * time.Second

// ErrAdvisoryLockNoCapacity is AdvisoryLockKeyed's one STRUCTURAL refusal: the
// pool can't spare advisoryLockFreeConnsNeeded connections (every call, on a
// pool at the documented floor of 2). The caller proceeds unlocked and audits
// it; every other error is a wait or a database fault, and refuses.
var ErrAdvisoryLockNoCapacity = errors.New("db: pool cannot spare a connection for an advisory lock")

// advisoryLockAcquireWait bounds the POOL ACQUIRE specifically: Acquire on an
// exhausted pool BLOCKS rather than errors, and wardynd sets no request-level
// write timeout, so an unbounded wait here would turn an optional lock into a
// stall.
const advisoryLockAcquireWait = 250 * time.Millisecond

// advisoryLockFreeConnsNeeded: connections the pool must have spare before a
// keyed lock is taken at all — ONE for the hold, one more for the guarded
// work (which needs the pool again while the hold is live). Without this
// check the lock self-deadlocks at the documented minimum pool size (2).
// A racy snapshot, acceptable only because advisoryLockGate caps this process
// at one hold and advisoryLockAcquireWait bounds any other race to 250ms.
const advisoryLockFreeConnsNeeded = 2

// advisoryLockGate caps a PROCESS at one keyed-lock hold at a time so its
// pinned connections never scale with sign-in concurrency — one slot shared
// by every key, not per-key, since N simultaneous sign-ins with no contention
// on the lock itself still pinned N connections and starved the daemon at
// pool_max_conns=3. Cross-replica correctness is untouched.
var advisoryLockGate = make(chan struct{}, 1)

// AdvisoryLockKeyed takes the SESSION-level TWO-argument advisory lock
// (class, obj) on a connection borrowed from pool, waiting up to wait in
// TOTAL. TryAdvisoryLock's blocking sibling, for work that must be SERIALIZED
// rather than skipped. ErrAdvisoryLockNoCapacity is the one error a caller
// may treat as "proceed unlocked"; every other one means the work was not
// serialized and must not run.
//
// SET LOCAL lock_timeout scopes server-side to a short transaction, but the
// LOCK itself is session-level and outlives that commit — the caller's
// guarded work is several independent statements after this returns, which a
// transaction-scoped lock could not cover.
//
// Call the returned release (deferred, every path) to unlock and free the
// in-process slot — skipping it strands both for the life of the process.
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
		// Background context: ctx is typically cancelled at shutdown, exactly
		// when releasing matters most (best-effort; also dies with the conn).
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
	// SET takes no bind parameters; the value formatted in is an integer
	// derived from wait, never caller text.
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
