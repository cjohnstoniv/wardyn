// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"testing"
)

// TestMigrate_RevocationOrderBackfill (0194): cut rows that exist before the migration get a seq in
// revoked_at order, below the sequence's value afterwards, so a credential that records the sequence at
// its issue is not ended by a cut older than itself, and one that records nothing is still compared on
// revoked_at; the trigger draws the seq for every later insert and update; the migration is replay-safe.
func TestMigrate_RevocationOrderBackfill(t *testing.T) {
	pool := pgPoolIsolated(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	// The state a database is in before the migration: cut rows with no seq, no trigger, a sequence never drawn.
	exec(`DROP TRIGGER oidc_session_revocations_stamp ON oidc_session_revocations`)
	exec(`DROP TRIGGER oidc_session_cuts_stamp ON oidc_session_cuts`)
	exec(`ALTER SEQUENCE oidc_revocation_seq RESTART WITH 1`)
	exec(`INSERT INTO oidc_session_revocations (sub, revoked_at, seq) VALUES
		('late', now() - interval '1 hour', 0), ('early', now() - interval '3 hours', 0), ('', now() - interval '2 hours', 0)`)
	exec(`INSERT INTO oidc_session_cuts (sub, cut_at, seq) VALUES ('cut', now() - interval '90 minutes', 0)`)

	sql, err := migrationFS.ReadFile("migrations/0194_revocation_order.sql")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 { // replayed: nothing changes the second time
		exec(string(sql))
		seqOf := func(table, sub string) int64 {
			var n int64
			if err := pool.QueryRow(ctx, `SELECT seq FROM `+table+` WHERE sub = $1`, sub).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		early, global, cut, late := seqOf("oidc_session_revocations", "early"), seqOf("oidc_session_revocations", ""),
			seqOf("oidc_session_cuts", "cut"), seqOf("oidc_session_revocations", "late")
		if early < 1 || global < 1 || cut < 1 || late < 1 {
			t.Fatalf("pass %d: a cut row kept seq 0: early=%d global=%d cut=%d late=%d", i, early, global, cut, late)
		}
		var last int64
		if err := pool.QueryRow(ctx, `SELECT last_value FROM oidc_revocation_seq`).Scan(&last); err != nil {
			t.Fatal(err)
		}
		for _, n := range []int64{early, global, cut, late} {
			if n > last {
				t.Fatalf("pass %d: a backfilled seq %d is above the sequence's value %d, so a credential issued now would be ended by it", i, n, last)
			}
		}
		if i == 0 && !(early < global && global < late) {
			t.Fatalf("backfill is not in revoked_at order: early=%d global=%d late=%d", early, global, late)
		}
	}

	// The trigger draws the seq for an insert that names none and for an update.
	exec(`INSERT INTO oidc_session_revocations (sub, revoked_at) VALUES ('fresh', now())`)
	var first, second, last int64
	if err := pool.QueryRow(ctx, `SELECT seq FROM oidc_session_revocations WHERE sub = 'fresh'`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE oidc_session_revocations SET revoked_at = now() WHERE sub = 'fresh'`)
	if err := pool.QueryRow(ctx, `SELECT seq FROM oidc_session_revocations WHERE sub = 'fresh'`).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT last_value FROM oidc_revocation_seq`).Scan(&last); err != nil {
		t.Fatal(err)
	}
	if second <= first || first <= 0 || second != last {
		t.Fatalf("trigger seqs: insert %d, update %d, sequence %d; want each draw greater than the last", first, second, last)
	}
}
