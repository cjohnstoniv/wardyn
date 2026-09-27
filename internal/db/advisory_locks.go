// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// Advisory locks: the fixed session-level lock keys wardynd and the reaper
// serialize on (each documented at its const, below), and the Try/blocking
// acquire helpers every caller shares.

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// migrateAdvisoryLockKey is the fixed session-level advisory lock key that
// serializes concurrent Migrate() runs (N5). Idempotent DDL makes a race benign
// today, but a future non-idempotent migration could partial-apply if two
// wardynd boots ran the loop at once; the lock makes the second boot BLOCK until
// the first finishes, then see every migration applied and no-op. Any stable
// value works.
const migrateAdvisoryLockKey int64 = 0x5741524459_4D4947 // ASCII "WARDYMIG"; any stable value works

// ReaperAdvisoryLockKey makes the lifecycle reap tick single-flight across
// control planes. Unlike migrateAdvisoryLockKey (a BLOCKING lock — the second
// boot must still see the migrations applied), this one is only ever taken with
// TryAdvisoryLock: a replica that loses SKIPS the tick, because a queued second
// reap of the same runs is pure duplicate work and a duplicate run.autostop.
const ReaperAdvisoryLockKey int64 = 0x5741524459_524541 // ASCII "WARDYREA"

// GroundTruthRotatorLockKey elects the ground-truth token rotator's leader
// across control planes (S2). Unlike ReaperAdvisoryLockKey (re-tried every
// tick via TryAdvisoryLock, release()d at the end of each one), this key is
// acquired ONCE before the rotator's loop starts and held for the process
// lifetime: only the holder mints/writes the shared token file, and every
// other replica parks on a backoff and retries, taking over automatically
// when the holder's Postgres session ends.
//
// It buys AT MOST ONE STEADY-STATE leader, NOT mutual exclusion. An advisory
// lock dies with its SESSION, not with the process, and the holder never
// re-verifies it: a Postgres restart, a failover, pg_terminate_backend or an
// idle-session timeout releases it under a still-running leader, and a standby
// takes over within one backoff — two rotators, neither aware. Harmless for
// THIS workload only, because every write is an atomic rename of a stateless
// token (cmd/wardynd/gt_rotator.go). Work that needs genuine fencing must not
// reuse this key. Any stable value works, as long as it differs from every
// other key in this file.
const GroundTruthRotatorLockKey int64 = 0x5741524459_475452 // ASCII "WARDYGTR"

// SingleInstanceLockKey is the RUNTIME half of the one-replica safety control
// (the Helm chart's `replicas > 1` render refusal is the other half). wardynd
// takes it ONCE at boot, with TryAdvisoryLock, and holds it for the process
// lifetime: a second instance against the same database refuses to start
// instead of quietly serving.
//
// Why it is a safety control, not modesty: wardynd keeps state per-process that
// a second instance cannot see, the sharpest being the secret-masking registry
// (internal/secretmask) — an in-memory, process-local map that FAILS OPEN.
// Secrets are registered on whichever instance served the run's proxy
// injection; a session recording uploaded to any other instance finds an empty
// snapshot and is persisted VERBATIM, live credentials in cleartext, with a
// `success` audit event. The chart's refusal is render-time only, so
// `kubectl scale`, an HPA, or a non-Helm replica edit defeated it silently.
//
// HONEST CEILING — at most one steady-state instance, not mutual exclusion,
// exactly as GroundTruthRotatorLockKey documents for the same mechanism. An
// advisory lock dies with its SESSION, not with the process, and the holder
// never re-verifies it: a Postgres restart, a failover, pg_terminate_backend or
// an idle-session timeout releases it under a still-running daemon, and the
// next instance to boot takes it — two daemons, neither aware. It closes the
// silent-scale hole; it is not a fence. Any stable value works, as long as it
// differs from every other key in this file.
const SingleInstanceLockKey int64 = 0x5741524459_494E53 // ASCII "WARDYINS"

