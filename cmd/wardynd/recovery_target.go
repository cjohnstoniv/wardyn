// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

// The target-selection chain of `wardynd -recovery-rehearse` (docs/design/0.9/recovery-set.md §5).
// Every check here refuses with exitMigrateRefused before a row of the target is read. They defend
// against mistakes: an operator who holds both DSNs and wants to fool them can (residual (i)).

const (
	rehearseVerb       = "rehearse"
	rehearseConnectTTL = 30 * time.Second
)

// dsnIdentity is where a DSN points, without the user, the password or any parameter.
type dsnIdentity struct {
	Host     string
	Port     uint16
	Database string
}

func (d dsnIdentity) String() string { return fmt.Sprintf("%s:%d/%s", d.Host, d.Port, d.Database) }

// parseDSNIdentity is the host, port and database of dsn after pgconn's own parse, host lowercased,
// and whether it names more than one host: pgconn would try the others when the first does not
// answer. The TLS fallback of sslmode=prefer repeats the same host and port, so only a different
// pair counts.
func parseDSNIdentity(dsn string) (id dsnIdentity, multiHost bool, err error) {
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return dsnIdentity{}, false, fmt.Errorf("parse the connection string: %w", err)
	}
	id = dsnIdentity{Host: strings.ToLower(cfg.Host), Port: cfg.Port, Database: cfg.Database}
	multiHost = slices.ContainsFunc(cfg.Fallbacks, func(fb *pgconn.FallbackConfig) bool {
		return strings.ToLower(fb.Host) != id.Host || fb.Port != id.Port
	})
	return id, multiHost, nil
}

// holdLiveFunc takes a transaction-scoped advisory lock on key on live and reports whether live itself
// shows the hold. release ends the transaction, and with it the lock.
type holdLiveFunc func(ctx context.Context, live *pgxpool.Pool, key int64) (confirmed bool, release func(), err error)

