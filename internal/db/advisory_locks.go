// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// Advisory locks: the fixed session-level lock keys wardynd and the reaper
// serialize on, and the Try/blocking acquire helpers every caller shares.

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// migrateAdvisoryLockKey serializes concurrent Migrate() runs: idempotent DDL
// makes a race benign today, but a future non-idempotent migration could
// partial-apply, so the second boot BLOCKs until the first finishes and no-ops.
const migrateAdvisoryLockKey int64 = 0x5741524459_4D4947 // ASCII "WARDYMIG"; any stable value works

// ReaperAdvisoryLockKey makes the lifecycle reap tick single-flight. Unlike
// migrateAdvisoryLockKey, only ever taken with TryAdvisoryLock: a losing
// replica SKIPS the tick, since a queued second reap is duplicate work.
const ReaperAdvisoryLockKey int64 = 0x5741524459_524541 // ASCII "WARDYREA"

// GroundTruthRotatorLockKey elects the ground-truth token rotator's leader.
// Unlike ReaperAdvisoryLockKey, acquired ONCE and held for the process
// lifetime; other replicas back off and retry.
//
// HONEST CEILING: at most one steady-state leader, not mutual exclusion — an
// advisory lock dies with its SESSION, not the process, so a Postgres
// restart/failover/idle-timeout can release it under a still-running leader
// and a standby takes over unaware. Harmless only because every write is an
// atomic rename of a stateless token; work needing genuine fencing must not
// reuse this key.
const GroundTruthRotatorLockKey int64 = 0x5741524459_475452 // ASCII "WARDYGTR"

// SingleInstanceLockKey is the RUNTIME half of the one-replica safety control
// (the Helm chart's `replicas > 1` render refusal is the other half). Taken
// once at boot, held for the process lifetime: a second instance refuses to start.
//
// SECURITY: not modesty. wardynd keeps per-process state a second instance
// can't see — sharpest in the secret-masking registry (internal/secretmask),
// an in-memory map that FAILS OPEN: a recording uploaded to the wrong
// instance is persisted VERBATIM with live credentials in cleartext under a
// `success` audit event. The chart's refusal is render-time only; `kubectl
// scale` or an HPA defeats it silently.
//
// HONEST CEILING, same mechanism as GroundTruthRotatorLockKey: at most one
// steady-state instance, not a fence.
const SingleInstanceLockKey int64 = 0x5741524459_494E53 // ASCII "WARDYINS"

// SecretRekeyLockKey serializes `wardynd -rotate-age-key` maintenance mode:
// two concurrent rekeys would each re-encrypt from an old key the other
// already replaced, so the second is refused.
//
// HONEST CEILING: does NOT detect a running wardynd (that's SingleInstanceLockKey).
// "Stop the daemon first" is an operator procedure, not enforced here — a
// live daemon would write ciphertext under an already-retired key.
const SecretRekeyLockKey int64 = 0x5741524459_524B59 // ASCII "WARDYRKY"

// BootKeyLockKey serializes the CREATE path of cmd/wardynd's boot keys across
// -allow-multi-instance replicas: without it, two booting against an empty
// store each generate a key and the loser serves one nobody else holds.
const BootKeyLockKey int64 = 0x5741524459_424B59 // ASCII "WARDYBKY"

// SecretConvertLockKey makes the boot conversion of legacy (v0) secrets rows
// to envelope v1 single-writer (secretstore/pg's ConvertV0): a second replica
// booting at the same moment waits, then finds nothing left to convert.
const SecretConvertLockKey int64 = 0x5741524459_454E56 // ASCII "WARDYENV"

// TerminalSandboxSweepLockKey makes the terminal-sandbox sweep tick
// single-flight across control planes (#710), the same shape as
// ReaperAdvisoryLockKey and for the same reason SingleInstanceLockKey alone
// is not enough: that lock is at most one steady-state instance, not mutual
// exclusion (its own HONEST CEILING) — a deployment booted with
// -allow-multi-instance skips the claim entirely, and a Postgres
// restart/failover can release its session under a still-running daemon
// while a second one boots and claims it. Either way, two tickers running at
// once would both re-run teardown for the same aged KILLED run, doubling its
// run.kill rows and calling the runner twice. Always taken with
// TryAdvisoryLock: a control plane that loses simply skips the tick, since a
// queued second sweep of the same page is pure duplicate work.
const TerminalSandboxSweepLockKey int64 = 0x5741524459_545353 // ASCII "WARDYTSS"

