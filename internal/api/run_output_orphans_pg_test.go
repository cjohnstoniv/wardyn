// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Live output chunks and the run_outputs row they owe, against a real Postgres: what reaches the tables
// is read with SQL. Guarded by WARDYN_TEST_PG (throwawayPGPool); skipped cleanly when unset.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// faultyOutputPG is the run-output store over Postgres as a server meets it before it stops: the first
// pending-row insert fails, as a dispatch-time insert does in a database blip, and every final write
// and claim refresh fails, so its retries reach nothing.
type faultyOutputPG struct {
	store.PG
	pendingFailed atomic.Bool
}

func (f *faultyOutputPG) InsertPendingRunOutput(ctx context.Context, id uuid.UUID) error {
	if f.pendingFailed.CompareAndSwap(false, true) {
		return errors.New("postgres is down")
	}
	return f.PG.InsertPendingRunOutput(ctx, id)
}

func (f *faultyOutputPG) SaveFinalRunOutput(context.Context, store.RunOutput) error {
	return errors.New("postgres is down")
}

func (f *faultyOutputPG) RefreshRunOutputClaim(context.Context, uuid.UUID) error {
	return errors.New("postgres is down")
}

// orphanChunks leaves runID as a server whose chunk append wrote no pending row left it after a failed
// dispatch-time insert: chunks holding b, and no run_outputs row at all.
func (l *maskLab) orphanChunks(runID uuid.UUID, b string) {
	l.t.Helper()
	ctx := context.Background()
	if _, err := store.NewPG(l.pool).AppendRunOutputChunk(ctx, runID, "older-server", []byte(b), 1<<20); err != nil {
		l.t.Fatal(err)
	}
	if _, err := l.pool.Exec(ctx, `DELETE FROM run_outputs WHERE run_id=$1`, runID); err != nil {
		l.t.Fatal(err)
	}
}

// endRunAgo marks runID COMPLETED with its sandbox gone, ended ago (a Postgres interval).
func (l *maskLab) endRunAgo(runID uuid.UUID, ago string) {
	l.t.Helper()
	if _, err := l.pool.Exec(context.Background(),
		`UPDATE agent_runs SET state='COMPLETED', sandbox_ref='', ended_at = now() - $2::interval WHERE id=$1`, runID, ago); err != nil {
		l.t.Fatal(err)
	}
}

