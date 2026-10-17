// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// auditSplitLegacyMode is `wardynd -audit-split-legacy`: split the one partition that holds all the audit
// history from before 0.8.6 into seq-contiguous ranges retention can drop one at a time, then exit,
// serving nothing (store.SplitLegacyAudit has the method and the proof).
//
// It needs the same quiet database -migrate-only does and takes the same two guards before its first
// statement: db.SingleInstanceLockKey, held on the one connection the split runs on, and no other client
// backend on the database (the lock alone cannot show a replica started with WARDYN_HA).
// It connects with WARDYN_PG_MIGRATE_DSN when set, else WARDYN_PG_DSN, and never falls back from one to
// the other: the app role has no INSERT on the anchors and cannot DETACH or ATTACH a partition, so the
// split refuses (exit 3) on any role that does not own the audit tables. The whole split is one
// transaction, so a failure (exit 1) leaves the log exactly as it was.
func auditSplitLegacyMode(f *bootFlags) error {
	dsn := strings.TrimSpace(*f.migrateDSN)
	if dsn == "" {
		dsn = strings.TrimSpace(*f.dsn)
	}
	if dsn == "" {
		return errors.New("-audit-split-legacy needs a database; set WARDYN_PG_MIGRATE_DSN (or WARDYN_PG_DSN)")
	}
	ctx := context.Background()
	s, err := acquireQuiet(ctx, dsn, 30*time.Second, "split the legacy audit partition")
	if err != nil {
		return err
	}
	defer s.close()

	splitCtx, cancel := context.WithTimeout(ctx, *f.migrateTimeout)
	defer cancel()
	// Pinned READ COMMITTED like every audit writer (a DSN may default to REPEATABLE READ), though no writer is running.
	tx, err := s.conn.BeginTx(splitCtx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return &exitCodeError{code: exitMigrateFailed, err: fmt.Errorf("begin the split: %w", err)}
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // a committed transaction has nothing to undo; an uncommitted one must

	res, err := store.SplitLegacyAudit(splitCtx, tx)
	var refused *store.AuditSplitRefused
	switch {
	case errors.As(err, &refused):
		return refusedTo("split the legacy audit partition", "%s: %s", refused.Reason, refused.Detail)
	case err != nil:
		return &exitCodeError{code: exitMigrateFailed, err: err}
	}
	if err := tx.Commit(splitCtx); err != nil {
		return &exitCodeError{code: exitMigrateFailed, err: fmt.Errorf("commit the split: %w", err)}
	}
	for _, r := range res.Ranges {
		slog.Info("wardynd: audit legacy range", "partition", r.Name, "rows", r.Rows, "seq_lo", r.SeqLo, "seq_hi", r.SeqHi,
			"recorded_lo", r.RecordedLo, "recorded_hi", r.RecordedHi, "digest", r.Digest)
	}
	slog.Info("wardynd: -audit-split-legacy finished; the chain verifies over every row it verified before and nothing was served",
		"ranges", len(res.Ranges), "rows", res.Rows, "checked", res.Checked, "legacy", res.Legacy)
	return nil
}
