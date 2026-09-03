// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

// EQUIVALENCE PIN for paging the verify sweep. The sweep is a VERIFICATION
// path, so the thing that has to survive the change is the verdict, not the
// throughput: a faster sweep that verifies slightly less is strictly worse than
// a slow one, because it reports the same confident answer.
//
// Every case below is run twice against the same database — once through the
// real store.VerifyAuditChain (keyset-paged) and once through a full-scan ORACLE
// that reproduces the single `ORDER BY seq` query the paged version replaced —
// and the two AuditChainStatus values must be identical field for field. The
// page size is shrunk so the cases actually straddle page boundaries; a chain
// walked in one page proves nothing about a chain walked in four.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// fullScanOracle is the pre-paging implementation, verbatim in shape: ONE
// statement, no LIMIT, no cursor, walked to the end. It is the reference the
// paged sweep must agree with.
func fullScanOracle(t *testing.T, pool *pgxpool.Pool) store.AuditChainStatus {
	t.Helper()
	const q = `
		SELECT seq,
		       row_hash IS NULL,
		       prev_hash IS NULL,
		       COALESCE(prev_hash,''),
		       COALESCE(row_hash,''),
		       COALESCE(audit_row_hash(prev_hash, id, time, run_id, actor_type, actor,
		                               action, target, outcome, source_ip, data), '')
		FROM audit_events
		ORDER BY seq`
	rows, err := pool.Query(context.Background(), q)
	if err != nil {
		t.Fatalf("oracle query: %v", err)
	}
	defer rows.Close()

	st := store.AuditChainStatus{OK: true}
	prev := ""
	fail := func(seq int64, reason string) {
		if st.OK {
			st.OK, st.BrokenSeq, st.Reason = false, seq, reason
		}
	}
	for rows.Next() {
		var seq int64
		var unchained, prevIsNull bool
		var p, row, want string
		if err := rows.Scan(&seq, &unchained, &prevIsNull, &p, &row, &want); err != nil {
			t.Fatalf("oracle scan: %v", err)
		}
		if unchained {
			if st.Checked == 0 {
				st.Legacy++
				continue
			}
			fail(seq, "row carries no hash although the chain had already started "+
				"(it was written with the chain trigger dropped, disabled, or bypassed)")
			break
		}
		if st.Checked == 0 {
			st.FirstSeq = seq
		}
		st.Checked++
		st.HeadSeq, st.HeadHash = seq, row
		switch {
		case row != want:
			fail(seq, "row_hash does not match the row's contents (the row was edited after it was written)")
		case prev == "" && !prevIsNull:
			fail(seq, "the first chained row carries a prev_hash (the row it chained to was deleted)")
		case prev != "" && p != prev:
			fail(seq, "prev_hash does not match the preceding row (a row was deleted or reordered)")
		}
		prev = row
		if !st.OK {
			break
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("oracle iterate: %v", err)
	}
	return st
}

// chainProbeDB is a throwaway database per case: these tests tamper with rows
// and disable triggers, which must never touch a shared audit_events.
func chainProbeDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := throwawayDatabase(t)
	if err := db.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("migrate probe database: %v", err)
	}
	return pool
}

func appendN(t *testing.T, pool *pgxpool.Pool, n int, action string) {
	t.Helper()
	for i := 0; i < n; i++ {
		appendChained(t, pool, action)
	}
}

// assertSameVerdict is the whole point: paged and full-scan must agree.
func assertSameVerdict(t *testing.T, pool *pgxpool.Pool, what string) store.AuditChainStatus {
	t.Helper()
	want := fullScanOracle(t, pool)
	got, err := store.NewPG(pool).VerifyAuditChain(context.Background())
	if err != nil {
		t.Fatalf("%s: paged VerifyAuditChain: %v", what, err)
	}
	if got != want {
		t.Fatalf("%s: the paged sweep and the full-scan oracle DISAGREE.\n paged  = %+v\n oracle = %+v\n"+
			"a verification path that returns a different verdict after a performance change has verified "+
			"something other than what it used to", what, got, want)
	}
	return got
}