// The faults in a row: the dispatch-time pending insert fails, the chunks still land, the run ends and
// its teardown clears the sandbox, and every final write fails while the server's retries reach nothing,
// as when it stops. The chunks owe their row through all of it: the next server's sweep recovers the run
// from them (the sandbox is gone, so they are all that is left), and once the run is past the retention
// window the read returns nothing.
func TestRunOutputPG_ChunksOweTheirRowThroughEveryFault(t *testing.T) {
	l := newMaskLab(t)
	ctx := t.Context()
	a := l.replica()
	run := l.run()
	a.dispatch(t, run, "pg-secret-value-123")
	faulty := &faultyOutputPG{PG: store.NewPG(l.pool)}
	a.srv.cfg.Store = faulty
	a.srv.runOutputRetryBaseOverride = time.Millisecond

	w := a.srv.openExecOutput(run, false)
	if !faulty.pendingFailed.Load() {
		t.Fatal("setup: no dispatch-time pending insert was made")
	}
	const printed = "printed before the faults\n"
	writeExecOutput(t, w, printed)
	haWait(t, 5*time.Second, "the chunks", func() bool { return l.storedChunks(run.ID) == printed })
	if n := l.count(`SELECT count(*) FROM run_outputs WHERE run_id=$1 AND captured_at IS NULL`, run.ID); n != 1 {
		t.Fatalf("%d pending rows beside the chunks, want the one their append wrote", n)
	}

	l.endRunAgo(run.ID, "0 seconds")
	a.srv.FinishRunOutput(ctx, run.ID)
	if n := l.count(`SELECT count(*) FROM run_outputs WHERE run_id=$1 AND captured_at IS NOT NULL`, run.ID); n != 0 {
		t.Fatal("setup: a final row was written although every final write failed")
	}

	b := l.replica()
	if _, err := l.pool.Exec(ctx, `UPDATE run_outputs SET claimed_at = now() - interval '10 minutes' WHERE run_id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := b.srv.SweepRunOutputs(ctx); err != nil {
		t.Fatal(err)
	}
	if out, gap, incomplete, _ := l.storedRow(run.ID); out != printed || !gap || !incomplete {
		t.Fatalf("the recovered row = %q gap=%v incomplete=%v, want the chunks as an incomplete capture gap", out, gap, incomplete)
	}
	if n := l.count(`SELECT count(*) FROM run_output_chunks WHERE run_id=$1`, run.ID); n != 0 {
		t.Fatalf("%d chunks outlived the row written from them", n)
	}

	b.srv.cfg.RunOutputRetention = 30 * 24 * time.Hour
	l.endRunAgo(run.ID, "40 days")
	if _, err := l.pool.Exec(ctx, `UPDATE run_outputs SET captured_at = now() - interval '35 days' WHERE run_id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := b.srv.SweepRunOutputs(ctx); err != nil {
		t.Fatal(err)
	}
	if code, _, body := l.readOutput(b, run.ID); code != http.StatusGone || !strings.Contains(body, reasonRunOutputExpired) || strings.Contains(body, "printed before") {
		t.Fatalf("read past the window = %d %s, want 410 %s and nothing of the output", code, body, reasonRunOutputExpired)
	}
}

// Chunks a server left with no row are found and aged by the sweep without touching live output. A run
// that ended past the retention window reads 410 with none of its chunks, before the sweep and after it,
// and the sweep deletes them without writing a row; a run that ended inside the window gets the pending
// row it was owed, and once that row's claim is stale the next pass writes its row from the chunks; a
// live run's chunks are left as they are; an erased run keeps its tombstone and its 404.
func TestRunOutputPG_OrphanChunksAreAgedOrRecovered(t *testing.T) {
	l := newMaskLab(t)
	ctx := t.Context()
	a := l.replica()
	a.srv.cfg.RunOutputRetention = 30 * 24 * time.Hour
	expired, young, live, erased := l.run(), l.run(), l.run(), l.run()
	l.orphanChunks(expired.ID, "expired output\n")
	l.orphanChunks(young.ID, "young output\n")
	l.orphanChunks(live.ID, "live output\n")
	l.endRunAgo(expired.ID, "40 days")
	l.endRunAgo(young.ID, "1 day")
	if err := a.srv.EraseRunOutputs(ctx, []uuid.UUID{erased.ID}); err != nil {
		t.Fatal(err)
	}
	l.endRunAgo(erased.ID, "40 days")
	chunks := func(id uuid.UUID) int { return l.count(`SELECT count(*) FROM run_output_chunks WHERE run_id=$1`, id) }
	rows := func(id uuid.UUID) int { return l.count(`SELECT count(*) FROM run_outputs WHERE run_id=$1`, id) }
	expiredRead := func(when string) {
		t.Helper()
		if code, _, body := l.readOutput(a, expired.ID); code != http.StatusGone || !strings.Contains(body, reasonRunOutputExpired) || strings.Contains(body, "expired output") {
			t.Fatalf("read of the expired run %s = %d %s, want 410 %s and none of its chunks", when, code, body, reasonRunOutputExpired)
		}
	}

	expiredRead("before the sweep")
	if code, got, body := l.readOutput(a, young.ID); code != http.StatusOK || got.Output != "young output\n" {
		t.Fatalf("read of a run inside the window = %d %s, want its chunks", code, body)
	}

	if err := a.srv.SweepRunOutputs(ctx); err != nil {
		t.Fatal(err)
	}
	if chunks(expired.ID) != 0 || rows(expired.ID) != 0 {
		t.Errorf("expired run: %d chunks, %d rows after the sweep; want both 0", chunks(expired.ID), rows(expired.ID))
	}
	expiredRead("after the sweep")
	if n := l.count(`SELECT count(*) FROM run_outputs WHERE run_id=$1 AND captured_at IS NULL`, young.ID); n != 1 || chunks(young.ID) == 0 {
		t.Errorf("run inside the window: %d pending rows, %d chunks; want the owed pending row and its chunks kept", n, chunks(young.ID))
	}
	if rows(live.ID) != 0 || chunks(live.ID) == 0 {
		t.Errorf("live run: %d rows, %d chunks; want its output untouched", rows(live.ID), chunks(live.ID))
	}
	if n := l.count(`SELECT count(*) FROM run_output_erasures WHERE run_id=$1`, erased.ID); n != 1 {
		t.Errorf("%d tombstones for the erased run, want it kept", n)
	}
	if code, _, body := l.readOutput(a, erased.ID); code != http.StatusNotFound || !strings.Contains(body, reasonRunOutputErased) {
		t.Errorf("read of the erased run = %d %s, want 404 %s", code, body, reasonRunOutputErased)
	}

	if _, err := l.pool.Exec(ctx, `UPDATE run_outputs SET claimed_at = now() - interval '10 minutes' WHERE run_id=$1`, young.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.srv.SweepRunOutputs(ctx); err != nil {
		t.Fatal(err)
	}
	if out, gap, incomplete, _ := l.storedRow(young.ID); out != "young output\n" || !gap || !incomplete || chunks(young.ID) != 0 {
		t.Errorf("run inside the window: row %q gap=%v incomplete=%v, %d chunks; want its chunks as an incomplete capture gap", out, gap, incomplete, chunks(young.ID))
	}
}
