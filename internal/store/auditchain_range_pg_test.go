// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func appendRunRow(t *testing.T, pool *pgxpool.Pool, run *uuid.UUID, at time.Time, action, target string) int64 {
	t.Helper()
	ev := types.AuditEvent{ID: uuid.New(), Time: at, RunID: run, ActorType: types.ActorSystem, Actor: "range-test",
		Action: action, Target: target, Outcome: "success"}
	if err := store.InsertAuditEvent(context.Background(), pool, &ev); err != nil {
		t.Fatalf("insert audit event: %v", err)
	}
	return auditSeq(t, pool, ev.ID)
}

// tamperAudit runs stmt with the append-only guard off, as a DB admin could.
func tamperAudit(t *testing.T, pool *pgxpool.Pool, stmt string, args ...any) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `ALTER TABLE audit_events DISABLE TRIGGER audit_events_no_update`); err != nil {
		storeSkipOrFatal(t, pool, "cannot disable the append-only trigger as this role (%v); the tamper case needs table ownership", err)
	}
	defer pool.Exec(ctx, `ALTER TABLE audit_events ENABLE TRIGGER audit_events_no_update`) //nolint:errcheck
	if _, err := pool.Exec(ctx, stmt, args...); err != nil {
		t.Fatalf("tamper: %v", err)
	}
}

// TestPG_RecordingAuditRange: the range runs from the run's first row at or after from to the
// recording's write row, other runs' rows inside it included; it names the run's desktop.recording
// rows; an edit inside the range breaks it at that row, an edit after it does not; and a row removed
// just before the range breaks the range's first link.
func TestPG_RecordingAuditRange(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	run, other := uuid.New(), uuid.New()
	t0 := time.Date(2026, 10, 20, 10, 40, 0, 0, time.UTC)
	key := run.String() + "~desktop-" + uuid.NewString()

	appendRunRow(t, pool, &run, t0.Add(-time.Hour), "run.create", "")
	before := appendRunRow(t, pool, &other, t0.Add(-time.Second), "test.other", "")
	lo := appendRunRow(t, pool, &run, t0.Add(time.Second), "ui.clipboard.transfer", "paste_in")
	pause := appendRunRow(t, pool, &run, t0.Add(2*time.Second), "desktop.recording.pause", run.String())
	inside := appendRunRow(t, pool, &other, t0.Add(3*time.Second), "test.other", "")
	resume := appendRunRow(t, pool, &run, t0.Add(4*time.Second), "desktop.recording.resume", run.String())
	hi := appendRunRow(t, pool, &run, t0.Add(5*time.Second), "desktop.recording.write", key)
	after := appendRunRow(t, pool, &other, t0.Add(6*time.Second), "test.other", "")

	rng, found, err := pg.VerifyRecordingAuditRange(ctx, run, key, t0)
	if err != nil || !found {
		t.Fatalf("verify: found=%v err=%v", found, err)
	}
	if !rng.OK || rng.FirstSeq != lo || rng.LastSeq != hi || rng.Checked != hi-lo+1 {
		t.Fatalf("range = %+v, want ok from %d to %d with %d rows", rng, lo, hi, hi-lo+1)
	}
	var marks []int64
	for _, m := range rng.Marks {
		marks = append(marks, m.Seq)
	}
	if len(marks) != 3 || marks[0] != pause || marks[1] != resume || marks[2] != hi {
		t.Fatalf("marks = %v, want the run's desktop.recording rows %d, %d, %d", marks, pause, resume, hi)
	}

	if _, found, err := pg.VerifyRecordingAuditRange(ctx, run, run.String()+"~desktop-unknown", t0); err != nil || found {
		t.Fatalf("an unknown key: found=%v err=%v, want not found", found, err)
	}

	tamperAudit(t, pool, `UPDATE audit_events SET actor='edited-after' WHERE seq=$1`, after)
	if rng, _, err := pg.VerifyRecordingAuditRange(ctx, run, key, t0); err != nil || !rng.OK {
		t.Fatalf("an edit after the range: %+v %v, want the range still ok", rng, err)
	}
	tamperAudit(t, pool, `UPDATE audit_events SET actor='edited-inside' WHERE seq=$1`, inside)
	if rng, _, err := pg.VerifyRecordingAuditRange(ctx, run, key, t0); err != nil || rng.OK || rng.BrokenSeq != inside {
		t.Fatalf("an edit inside the range: %+v %v, want broken at %d", rng, err, inside)
	}
	tamperAudit(t, pool, `UPDATE audit_events SET actor='range-test' WHERE seq=$1`, inside)
	tamperAudit(t, pool, `DELETE FROM audit_events WHERE seq=$1`, before)
	if rng, _, err := pg.VerifyRecordingAuditRange(ctx, run, key, t0); err != nil || rng.OK || rng.BrokenSeq != lo {
		t.Fatalf("the row before the range removed: %+v %v, want broken at the range's first row %d", rng, err, lo)
	}
}

