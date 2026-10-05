// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Postgres-backed tests for `wardynd -audit-split-legacy`: it takes the same two guards -migrate-only does
// (the single-instance lock, and no other client on the database) before it touches the schema, and then
// splits the legacy audit partition on the migrator's one connection. Each test gets its own throwaway
// database (emptyDatabase) because the client check counts every backend on it. Guarded by WARDYN_TEST_PG.

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

func TestAuditSplitLegacy_RefusesWhileTheSingleInstanceLockIsHeld(t *testing.T) {
	dsn := emptyDatabase(t)
	ctx := context.Background()
	holderPool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer holderPool.Close()
	release, err := claimSingleInstance(ctx, holderPool, false)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	defer release()

	err = auditSplitLegacyMode(migrateOnlyFlags(dsn))
	requireExit(t, err, exitMigrateRefused, "split the legacy audit partition", "single-instance lock is held by", "backend pid")
}

func TestAuditSplitLegacy_RefusesWhileASecondConnectionIsOpenAndNoLockIsHeld(t *testing.T) {
	dsn := emptyDatabase(t)
	ctx := context.Background()
	// A replica run with WARDYN_HA holds a connection and no lock.
	other, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer other.Close()

	err = auditSplitLegacyMode(migrateOnlyFlags(dsn))
	requireExit(t, err, exitMigrateRefused, "split the legacy audit partition", "other client connection", "WARDYN_HA")
	// The refusal released its own lock.
	release, err := claimSingleInstance(ctx, other, false)
	if err != nil {
		t.Fatalf("a refused -audit-split-legacy left the lock held: %v", err)
	}
	release()
}

func TestAuditSplitLegacy_NeedsADatabase(t *testing.T) {
	empty, timeout := "", time.Minute
	err := auditSplitLegacyMode(&bootFlags{dsn: &empty, migrateDSN: &empty, migrateTimeout: &timeout})
	if err == nil || exitCodeOf(err) != exitMigrateFailed {
		t.Fatalf("no database configured: exit %d, err %v; want %d", exitCodeOf(err), err, exitMigrateFailed)
	}
}

func TestAuditSplitLegacy_SplitsAConvertedChainAndNamesARefusalWhenThereIsNothingLeftToSplit(t *testing.T) {
	dsn := emptyDatabase(t)
	ctx := context.Background()
	files, err := filepath.Glob(filepath.Join("..", "..", "internal", "db", "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("list migrations: %v (%d files)", err, len(files))
	}
	sort.Strings(files)
	seed, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	apply := func(f string) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := seed.Exec(ctx, string(b)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(f), err)
		}
	}
	converted := false
	for _, f := range files {
		name := filepath.Base(f)
		if !converted && name >= "0111_audit_partitioned.sql" {
			// The 0.8.5 trigger is in place: three rows in two months, the third a replay from the first.
			ago := time.Now().UTC().AddDate(0, -20, 0)
			month := time.Date(ago.Year(), ago.Month(), 1, 12, 0, 0, 0, time.UTC)
			for _, tm := range []time.Time{month.AddDate(0, 0, 4), month.AddDate(0, 1, 2), month.AddDate(0, 0, 19)} {
				if _, err := seed.Exec(ctx, `INSERT INTO audit_events (id, "time", actor_type, actor, action, outcome)
					VALUES (gen_random_uuid(), $1, 'system', 'split-probe', 'split.probe', 'success')`, tm); err != nil {
					t.Fatalf("seed: %v", err)
				}
			}
			converted = true
		}
		apply(f)
	}
	seed.Close()

	// The seeding pool's backends take a moment to leave pg_stat_activity, and until they do the client
	// check rightly reads them as a live writer: retry only that refusal.
	var merr error
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if merr = auditSplitLegacyMode(migrateOnlyFlags(dsn)); exitCodeOf(merr) != exitMigrateRefused {
			break
		}
	}
	if merr != nil {
		t.Fatalf("-audit-split-legacy on a converted chain: %v", merr)
	}

	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	var anchors int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_chain_anchors WHERE kind = 'split'`).Scan(&anchors); err != nil {
		t.Fatalf("count anchors: %v", err)
	}
	if anchors != 2 {
		t.Errorf("%d split anchors, want 2 (January and February)", anchors)
	}
	st, err := store.NewPG(pool).VerifyAuditChain(ctx)
	if err != nil || !st.OK || st.Checked != 3 {
		t.Fatalf("verify after the split: %+v, %v", st, err)
	}
	// The lock is free again afterwards.
	release, err := claimSingleInstance(ctx, pool, false)
	if err != nil {
		t.Fatalf("lock still held after -audit-split-legacy returned: %v", err)
	}
	release()
	pool.Close()

	// A second run finds the legacy partition gone: a named refusal, exit 3, and nothing changed.
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if merr = auditSplitLegacyMode(migrateOnlyFlags(dsn)); !strings.Contains(merr.Error(), "other client connection") {
			break
		}
	}
	requireExit(t, merr, exitMigrateRefused, "split the legacy audit partition", store.SplitNoLegacy)
}