func TestPG_PagedSweepMatchesTheFullScanOracle(t *testing.T) {
	// Small pages so every case below crosses boundaries.
	restore := store.AuditChainPageSize
	store.AuditChainPageSize = 7
	t.Cleanup(func() { store.AuditChainPageSize = restore })

	t.Run("empty log", func(t *testing.T) {
		pool := chainProbeDB(t)
		st := assertSameVerdict(t, pool, "empty")
		if !st.OK || st.Checked != 0 {
			t.Fatalf("empty log: %+v", st)
		}
	})

	t.Run("chain shorter than one page", func(t *testing.T) {
		pool := chainProbeDB(t)
		appendN(t, pool, 3, "short.chain")
		if st := assertSameVerdict(t, pool, "short"); st.Checked != 3 {
			t.Fatalf("checked %d, want 3", st.Checked)
		}
	})

	t.Run("chain exactly one page", func(t *testing.T) {
		// The off-by-one that a paged walk gets wrong: a full page must not be
		// mistaken for the end, nor re-read.
		pool := chainProbeDB(t)
		appendN(t, pool, int(store.AuditChainPageSize), "exact.page")
		if st := assertSameVerdict(t, pool, "exact page"); st.Checked != int64(store.AuditChainPageSize) {
			t.Fatalf("checked %d, want %d", st.Checked, store.AuditChainPageSize)
		}
	})

	t.Run("chain spanning several pages", func(t *testing.T) {
		pool := chainProbeDB(t)
		appendN(t, pool, int(store.AuditChainPageSize)*3+2, "many.pages")
		st := assertSameVerdict(t, pool, "many pages")
		if !st.OK {
			t.Fatalf("a clean multi-page chain reported broken: %+v", st)
		}
	})
}

// TestPG_PagedSweepStillDetectsABreak is the arm that matters most: a sweep
// that got faster and stopped seeing tampering would report the same confident
// "ok" over an edited log. The break is planted at three positions relative to
// the page boundary, because an off-by-one in the paging would show up at
// exactly one of them.
func TestPG_PagedSweepStillDetectsABreak(t *testing.T) {
	restore := store.AuditChainPageSize
	store.AuditChainPageSize = 7
	t.Cleanup(func() { store.AuditChainPageSize = restore })

	// offsets into a 22-row chain: inside the first page, exactly ON a page
	// boundary (the first row of page 2), and one past it.
	for _, tc := range []struct {
		name  string
		index int
	}{
		{"inside the first page", 2},
		{"the last row of a page", 6},
		{"the first row of the next page", 7},
		{"deep in a later page", 18},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := chainProbeDB(t)
			appendN(t, pool, 22, "tampered.chain")

			var seq int64
			if err := pool.QueryRow(context.Background(),
				`SELECT seq FROM audit_events ORDER BY seq OFFSET $1 LIMIT 1`, tc.index).Scan(&seq); err != nil {
				t.Fatalf("pick row: %v", err)
			}
			// Edit the row's CONTENT, leaving its stored hashes: the "row was
			// edited after it was written" break. Needs the append-only trigger
			// out of the way, which is what an actual tamperer would also need.
			triggersOff(t, pool, func(tx pgx.Tx) error {
				_, err := tx.Exec(context.Background(),
					`UPDATE audit_events SET actor = 'tampered@corp.example' WHERE seq = $1`, seq)
				return err
			})

			st := assertSameVerdict(t, pool, tc.name)
			if st.OK {
				t.Fatalf("the sweep reported a tampered chain as OK: %+v", st)
			}
			if st.BrokenSeq != seq {
				t.Errorf("BrokenSeq = %d, want the edited row %d", st.BrokenSeq, seq)
			}
		})
	}
}

// TestPG_PagedSweepStillSeesAHashlessRowInPlace pins rule 3 across the page
// boundary. "Legacy" versus "written with the chain trigger off" is decided by
// WHERE the hashless row sits relative to the first chained row, so a paged walk
// that lost its place would reclassify a break as a legacy row — the quietest
// possible regression, since legacy rows are reported as normal.
func TestPG_PagedSweepStillSeesAHashlessRowInPlace(t *testing.T) {
	restore := store.AuditChainPageSize
	store.AuditChainPageSize = 5
	t.Cleanup(func() { store.AuditChainPageSize = restore })

	pool := chainProbeDB(t)
	appendN(t, pool, 12, "rule3.chain")
	var seq int64
	if err := pool.QueryRow(context.Background(),
		`SELECT seq FROM audit_events ORDER BY seq OFFSET 8 LIMIT 1`).Scan(&seq); err != nil {
		t.Fatalf("pick row: %v", err)
	}
	triggersOff(t, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(),
			`UPDATE audit_events SET row_hash = NULL, prev_hash = NULL WHERE seq = $1`, seq)
		return err
	})

	st := assertSameVerdict(t, pool, "hashless row above the chain start")
	if st.OK {
		t.Fatalf("a hashless row ABOVE the first chained row was not reported as a break: %+v", st)
	}
	if st.BrokenSeq != seq {
		t.Errorf("BrokenSeq = %d, want %d", st.BrokenSeq, seq)
	}
	if st.Legacy != 0 {
		t.Errorf("Legacy = %d; a hashless row after the chain started must never be counted as legacy", st.Legacy)
	}
}