// SecretRekeyLockKey serializes the `wardynd -rotate-age-key` maintenance mode
// (cmd/wardynd's rotateAgeKeyMode): two concurrent rekeys of the same store
// would each re-encrypt from an old key the other has already replaced, so the
// second is refused rather than queued (TryAdvisoryLock, like
// ReaperAdvisoryLockKey).
//
// HONEST CEILING — this does NOT detect a running wardynd. No wardynd holds a
// process-lifetime lock on THIS key: a serving daemon holds
// SingleInstanceLockKey (a different key), the reaper takes
// ReaperAdvisoryLockKey per tick and releases it, and GroundTruthRotatorLockKey
// is only taken when the rotator is configured — so a serving daemon is
// invisible to this check. "Stop the daemon first" is an operator procedure
// documented in docs/OPERATIONS.md, not something this lock enforces — a live
// daemon holds the OLD identity in memory and would write ciphertext under a key
// the rekey has already retired.
const SecretRekeyLockKey int64 = 0x5741524459_524B59 // ASCII "WARDYRKY"

// BootKeyLockKey serializes the CREATE path of cmd/wardynd's boot keys
// (loadOrCreateSecret) across replicas that boot with -allow-multi-instance.
// Without it two replicas booting at once against an empty store each generate
// a key, each Put, and the loser serves with a key nobody else holds. Taken
// with the BLOCKING AdvisoryLock, like migrateAdvisoryLockKey: the second
// replica must wait and then read the first one's key, not skip. Any stable
// value works, as long as it differs from every other key in this file.
const BootKeyLockKey int64 = 0x5741524459_424B59 // ASCII "WARDYBKY"

// SecretConvertLockKey makes the boot conversion of legacy (v0) secrets rows to
// envelope v1 single-writer (secretstore/pg's ConvertV0). Taken with the
// TRANSACTION-scoped pg_advisory_xact_lock and BLOCKING, like
// migrateAdvisoryLockKey: a second replica booting at the same moment must wait
// and then find nothing left to convert, not skip ahead and read v0 rows.
const SecretConvertLockKey int64 = 0x5741524459_454E56 // ASCII "WARDYENV"

// AuditChainLockKey serializes appends to the audit_events hash chain
// (migration 0047). Unlike every key above it is taken with the TRANSACTION
// -scoped pg_advisory_xact_lock, never the session-scoped form: it is released
// by the commit that makes the new row visible, so the next writer's head read
// cannot miss it, and no code path can leak it by forgetting a release.
// Since 0056_audit_chain_serialize.sql the trigger takes it too, and that is
// what binds writers this package does not know about. 0047 could not: the
// identity default had already assigned seq by the time a BEFORE INSERT trigger
// ran, so two racing writers could take the lock there in the opposite order to
// their seq allocation and invert chain order against seq order. 0056 removes
// that objection by allocating seq inside the trigger, under this lock, so
// position and chain link are decided together.
// Both in-tree insert paths still take it BEFORE their INSERT statement, on the
// inserting transaction (store.InsertAuditEvent and the broker's
// insertAuditEventTx): advisory locks are re-entrant within a transaction, so
// the trigger's acquisition is free for them, and holding it across the whole
// statement is what it always was.
// ponytail: ONE lock for the whole chain, so audit appends are globally
// serialized. That IS the feature (a chain has exactly one head), and audit
// write volume is nowhere near a contention regime. If it ever is, the upgrade
// is per-partition chains with a key per partition, not a finer lock over one.
const AuditChainLockKey int64 = 0x5741524459_434841 // ASCII "WARDYCHA"