// AuditChainLockKey serializes appends to the audit_events hash chain.
// Transaction-scoped, never session-scoped: released by the commit that makes
// the new row visible, so the next writer's head read can't miss it. Since
// 0056_audit_chain_serialize.sql the trigger takes it too and allocates seq
// inside the trigger under this lock, so position and chain link are decided
// together (0047 alone let two racing writers invert chain order against seq
// order, since seq was assigned before the trigger ran). In-tree insert paths
// still take it before their INSERT; re-entrant within a transaction, so the
// trigger's own acquisition is free for them.
// ponytail: ONE lock for the whole chain — a chain has exactly one head, and
// write volume is nowhere near contention. Upgrade only if that changes:
// per-partition chains with a key per partition.
const AuditChainLockKey int64 = 0x5741524459_434841 // ASCII "WARDYCHA"

// AuditChainLockTimeout bounds how long ANY writer waits for AuditChainLockKey.
// Since 0056 the trigger takes it on every audit_events insert, including
// from outside this repo, so a stray open transaction (an operator's psql
// session) can hold up every audit write with no Wardyn bug involved.
// Previously an unbounded request-path wait pinned a goroutine and a pool
// connection per stuck write, exhausting the pool at pool_max_conns=3.
//
// 5s is measured: a legitimate wait is other writers queueing for one
// nextval+head-read+sha256+insert each (low single-digit ms), so 5s absorbs
// a queue in the thousands, and stays under AuditSpool.Drain's 15s bound so
// the request path yields before the background drain does.
//
// A write that loses the race is not dropped: on the request path it's
// spooled locally for the drain to replay; on the broker's mint transaction
// the audit insert shares the tx with the credential, so a timeout
// fail-closed refuses the MINT rather than issue an unaudited credential.
//
// lock_timeout, not a context deadline: fires only on a lock wait (never a
// slow-but-progressing statement), server-side, SET LOCAL-scoped, reports
// SQLSTATE 55P03, and (measured on Postgres 17) also bounds the trigger's own
// acquisition, covering a writer that never takes the lock explicitly.
var AuditChainLockTimeout = 5 * time.Second

// AuditChainLockTimeoutSQL applies AuditChainLockTimeout to the current
// transaction. SET takes no bind parameters — the value is a formatted-in
// integer, never caller input. SET LOCAL reverts at commit/rollback.
func AuditChainLockTimeoutSQL() string {
	return fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", AuditChainLockTimeout.Milliseconds())
}

// TryAdvisoryLock takes session-level advisory lock key on a connection
// borrowed from pool WITHOUT waiting, reporting ok=false if held elsewhere.
// Call the returned release (deferred) or strand a pooled conn permanently.
// Only for BOUNDED work: the conn is unavailable to everyone else until
// release, so the caller's own queries need a second conn — requires
// pool_max_conns >= 2 (a 1-conn pool self-deadlocks).
func TryAdvisoryLock(ctx context.Context, pool *pgxpool.Pool, key int64) (release func(), ok bool, err error) {
	_, release, ok, err = TryAdvisoryLockConn(ctx, pool, key)
	return release, ok, err
}

// TryAdvisoryLockConn is TryAdvisoryLock that also hands back the connection holding the lock, for
// the caller that must run its own statements on that same session (wardynd -migrate-only). The
// conn is only valid until release is called; the caller must not Release it itself.
func TryAdvisoryLockConn(ctx context.Context, pool *pgxpool.Pool, key int64) (conn *pgxpool.Conn, release func(), ok bool, err error) {
	conn, err = pool.Acquire(ctx)
	if err != nil {
		return nil, nil, false, fmt.Errorf("db: acquire advisory lock conn: %w", err)
	}
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&got); err != nil {
		conn.Release()
		return nil, nil, false, fmt.Errorf("db: try advisory lock: %w", err)
	}
	if !got {
		conn.Release()
		return nil, nil, false, nil
	}
	return conn, func() {
		// Background context: ctx is typically cancelled at shutdown, exactly
		// when releasing matters most. Best-effort — lock also dies with the
		// session when the conn closes.
		conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, key) //nolint:errcheck // best-effort release
		conn.Release()
	}, true, nil
}

// AdvisoryLock is TryAdvisoryLock's BLOCKING sibling: waits for session-level
// advisory lock key until ctx ends, erroring rather than proceeding unlocked.
// Same release contract and pool_max_conns >= 2 need.
func AdvisoryLock(ctx context.Context, pool *pgxpool.Pool, key int64) (release func(), err error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("db: acquire advisory lock conn: %w", err)
	}
	unlock := func() {
		// Background context, as in TryAdvisoryLock; unlocking a lock this
		// session doesn't hold is a harmless no-op.
		conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, key) //nolint:errcheck // best-effort release
		conn.Release()
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		// pgx can return ctx's error after the server granted the lock, so
		// unlock rather than hand back a conn that may still hold it.
		unlock()
		return nil, fmt.Errorf("db: advisory lock: %w", err)
	}
	return unlock, nil
}