// TestPG_RecordingAuditRangeFirstRowRemoved: a range with no row before it starts at the log's oldest
// retained row, which must chain to nothing (no retention drop): the oldest rows deleted leave the
// range's first row carrying a prev_hash, and the range breaks there as the whole sweep does.
func TestPG_RecordingAuditRangeFirstRowRemoved(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	run, other := uuid.New(), uuid.New()
	t0 := time.Now().UTC().Truncate(time.Second)
	key := run.String() + "~desktop-" + uuid.NewString()

	appendRunRow(t, pool, &other, t0.Add(-time.Second), "test.other", "")
	lo := appendRunRow(t, pool, &run, t0.Add(time.Second), "ui.clipboard.transfer", "paste_in")
	appendRunRow(t, pool, &run, t0.Add(2*time.Second), "desktop.recording.write", key)

	if rng, found, err := pg.VerifyRecordingAuditRange(ctx, run, key, t0); err != nil || !found || !rng.OK {
		t.Fatalf("intact: %+v found=%v err=%v, want ok", rng, found, err)
	}
	tamperAudit(t, pool, `DELETE FROM audit_events WHERE seq < $1`, lo)
	if rng, found, err := pg.VerifyRecordingAuditRange(ctx, run, key, t0); err != nil || !found || rng.OK || rng.BrokenSeq != lo {
		t.Fatalf("the log's oldest rows removed: %+v found=%v err=%v, want broken at the range's first row %d", rng, found, err, lo)
	}
}

// TestPG_RecordingAuditRangeAtTheLogsStart: an intact range with no row before it verifies, on a log
// that never dropped rows (the first row chains to nothing) and on one whose oldest rows a retention
// drop removed (it chains to the newest drop's recorded tail, and breaks at the range's first row
// when a newer drop records another).
func TestPG_RecordingAuditRangeAtTheLogsStart(t *testing.T) {
	ctx := context.Background()
	run, other := uuid.New(), uuid.New()
	t0 := time.Now().UTC().Truncate(time.Second)
	key := run.String() + "~desktop-" + uuid.NewString()

	t.Run("no drop", func(t *testing.T) {
		pool := runsPGPoolIsolated(t)
		lo := appendRunRow(t, pool, &run, t0.Add(time.Second), "ui.clipboard.transfer", "paste_in")
		hi := appendRunRow(t, pool, &run, t0.Add(2*time.Second), "desktop.recording.write", key)
		var before int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE seq < $1`, lo).Scan(&before); err != nil || before != 0 {
			t.Fatalf("fixture: %d rows before the range (err %v), want none", before, err)
		}
		rng, found, err := store.NewPG(pool).VerifyRecordingAuditRange(ctx, run, key, t0)
		if err != nil || !found || !rng.OK || rng.FirstSeq != lo || rng.LastSeq != hi {
			t.Fatalf("range = %+v found=%v err=%v, want ok from %d to %d", rng, found, err, lo, hi)
		}
	})

	t.Run("after a drop", func(t *testing.T) {
		pool := runsPGPoolIsolated(t)
		appendRunRow(t, pool, &other, t0.Add(-time.Second), "test.other", "")
		lo := appendRunRow(t, pool, &run, t0.Add(time.Second), "ui.clipboard.transfer", "paste_in")
		appendRunRow(t, pool, &run, t0.Add(2*time.Second), "desktop.recording.write", key)
		var tail string
		if err := pool.QueryRow(ctx, `SELECT row_hash FROM audit_events WHERE seq < $1 ORDER BY seq DESC LIMIT 1`, lo).Scan(&tail); err != nil {
			t.Fatalf("read the tail: %v", err)
		}
		tamperAudit(t, pool, `DELETE FROM audit_events WHERE seq < $1`, lo)
		anchor := func(name, tail string) {
			t.Helper()
			if _, err := pool.Exec(ctx, `INSERT INTO audit_chain_anchors (kind, partition_name, seq_lo, seq_hi, row_count, tail_row_hash, actor)
				VALUES ('drop', $1, 1, $2, 1, $3, 'test')`, name, lo-1, tail); err != nil {
				t.Fatalf("anchor: %v", err)
			}
		}
		anchor("audit_events_range_test_1", tail)
		pg := store.NewPG(pool)
		if rng, found, err := pg.VerifyRecordingAuditRange(ctx, run, key, t0); err != nil || !found || !rng.OK {
			t.Fatalf("a range that chains to the drop's tail: %+v found=%v err=%v, want ok", rng, found, err)
		}
		anchor("audit_events_range_test_2", "not-the-tail")
		if rng, found, err := pg.VerifyRecordingAuditRange(ctx, run, key, t0); err != nil || !found || rng.OK || rng.BrokenSeq != lo {
			t.Fatalf("a range that does not chain to the newest drop: %+v found=%v err=%v, want broken at %d", rng, found, err, lo)
		}
	})
}
