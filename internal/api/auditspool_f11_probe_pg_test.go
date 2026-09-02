// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// F11 PROBE — destination: internal/api/auditspool_f11_probe_pg_test.go
//
// The spool → drain → store hop of the append-only claim: a spooled line can
// carry ANY prev_hash/row_hash (the JSON tags exist on types.AuditEvent) and
// even a "seq", yet what lands in audit_events must be chained by Postgres at
// the CURRENT head — the INSERT statement never names those columns and the
// 0047 BEFORE INSERT trigger overwrites them unconditionally. Guarded by
// WARDYN_TEST_PG (db.Connect + db.Migrate, the pgHarness convention).
//
// Expected result on feat/v0.7-profiles @ fa910735: GREEN (pins the claim).
package api

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

func TestPG_ProbeF11_SpoolReplayCannotForgeChain(t *testing.T) {
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping Postgres-backed spool replay probe")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	path := filepath.Join(t.TempDir(), "audit-spool.jsonl")
	sp, err := NewAuditSpool(path)
	if err != nil {
		t.Fatalf("NewAuditSpool: %v", err)
	}

	const forgedPrev = "1111111111111111111111111111111111111111111111111111111111111111"
	const forgedRow = "2222222222222222222222222222222222222222222222222222222222222222"

	// Two events spooled through the real Append path, hashes pre-filled as if
	// a writer (or a host-level editor of the file) tried to pick them.
	ev1 := newTestEvent("test.chain.spool")
	ev1.Time = time.Now().UTC()
	ev1.PrevHash, ev1.RowHash = forgedPrev, forgedRow
	ev2 := newTestEvent("test.chain.spool")
	ev2.Time = time.Now().UTC()
	ev2.PrevHash, ev2.RowHash = forgedRow, forgedPrev
	if err := sp.Append(ev1); err != nil {
		t.Fatalf("append ev1: %v", err)
	}
	if err := sp.Append(ev2); err != nil {
		t.Fatalf("append ev2: %v", err)
	}

	// A hand-written line that ALSO tries to choose its seq (the identity PK)
	// and carries forged hashes — the shape a host-level attacker would write.
	handID := uuid.New()
	line := fmt.Sprintf(`{"id":%q,"time":%q,"actor_type":"system","actor":"f11-handwritten","action":"test.chain.spool",`+
		`"outcome":"success","seq":1,"prev_hash":%q,"row_hash":%q}`+"\n",
		handID, time.Now().UTC().Format(time.RFC3339Nano), forgedPrev, forgedRow)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open spool for append: %v", err)
	}
	if _, err := f.WriteString(line); err != nil {
		t.Fatalf("write handwritten line: %v", err)
	}
	_ = f.Close()

	n, err := sp.Drain(ctx, store.Recorder{Pool: pool}, 100)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if n != 3 {
		t.Fatalf("drained %d events, want 3", n)
	}
	if got := spoolLineCount(t, path); got != 0 {
		t.Errorf("spool still holds %d lines after a full drain", got)
	}

	type stored struct {
		seq       int64
		prev, row string
	}
	read := func(id uuid.UUID) stored {
		t.Helper()
		var s stored
		if err := pool.QueryRow(ctx,
			`SELECT seq, COALESCE(prev_hash,''), COALESCE(row_hash,'') FROM audit_events WHERE id = $1 ORDER BY seq DESC LIMIT 1`, id,
		).Scan(&s.seq, &s.prev, &s.row); err != nil {
			t.Fatalf("read replayed row %s: %v", id, err)
		}
		return s
	}
	r1, r2, r3 := read(ev1.ID), read(ev2.ID), read(handID)
	for _, c := range []struct {
		name string
		s    stored
	}{{"ev1", r1}, {"ev2", r2}, {"handwritten", r3}} {
		if c.s.row == "" || c.s.row == forgedRow || c.s.row == forgedPrev {
			t.Errorf("%s: stored row_hash = %q — the replayed line chose its own hash (want a Postgres-computed one)", c.name, c.s.row)
		}
		if c.s.prev == forgedPrev || c.s.prev == forgedRow {
			t.Errorf("%s: stored prev_hash = %q — the replayed line chose its own predecessor", c.name, c.s.prev)
		}
		if c.s.seq == 1 {
			t.Errorf("%s: stored seq = 1 — the replayed line chose its own seq", c.name)
		}
	}
	// FIFO replay chains in file order, each row to the one drained before it.
	if r2.prev != r1.row {
		t.Errorf("ev2.prev_hash = %s, want ev1.row_hash %s (replay must chain in file order)", r2.prev, r1.row)
	}
	if r3.prev != r2.row {
		t.Errorf("handwritten.prev_hash = %s, want ev2.row_hash %s", r3.prev, r2.row)
	}
	if r1.seq >= r2.seq || r2.seq >= r3.seq {
		t.Errorf("replay seq order = %d,%d,%d — want strictly increasing in file order", r1.seq, r2.seq, r3.seq)
	}

	st, err := store.NewPG(pool).VerifyAuditChain(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !st.OK {
		t.Fatalf("chain broken after a spool replay: seq=%d %s", st.BrokenSeq, st.Reason)
	}
}