// AuditChainLockTimeout bounds how long ANY writer waits for AuditChainLockKey
// before giving up. Since 0056 the trigger takes that lock on every insert into
// audit_events, including inserts from outside this repo, so one transaction
// that inserted an audit row and stayed open holds up every audit write in the
// process - and that is reachable with no Wardyn bug at all: an operator's psql
// session, a seed script, a paused migration tool. AuditSpool.Drain already
// bounds its side of this at 15s per pass ("one idle psql transaction must not
// become a process-wide stall"); the SYNCHRONOUS side had no bound of any kind.
// A request-path audit write took the lock on the raw request context, against a
// server with lock_timeout = 0 and statement_timeout = 0 and an http.Server that
// deliberately sets no WriteTimeout, so it waited forever - pinning a request
// goroutine and a pool connection each time. With pool_max_conns at the
// documented minimum of 3, a handful of stuck audit writes exhausts the pool and
// every other query in the process starts blocking behind them.
//
// Both directions of the choice matter, because a bound on a synchronous path can fail
// either way. Too short and a healthy-but-loaded deployment refuses audit writes
// it could have completed; too long and the request path stalls exactly when the
// database is already in trouble. 5s is chosen against measured shapes rather
// than taste: a legitimate wait here is other audit writers queueing, each
// holding the lock for one nextval, one indexed head read, one sha256 and one
// insert - low single-digit milliseconds - so 5s absorbs a queue in the
// thousands before it ever refuses a write that would have completed. It is also
// deliberately well UNDER the drain's 15s pass bound, so the request path yields
// before the background drain does, which is the right order: the drain is the
// thing built to absorb a backlog.
//
// What happens to the write that loses the race decides whether this is a fix or
// a relocation of the failure, so it is stated here. On the request path, the
// error travels back through spoolingRecorder, which fsyncs the event to the
// local spool and logs AUDIT WRITE FAILED; the drain replays it once the lock
// clears. The event is not dropped - it takes exactly the degraded path C1 built
// for a failed durable write. On the broker's mint transaction the audit insert
// is in the same tx as the credential, so a timeout refuses the MINT: no
// credential is issued that could not be audited, which is the fail-closed
// direction a governance tool wants, and is what that path already did for every
// other audit failure.
//
// lock_timeout rather than a context deadline, verified rather than assumed: it
// fires ONLY on a lock wait, never on a slow-but-progressing statement (so the
// "too short" direction cannot abort work that was making progress), it is
// enforced server-side, SET LOCAL scopes it to the transaction so nothing leaks
// onto a pooled connection, and it reports the distinguishable SQLSTATE 55P03.
// Measured on Postgres 17: it bounds the explicit pg_advisory_xact_lock AND the
// trigger's own acquisition during an ordinary INSERT, so it also covers a
// writer that never takes the lock explicitly.
var AuditChainLockTimeout = 5 * time.Second

// AuditChainLockTimeoutSQL is the statement that applies AuditChainLockTimeout
// to the current transaction. SET takes no bind parameters, so the value is
// formatted in - it is an integer from the variable above, never caller input.
// SET LOCAL, so it reverts at commit or rollback and the pooled connection is
// handed back exactly as it was found.
func AuditChainLockTimeoutSQL() string {
	return fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", AuditChainLockTimeout.Milliseconds())
}

// TryAdvisoryLock takes session-level advisory lock key on a connection borrowed
// from pool WITHOUT waiting, reporting ok=false when another session already
// holds it. Call the returned release (deferred) to unlock and hand the
// connection back — skipping it strands a pooled conn for the life of the
// process. Only for work of BOUNDED duration: the borrowed conn is unavailable
// to everyone else until release — which also means the caller's own queries
// need a SECOND conn, so this requires pool_max_conns >= 2 (a 1-conn pool would
// self-deadlock: the lock holds the only conn while the guarded work blocks on
// Acquire; the reaper's per-tick deadline turns that into a failed tick, not a
// hang, but the lock is still wasted).
func TryAdvisoryLock(ctx context.Context, pool *pgxpool.Pool, key int64) (release func(), ok bool, err error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("db: acquire advisory lock conn: %w", err)
	}
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&got); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("db: try advisory lock: %w", err)
	}
	if !got {
		conn.Release()
		return nil, false, nil
	}
	return func() {
		// Unlock on a background context: ctx is typically cancelled at shutdown,
		// exactly when releasing matters most. Best-effort — the lock also dies
		// with the session when the conn is finally closed.
		conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, key) //nolint:errcheck // best-effort release
		conn.Release()
	}, true, nil
}

// AdvisoryLock is TryAdvisoryLock's BLOCKING sibling: it waits for session-level
// advisory lock key until ctx ends, and returns an error rather than proceeding
// unlocked. Same release contract, and the same pool_max_conns >= 2 need.
func AdvisoryLock(ctx context.Context, pool *pgxpool.Pool, key int64) (release func(), err error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("db: acquire advisory lock conn: %w", err)
	}
	unlock := func() {
		// Background context, as in TryAdvisoryLock; unlocking a lock this
		// session does not hold is a harmless no-op.
		conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, key) //nolint:errcheck // best-effort release
		conn.Release()
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		// pgx can return ctx's error after the server granted the lock, so
		// unlock rather than hand back a connection that may still hold it.
		unlock()
		return nil, fmt.Errorf("db: advisory lock: %w", err)
	}
	return unlock, nil
}