// holdLiveLock is the live half of T3. The lock is transaction-scoped, so it cannot outlive release
// even through a pooler that keeps the server connection, and the probe touches no row.
func holdLiveLock(ctx context.Context, live *pgxpool.Pool, key int64) (bool, func(), error) {
	tx, err := live.Begin(ctx)
	if err != nil {
		return false, func() {}, fmt.Errorf("begin the live probe: %w", err)
	}
	release := func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, key); err != nil {
		return false, release, fmt.Errorf("take the live probe lock: %w", err)
	}
	var held bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks
		WHERE locktype = 'advisory' AND granted AND pid = pg_backend_pid() AND classid = $1 AND objid = $2 AND objsubid = 1)`,
		uint32(uint64(key)>>32), uint32(key)).Scan(&held)
	if err != nil {
		return false, release, fmt.Errorf("confirm the live probe lock: %w", err)
	}
	return held, release, nil
}

// targetChecks is the input of T0 to T7: what the target must differ from, and what it must match.
type targetChecks struct {
	liveDSN, targetDSN string
	// source is the manifest's source.db; migration its newest migration.
	source    dsnIdentity
	migration string
	setDir    string
	// protected are the live directories the set must not overlap (T6).
	protected []string
	holdLive  holdLiveFunc
}

// rehearseSession is what the chain leaves open: the one target backend and the live pool the audit
// rows go through.
type rehearseSession struct {
	target *migrateOnlySession
	live   *pgxpool.Pool
	id     dsnIdentity
}

func (s *rehearseSession) close() {
	s.live.Close()
	s.target.close()
}

// conn is the one target backend. Nothing in a rehearsal opens another.
func (s *rehearseSession) conn() *pgxpool.Conn { return s.target.conn }

// readTx begins a read-only transaction on the target backend.
func (s *rehearseSession) readTx(ctx context.Context, iso pgx.TxIsoLevel) (pgx.Tx, error) {
	return s.conn().BeginTx(ctx, pgx.TxOptions{IsoLevel: iso, AccessMode: pgx.ReadOnly})
}

// run is T0, T1, T2, T5, T3, T4, T6, T7 in that order; the first refusal stops it. T5 comes before T3
// because it opens the one target backend that T3, T4 and T7 then ask. On success the session is open.
func (c targetChecks) run(ctx context.Context) (*rehearseSession, error) {
	id, multiHost, err := parseDSNIdentity(c.targetDSN)
	if err != nil {
		return nil, fmt.Errorf("WARDYN_REHEARSE_PG_DSN: %w", err)
	}
	if multiHost { // T0
		return nil, refusedTo(rehearseVerb, "T0 one host: the target names more than one host, so a fallback could be a database no check saw. Name exactly one")
	}
	liveID, _, err := parseDSNIdentity(c.liveDSN)
	if err != nil {
		return nil, fmt.Errorf("WARDYN_PG_DSN: %w", err)
	}
	if id == liveID || id == c.source { // T1
		return nil, refusedTo(rehearseVerb, "T1 static compare: the target %s is the live database or the one the set was taken from (%s, %s)", id, liveID, c.source)
	}
	if err := liveReachable(ctx, c.liveDSN); err != nil { // T2
		return nil, err
	}
	quiet, err := acquireQuiet(ctx, c.targetDSN, rehearseConnectTTL, rehearseVerb) // T5
	if err != nil {
		return nil, fmt.Errorf("T5 quiet target: %w", err)
	}
	s := &rehearseSession{target: quiet, id: id}
	if err := s.afterQuiet(ctx, c); err != nil {
		if s.live != nil {
			s.live.Close()
		}
		quiet.close()
		return nil, err
	}
	return s, nil
}

// afterQuiet is T3, T4, T6 and T7 on the backend T5 opened.
func (s *rehearseSession) afterQuiet(ctx context.Context, c targetChecks) error {
	// default_transaction_read_only makes every statement this backend runs without a transaction of
	// its own read-only too; the transactions below say READ ONLY themselves.
	if _, err := s.conn().Exec(ctx, `SET default_transaction_read_only = on`); err != nil {
		return fmt.Errorf("set the target read-only: %w", err)
	}
	if err := s.lockVisibility(ctx, c); err != nil { // T3
		return err
	}
	var standby bool
	if err := s.conn().QueryRow(ctx, `SELECT pg_is_in_recovery()`).Scan(&standby); err != nil {
		return fmt.Errorf("T4 standby: %w", err)
	}
	if standby {
		return refusedTo(rehearseVerb, "T4 standby: the target is a standby in recovery; a streaming replica of live passes T3 and would verify live, not the set")
	}
	if err := checkSetOverlap(c.setDir, c.protected); err != nil { // T6
		return err
	}
	return s.checkVersion(ctx, c.migration) // T7
}

// liveReachable is T2: the live database answers. The connection is closed again, so that T5 finds no
// client of ours on a target that is in fact live.
func liveReachable(ctx context.Context, dsn string) error {
	cctx, cancel := context.WithTimeout(ctx, rehearseConnectTTL)
	defer cancel()
	pool, err := db.Connect(cctx, dsn)
	if err != nil {
		return refusedTo(rehearseVerb, "T2 live reachable: WARDYN_PG_DSN does not connect (%v). T3 needs live: run the rehearsal while the deployment is up", err)
	}
	pool.Close()
	return nil
}

// lockVisibility is T3. Live takes a transaction-scoped advisory lock on a random key and confirms it
// holds it; the target must NOT see that key in pg_locks for its own database. Advisory locks are per
// database, so seeing it proves the target is live's database, through any name, address, forwarded
// port or pooler. A live that cannot confirm its own hold refuses: a lock that never landed would
// read as "not seen".
func (s *rehearseSession) lockVisibility(ctx context.Context, c targetChecks) error {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Errorf("T3 lock visibility: draw the probe key: %w", err)
	}
	key := int64(binary.BigEndian.Uint64(raw[:]))
	cctx, cancel := context.WithTimeout(ctx, rehearseConnectTTL)
	defer cancel()
	live, err := db.Connect(cctx, c.liveDSN)
	if err != nil {
		return refusedTo(rehearseVerb, "T3 lock visibility: live does not connect (%v)", err)
	}
	s.live = live
	hold := c.holdLive
	if hold == nil {
		hold = holdLiveLock
	}
	confirmed, release, err := hold(cctx, live, key)
	defer release()
	if err != nil {
		return refusedTo(rehearseVerb, "T3 lock visibility: the live probe failed: %v", err)
	}
	if !confirmed {
		return refusedTo(rehearseVerb, "T3 lock visibility: live did not confirm its own probe lock, so \"the target does not see it\" would prove nothing")
	}
	var seen bool
	err = s.conn().QueryRow(cctx, `SELECT EXISTS (SELECT 1 FROM pg_locks
		WHERE locktype = 'advisory' AND granted AND classid = $1 AND objid = $2 AND objsubid = 1
		  AND database = (SELECT oid FROM pg_database WHERE datname = current_database()))`,
		uint32(uint64(key)>>32), uint32(key)).Scan(&seen)
	if err != nil {
		return fmt.Errorf("T3 lock visibility: ask the target: %w", err)
	}
	if seen {
		return refusedTo(rehearseVerb, "T3 lock visibility: the target sees a lock that live holds, so it is the live database under another name. Point WARDYN_REHEARSE_PG_DSN at the restored copy")
	}
	return nil
}

// checkVersion is T7: the target's newest migration, the manifest's and this binary's must be one.
func (s *rehearseSession) checkVersion(ctx context.Context, manifest string) error {
	var target string
	if err := s.conn().QueryRow(ctx, `SELECT COALESCE(max(filename COLLATE "C"), '') FROM schema_migrations`).Scan(&target); err != nil {
		return fmt.Errorf("T7 version: read the target's migrations: %w", err)
	}
	binary, err := db.NewestMigration()
	if err != nil {
		return err
	}
	if target != manifest || target != binary {
		return refusedTo(rehearseVerb, "T7 version: newest migration is %q on the target, %q in the manifest and %q in this binary. "+
			"A read-only rehearsal cannot migrate the target: run the release that took the set", target, manifest, binary)
	}
	return nil
}

// checkSetOverlap is T6: the set directory must not be, contain or sit inside any live directory,
// compared by os.SameFile along every ancestor so a bind mount or symlink alias is caught. A live
// directory that does not exist here is skipped.
func checkSetOverlap(setDir string, live []string) error {
	set, err := filepath.Abs(setDir)
	if err != nil {
		return fmt.Errorf("T6 set overlap: %w", err)
	}
	for _, p := range live {
		if p == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return fmt.Errorf("T6 set overlap: %w", err)
		}
		if ancestorIsSame(set, abs) || ancestorIsSame(abs, set) {
			return refusedTo(rehearseVerb, "T6 set overlap: the set directory overlaps %s, which a live deployment uses. Restore the set into a directory of its own", p)
		}
	}
	return nil
}

// ancestorIsSame reports whether path or any directory above it is the same file as other.
func ancestorIsSame(path, other string) bool {
	want, err := os.Stat(other)
	if err != nil {
		return false
	}
	for p := path; ; p = filepath.Dir(p) {
		if fi, err := os.Stat(p); err == nil && os.SameFile(fi, want) {
			return true
		}
		if filepath.Dir(p) == p {
			return false
		}
	}
}

// protectedDirs are the live directories T6 compares the set with: the audit spool's directory, the
// recording directory and every host-path drive root.
func protectedDirs(f *bootFlags) []string {
	dirs := splitCSV(*f.userDriveHostRoots)
	if s := strings.TrimSpace(*f.auditSpool); s != "" {
		dirs = append(dirs, filepath.Dir(s))
	}
	return append(dirs, strings.TrimSpace(*f.recordingDir))
}
