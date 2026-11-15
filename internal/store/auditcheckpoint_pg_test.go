// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/auditreceipt"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// receiptRow is a stored row in the shape a receipt carries it.
func receiptRow(r store.AuditPartitionRow) auditreceipt.Row {
	target, sourceIP := r.Target, r.SourceIP
	return auditreceipt.Row{Seq: r.Seq, ID: r.ID, TimeUS: r.TimeUS, RunID: r.RunID, ActorType: r.ActorType, Actor: r.Actor,
		Action: r.Action, Target: &target, Outcome: r.Outcome, SourceIP: &sourceIP, Data: r.Data, PrevHash: r.PrevHash,
		RowHash: r.RowHash}
}

// TestRowHash_MatchesPostgres pins the Go port to audit_row_hash: rows written through the real path, covering
// every escape class, re-hash in Go to their stored row_hash; and the function itself, called with NULL text
// columns the table cannot hold, agrees with the port's bare-null rendering.
func TestRowHash_MatchesPostgres(t *testing.T) {
	pool := chainProbeDB(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	run := uuid.New()
	var ids []uuid.UUID
	for i, ev := range []types.AuditEvent{
		{RunID: &run, ActorType: types.ActorHuman, Actor: `quote " back \ slash`, Action: "test.rowhash", Target: "l1\nl2\tt\r\b\f",
			Outcome: "success", SourceIP: "\x01\x1f\x7f", Data: json.RawMessage(`{"z": "a\"b\u0001 ", "a": [1, 2.50, null], "u": "é<>&"}`)},
		{ActorType: types.ActorSystem, Actor: "José 日本   <>&", Action: "test.rowhash", Outcome: "denied"},
		{ActorType: types.ActorAgent, Actor: "", Action: "test.rowhash", Outcome: "failure", Data: json.RawMessage(`null`)},
		{ActorType: types.ActorSystem, Actor: "s", Action: "test.rowhash", Outcome: "success", Data: json.RawMessage(`"just a string"`),
			Time: time.Date(1969, 12, 31, 23, 59, 59, 999000, time.UTC)},
	} {
		ev.ID = uuid.New()
		if ev.Time.IsZero() {
			ev.Time = time.Now().UTC().Add(time.Duration(i) * time.Microsecond)
		}
		if err := store.InsertAuditEvent(ctx, pool, &ev); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
		ids = append(ids, ev.ID)
	}
	rows, err := pg.AuditReceiptRows(ctx, ids, nil, 0, 10)
	if err != nil || len(rows) != len(ids) {
		t.Fatalf("AuditReceiptRows = %d rows, %v; want %d", len(rows), err, len(ids))
	}
	for _, r := range rows {
		if r.RowHash == nil {
			t.Fatalf("row %d has no row_hash", r.Seq)
		}
		if got := auditreceipt.RowHash(receiptRow(r)); got != *r.RowHash {
			t.Errorf("seq %d: Go RowHash %s, Postgres %s (input %q)", r.Seq, got, *r.RowHash, auditreceipt.HashInput(receiptRow(r)))
		}
	}

	// NULL target and source_ip, nil run_id, SQL NULL data and a prev, straight through the function.
	prev := strings.Repeat("ab", 32)
	row := auditreceipt.Row{ID: uuid.New(), TimeUS: 1700000000123456, ActorType: "system", Actor: "a", Action: "b",
		Outcome: "success", PrevHash: &prev}
	var want string
	if err := pool.QueryRow(ctx, `SELECT audit_row_hash($1, $2, $3, NULL, $4, $5, $6, NULL, $7, NULL, NULL)`,
		prev, row.ID, time.UnixMicro(row.TimeUS).UTC(), row.ActorType, row.Actor, row.Action, row.Outcome).Scan(&want); err != nil {
		t.Fatalf("audit_row_hash: %v", err)
	}
	if got := auditreceipt.RowHash(row); got != want {
		t.Fatalf("NULL columns: Go RowHash %s, Postgres %s (input %q)", got, want, auditreceipt.HashInput(row))
	}
}

// TestAuditCheckpointRange_SeededFromPrevHead: the range is the rows after the previous checkpoint's head, up to
// the high-water mark, capped at max, each with its stored row_hash; a seed that is not the head's hash refuses.
func TestAuditCheckpointRange_SeededFromPrevHead(t *testing.T) {
	pool := chainProbeDB(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	evs := make([]types.AuditEvent, 5)
	for i := range evs {
		evs[i] = appendChained(t, pool, "checkpoint.range")
	}
	headSeq := auditSeq(t, pool, evs[1].ID)

	rows, hw, err := pg.AuditCheckpointRange(ctx, headSeq, evs[1].RowHash, store.AuditCheckpointMaxRows)
	if err != nil {
		t.Fatalf("range: %v", err)
	}
	if hw != auditSeq(t, pool, evs[4].ID) {
		t.Errorf("high-water = %d, want the newest row's seq %d", hw, auditSeq(t, pool, evs[4].ID))
	}
	if len(rows) != 3 {
		t.Fatalf("range holds %d rows, want the 3 after the head", len(rows))
	}
	for i, r := range rows {
		if r.Seq != auditSeq(t, pool, evs[i+2].ID) || r.RowHash != evs[i+2].RowHash {
			t.Errorf("row %d = %+v, want seq %d hash %s", i, r, auditSeq(t, pool, evs[i+2].ID), evs[i+2].RowHash)
		}
	}

	if rows, _, err := pg.AuditCheckpointRange(ctx, headSeq, evs[1].RowHash, 2); err != nil || len(rows) != 2 {
		t.Fatalf("capped range = %d rows, %v; want 2", len(rows), err)
	}
	if _, _, err := pg.AuditCheckpointRange(ctx, headSeq, evs[1].RowHash, store.AuditCheckpointMaxRows+1); err == nil {
		t.Fatal("a cap above the per-checkpoint maximum was accepted")
	}

	_, _, err = pg.AuditCheckpointRange(ctx, headSeq, evs[0].RowHash, store.AuditCheckpointMaxRows)
	var brk *store.AuditCheckpointBreak
	if !errors.As(err, &brk) || brk.Seq != auditSeq(t, pool, evs[2].ID) {
		t.Fatalf("a seed that is not the head's hash: %v, want a break at seq %d", err, auditSeq(t, pool, evs[2].ID))
	}

	if seq, hash, err := pg.AuditChainHead(ctx); err != nil || seq != hw || hash != evs[4].RowHash {
		t.Fatalf("AuditChainHead = %d %s %v, want the newest row %d %s", seq, hash, err, hw, evs[4].RowHash)
	}

	// Nothing new: an empty range, no error.
	if rows, _, err := pg.AuditCheckpointRange(ctx, hw, evs[4].RowHash, store.AuditCheckpointMaxRows); err != nil || len(rows) != 0 {
		t.Fatalf("range past the head = %d rows, %v; want none", len(rows), err)
	}
}

// TestAuditCheckpointRange_RefusesEditedRow: a row edited after it was written (here by a table owner with the
// append-only guard off) breaks the range at that row, and nothing is returned to sign.
func TestAuditCheckpointRange_RefusesEditedRow(t *testing.T) {
	pool := chainProbeDB(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	evs := make([]types.AuditEvent, 4)
	for i := range evs {
		evs[i] = appendChained(t, pool, "checkpoint.edit")
	}
	edited := auditSeq(t, pool, evs[2].ID)
	tamperAudit(t, pool, `UPDATE audit_events SET target = 'rewritten' WHERE seq = $1`, edited)

	rows, _, err := pg.AuditCheckpointRange(ctx, auditSeq(t, pool, evs[0].ID), evs[0].RowHash, store.AuditCheckpointMaxRows)
	var brk *store.AuditCheckpointBreak
	if !errors.As(err, &brk) || brk.Seq != edited || rows != nil {
		t.Fatalf("an edited row: rows %v, err %v; want no rows and a break at seq %d", rows, err, edited)
	}
}

// TestAuditCheckpointRange_RefusesHashlessAboveStart: a row with no row_hash after the checkpoints began (the
// chain trigger was off when it was written) breaks the range; it is never skipped as legacy.
func TestAuditCheckpointRange_RefusesHashlessAboveStart(t *testing.T) {
	pool := chainProbeDB(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	evs := make([]types.AuditEvent, 4)
	for i := range evs {
		evs[i] = appendChained(t, pool, "checkpoint.hashless")
	}
	hashless := auditSeq(t, pool, evs[2].ID)
	triggersOff(t, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE audit_events SET row_hash = NULL, prev_hash = NULL WHERE seq = $1`, hashless)
		return err
	})

	_, _, err := pg.AuditCheckpointRange(ctx, auditSeq(t, pool, evs[1].ID), evs[1].RowHash, store.AuditCheckpointMaxRows)
	var brk *store.AuditCheckpointBreak
	if !errors.As(err, &brk) || brk.Seq != hashless {
		t.Fatalf("a hashless row above the start: %v, want a break at seq %d", err, hashless)
	}
}

// TestAuditCheckpointRange_ResumesPastAnAttestedDrop: the last checkpoint ended at the fourth legacy row and
// signing stopped there; an attested drop then removed the legacy partition, the unsigned fifth row with it.
// The range from that head resumes at the first retained row, which chains to the drop's recorded tail. A
// head at the drop's last row is still held to its own hash, a head above the drop reads on from its own hash,
// and a partition removed after it without an attested drop is a break: the resume never skips an unattested
// gap.
func TestAuditCheckpointRange_ResumesPastAnAttestedDrop(t *testing.T) {
	c := newRetChain(t)
	ctx := context.Background()
	pg := store.NewPG(c.pool)
	head := c.preSeqs[3]
	headHash := scalar[string](t, c.pool, `SELECT row_hash FROM audit_events WHERE seq = $1`, head)
	if _, err := c.drop("audit_events_legacy", c.digest(t, "audit_events_legacy"), "ops@example.com"); err != nil {
		t.Fatalf("attested drop: %v", err)
	}
	first := scalar[int64](t, c.pool, `SELECT min(seq) FROM audit_events`)
	rows, _, err := pg.AuditCheckpointRange(ctx, head, headHash, store.AuditCheckpointMaxRows)
	if err != nil || len(rows) == 0 || rows[0].Seq != first {
		t.Fatalf("range after the drop = %v, %v; want the retained rows from seq %d", rows, err, first)
	}
	var brk *store.AuditCheckpointBreak
	if _, _, err := pg.AuditCheckpointRange(ctx, c.preSeqs[4], headHash, store.AuditCheckpointMaxRows); !errors.As(err, &brk) || brk.Seq != first {
		t.Fatalf("a head at the drop's last row with another row's hash: %v, want a break at seq %d", err, first)
	}
	// A head above the drop is still held to its own hash: the drop's tail seeds the walk only for a head below it.
	above := scalar[int64](t, c.pool, `SELECT max(seq) FROM `+retMid)
	aboveHash := scalar[string](t, c.pool, `SELECT row_hash FROM audit_events WHERE seq = $1`, above)
	if rows, _, err := pg.AuditCheckpointRange(ctx, above, aboveHash, store.AuditCheckpointMaxRows); err != nil || len(rows) == 0 || rows[0].Seq <= above {
		t.Fatalf("a head above the drop = %v, %v; want the rows after seq %d", rows, err, above)
	}

	c.exec(t, `ALTER TABLE audit_events DETACH PARTITION `+retMid)
	c.exec(t, `DROP TABLE `+retMid)
	_, _, err = pg.AuditCheckpointRange(ctx, head, headHash, store.AuditCheckpointMaxRows)
	if !errors.As(err, &brk) || !strings.Contains(brk.Reason, "rows removed without an attested retention drop") {
		t.Fatalf("range after a partition was removed without an attested drop: %v, want a break", err)
	}
}

// TestAuditCheckpoints_InsertReadAndConflict: a checkpoint is stored verbatim and read back in seq order; a
// second insert over the same range is refused with the stored row, so the signer can check whose it is.
func TestAuditCheckpoints_InsertReadAndConflict(t *testing.T) {
	pool := chainProbeDB(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	sig := func(b byte) []byte { s := make([]byte, 64); s[0] = b; return s }

	if _, found, err := pg.LatestAuditCheckpoint(ctx); err != nil || found {
		t.Fatalf("latest on an empty table: found=%v err=%v", found, err)
	}
	c1 := store.AuditCheckpoint{SeqLo: 1, SeqHi: 10, Kid: "k1", Statement: `{"a":1}`, Signature: sig(1)}
	c2 := store.AuditCheckpoint{SeqLo: 11, SeqHi: 20, Kid: "k1", Statement: `{"a":2}`, Signature: sig(2)}
	for _, c := range []store.AuditCheckpoint{c1, c2} {
		if _, err := pg.InsertAuditCheckpoint(ctx, c); err != nil {
			t.Fatalf("insert %d-%d: %v", c.SeqLo, c.SeqHi, err)
		}
	}
	latest, found, err := pg.LatestAuditCheckpoint(ctx)
	if err != nil || !found || latest.SeqHi != 20 || latest.Statement != c2.Statement {
		t.Fatalf("latest = %+v found=%v err=%v, want the 11-20 checkpoint", latest, found, err)
	}
	all, err := pg.AuditCheckpoints(ctx, 0, 10)
	if err != nil || len(all) != 2 || all[0].SeqLo != 1 || all[1].SeqLo != 11 || string(all[1].Signature) != string(c2.Signature) {
		t.Fatalf("AuditCheckpoints(0) = %+v, %v", all, err)
	}
	if after, err := pg.AuditCheckpoints(ctx, 10, 10); err != nil || len(after) != 1 || after[0].SeqLo != 11 {
		t.Fatalf("AuditCheckpoints(after 10) = %+v, %v; want only 11-20", after, err)
	}

	foreign := store.AuditCheckpoint{SeqLo: 11, SeqHi: 15, Kid: "k9", Statement: `{"x":1}`, Signature: sig(9)}
	existing, err := pg.InsertAuditCheckpoint(ctx, foreign)
	if !errors.Is(err, store.ErrAuditCheckpointExists) || existing.Statement != c2.Statement {
		t.Fatalf("a conflicting insert: existing %+v, err %v; want ErrAuditCheckpointExists with the stored row", existing, err)
	}
}

// TestAuditCheckpoints_OverlapIsAConflict: a range strictly inside, or straddling, a stored checkpoint shares
// neither its seq_lo nor its seq_hi, and is still refused as a conflict that returns the stored row.
func TestAuditCheckpoints_OverlapIsAConflict(t *testing.T) {
	pool := chainProbeDB(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	sig := func(b byte) []byte { s := make([]byte, 64); s[0] = b; return s }

	stored := store.AuditCheckpoint{SeqLo: 100, SeqHi: 200, Kid: "k1", Statement: `{"a":1}`, Signature: sig(1)}
	if _, err := pg.InsertAuditCheckpoint(ctx, stored); err != nil {
		t.Fatalf("insert 100-200: %v", err)
	}
	for _, r := range [][2]int64{{120, 150}, {150, 250}, {50, 100}, {200, 300}} {
		foreign := store.AuditCheckpoint{SeqLo: r[0], SeqHi: r[1], Kid: "k9", Statement: `{"x":1}`, Signature: sig(9)}
		existing, err := pg.InsertAuditCheckpoint(ctx, foreign)
		if !errors.Is(err, store.ErrAuditCheckpointExists) || existing.Statement != stored.Statement {
			t.Fatalf("insert %d-%d over 100-200: existing %+v, err %v; want ErrAuditCheckpointExists with the stored row",
				r[0], r[1], existing, err)
		}
	}
	all, err := pg.AuditCheckpoints(ctx, 0, 10)
	if err != nil || len(all) != 1 {
		t.Fatalf("after overlapping inserts: %+v, %v; want only the stored checkpoint", all, err)
	}
	if _, err := pg.InsertAuditCheckpoint(ctx, store.AuditCheckpoint{SeqLo: 201, SeqHi: 300, Kid: "k1",
		Statement: `{"a":2}`, Signature: sig(2)}); err != nil {
		t.Fatalf("an adjacent range 201-300: %v", err)
	}
}

// TestAuditCheckpointRowHashes: a checkpoint's hash list is the seq and stored row_hash of every row in its
// range, in seq order.
func TestAuditCheckpointRowHashes(t *testing.T) {
	pool := chainProbeDB(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	evs := make([]types.AuditEvent, 3)
	for i := range evs {
		evs[i] = appendChained(t, pool, "checkpoint.hashes")
	}
	got, err := pg.AuditCheckpointRowHashes(ctx, auditSeq(t, pool, evs[0].ID), auditSeq(t, pool, evs[2].ID))
	if err != nil || len(got) != 3 {
		t.Fatalf("row hashes = %v, %v", got, err)
	}
	for i, ev := range evs {
		if got[i].Seq != auditSeq(t, pool, ev.ID) || got[i].RowHash != ev.RowHash {
			t.Fatalf("row %d = %+v, want seq %d hash %s", i, got[i], auditSeq(t, pool, ev.ID), ev.RowHash)
		}
	}
}
