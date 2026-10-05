// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The persisted run output against a real Postgres: what reaches run_outputs is
// read with SQL, not through the API, and two Servers over one database are two
// wardynds, each with its own memory and manifest cache. Guarded by
// WARDYN_TEST_PG (throwawayPGPool); skipped cleanly when unset.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func (l *maskLab) readOutput(rp replica, runID uuid.UUID) (int, runOutputResponse, string) {
	l.t.Helper()
	w := do(l.t, rp.srv, http.MethodGet, "/api/v1/runs/"+runID.String()+"/output", adminToken, "")
	var got runOutputResponse
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			l.t.Fatalf("decode %q: %v", w.Body, err)
		}
	}
	return w.Code, got, w.Body.String()
}

// storedOutput reads the row's bytes straight from the column.
func (l *maskLab) storedOutput(runID uuid.UUID) (out []byte, capturedAtSet bool) {
	l.t.Helper()
	if err := l.pool.QueryRow(context.Background(),
		`SELECT output, captured_at IS NOT NULL FROM run_outputs WHERE run_id=$1`, runID).Scan(&out, &capturedAtSet); err != nil {
		l.t.Fatalf("read run_outputs for %s: %v", runID, err)
	}
	return out, capturedAtSet
}

func (l *maskLab) count(query string, args ...any) int {
	l.t.Helper()
	var n int
	if err := l.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		l.t.Fatalf("%s: %v", query, err)
	}
	return n
}

// Output survives a restart, and what is in the column is already masked: a
// registered secret printed whole, split across two writes and held back as the
// final bytes is absent from run_outputs.output read with SQL. The terminal row
// is then served by two fresh servers (neither holds the manifest), carrying the
// scope of the manifest the capture was made under, until an erasure.
func TestRunOutputPG_SurvivesRestartMaskedBeforeInsert(t *testing.T) {
	l := newMaskLab(t)
	a := l.replica()
	run := l.run()
	a.dispatch(t, run, "pg-secret-value-123")

	w := a.srv.openExecOutput(run, false)
	if l.count(`SELECT count(*) FROM run_outputs WHERE run_id=$1 AND captured_at IS NULL`, run.ID) != 1 {
		t.Fatal("dispatch wrote no pending row recording that a capture is owed")
	}
	writeExecOutput(t, w, "whole pg-secret-value-123 ok\n", "split pg-secret-", "value-123 ok\n", "held pg-secret-v")
	a.srv.FinishRunOutput(t.Context(), run.ID)

	raw, final := l.storedOutput(run.ID)
	const want = "whole <secret-hidden> ok\nsplit <secret-hidden> ok\nheld <secret-hidden>"
	if !final || string(raw) != want || strings.Contains(string(raw), "pg-secret-") {
		t.Fatalf("run_outputs.output = %q (final=%v), want %q: the secret must never reach the column", raw, final, want)
	}
	if a.srv.tailFor(run.ID) != nil {
		t.Error("the memory tail outlived its committed row")
	}

	b, c := l.replica(), l.replica() // both replicas restarted
	for name, rp := range map[string]replica{"B": b, "C": c} {
		code, got, body := l.readOutput(rp, run.ID)
		if code != http.StatusOK || got.Output != want || !got.Complete || got.MaskScope != "run" || got.CapturedAt == nil || got.Source != "stdout" || got.Incomplete {
			t.Fatalf("replica %s read = %d %s, want the stored row, complete, mask_scope run", name, code, body)
		}
	}

	// No manifest loaded anywhere: the row is still served, and the live door stays closed.
	if _, err := l.pool.Exec(t.Context(), `DELETE FROM run_mask_manifest WHERE run_id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	d := l.replica()
	if code, got, body := l.readOutput(d, run.ID); code != http.StatusOK || got.Output != want {
		t.Fatalf("a replica with no manifest read = %d %s, want the stored row", code, body)
	}

	// The tombstone is checked on that read, on every replica.
	if err := b.srv.EraseRunOutputs(t.Context(), []uuid.UUID{run.ID}); err != nil {
		t.Fatal(err)
	}
	for name, rp := range map[string]replica{"C": c, "D": d, "fresh": l.replica()} {
		if code, _, body := l.readOutput(rp, run.ID); code != http.StatusNotFound || !strings.Contains(body, reasonRunOutputErased) {
			t.Fatalf("replica %s after the erasure = %d %s, want 404 %s", name, code, body, reasonRunOutputErased)
		}
	}
	if l.count(`SELECT count(*) FROM run_outputs WHERE run_id=$1`, run.ID) != 0 {
		t.Error("the erasure left the row in Postgres")
	}
}

// A live read of a run whose manifest stopped covering it is refused with
// ha-l2.0's denied audit row; the capture's writer says it was uncovered; and
// the terminal row of the same run is served, marked globals_only.
func TestRunOutputPG_UncoveredLiveReadRefusedTerminalRowServedGlobalsOnly(t *testing.T) {
	l := newMaskLab(t)
	a, b := l.replica(), l.replica()
	run := l.run()
	a.dispatch(t, run, "covered-then-fenced-value")
	w := a.srv.openExecOutput(run, false)
	writeExecOutput(t, w, "covered line\n")
	if code, got, body := l.readOutput(a, run.ID); code != http.StatusOK || got.Output != "covered line\n" || got.Complete {
		t.Fatalf("a live read while covered = %d %s, want 200 and not complete", code, body)
	}

	if fenced, err := b.srv.cfg.MaskManifests.FenceSubject(t.Context(), maskOwner); err != nil || len(fenced) != 1 {
		t.Fatalf("FenceSubject = %v, %v", fenced, err)
	}
	code, _, body := l.readOutput(a, run.ID)
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "mask_state_unavailable") {
		t.Fatalf("an uncovered live read = %d %s, want 503 mask_state_unavailable", code, body)
	}
	if row := l.deniedRow(run.ID, "runs.output"); row["mask_scope"] != "globals_only" {
		t.Errorf("denied row %v, want mask_scope globals_only", row)
	}
	waitFor(t, "the writer to see the fence", func() bool {
		_, _ = w.Write([]byte("x"))
		e := a.srv.tailFor(run.ID)
		e.mw.mu.Lock()
		defer e.mw.mu.Unlock()
		return e.mw.capture.uncovered
	})

	a.srv.FinishRunOutput(t.Context(), run.ID)
	c := l.replica()
	code, got, body := l.readOutput(c, run.ID)
	if code != http.StatusOK || got.MaskScope != "globals_only" || !got.Incomplete || !got.Complete || !strings.HasPrefix(got.Output, "covered line\n") {
		t.Fatalf("the terminal row = %d %s, want it served with mask_scope globals_only, incomplete", code, body)
	}
}

// Retention and the stale-pending sweep, against the real SQL: a row past the
// window is deleted and its run reads 410; a terminal run's pending row whose
// claim is stale becomes a capture gap, a fresh claim is left alone, and a run
// still live is never resolved.
func TestRunOutputPG_SweepDeletesExpiredAndResolvesStalePending(t *testing.T) {
	l := newMaskLab(t)
	a := l.replica()
	a.srv.cfg.RunOutputRetention = 30 * 24 * time.Hour
	ctx := t.Context()

	expired, stale, fresh, live := l.run(), l.run(), l.run(), l.run()
	for _, r := range []types.AgentRun{expired, stale, fresh} {
		if _, err := l.pool.Exec(ctx, `UPDATE agent_runs SET state='COMPLETED' WHERE id=$1`, r.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := l.pool.Exec(ctx, `UPDATE agent_runs SET ended_at = now() - interval '40 days' WHERE id=$1`, expired.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := l.pool.Exec(ctx, `INSERT INTO run_outputs (run_id, output, source, captured_at) VALUES ($1, 'old', 'stdout', now() - interval '35 days')`, expired.ID); err != nil {
		t.Fatal(err)
	}
	for id, claimed := range map[uuid.UUID]string{stale.ID: "10 minutes", fresh.ID: "1 minute", live.ID: "10 minutes"} {
		if _, err := l.pool.Exec(ctx, `INSERT INTO run_outputs (run_id, source, claimed_at) VALUES ($1, 'stdout', now() - $2::interval)`, id, claimed); err != nil {
			t.Fatal(err)
		}
	}

	if err := a.srv.SweepRunOutputs(ctx); err != nil {
		t.Fatal(err)
	}
	if l.count(`SELECT count(*) FROM run_outputs WHERE run_id=$1`, expired.ID) != 0 {
		t.Error("the sweep kept a row past the retention window")
	}
	if code, _, body := l.readOutput(a, expired.ID); code != http.StatusGone || !strings.Contains(body, reasonRunOutputExpired) {
		t.Errorf("read of the expired run = %d %s, want 410 %s", code, body, reasonRunOutputExpired)
	}
	if n := l.count(`SELECT count(*) FROM run_outputs WHERE run_id=$1 AND capture_gap AND captured_at IS NOT NULL`, stale.ID); n != 1 {
		t.Error("a terminal run's stale pending row was not resolved to a capture gap")
	}
	for name, id := range map[string]uuid.UUID{"fresh claim": fresh.ID, "live run": live.ID} {
		if l.count(`SELECT count(*) FROM run_outputs WHERE run_id=$1 AND captured_at IS NULL`, id) != 1 {
			t.Errorf("the sweep resolved a pending row it must leave (%s)", name)
		}
	}
	if len(l.recEvents("run.output.retention.sweep")) != 1 {
		t.Error("a sweep that deleted rows must write one run.output.retention.sweep row")
	}
}

func (l *maskLab) recEvents(action string) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range l.rec.snapshot() {
		if ev.Action == action {
			out = append(out, ev)
		}
	}
	return out
}

// killOnDelete is a pgx tracer that ends the connection's backend just before
// the erasure's DELETE runs: a Postgres outage in the middle of the transaction,
// after its locks and tombstones were taken.
type killOnDelete struct {
	admin *pgxpool.Pool
	armed atomic.Bool
}

func (k *killOnDelete) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if k.armed.Load() && strings.Contains(data.SQL, "DELETE FROM run_outputs") {
		_, _ = k.admin.Exec(context.Background(), `SELECT pg_terminate_backend($1)`, conn.PgConn().PID())
		time.Sleep(100 * time.Millisecond)
	}
	return ctx
}

func (k *killOnDelete) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// An erasure whose transaction dies part-way deletes nothing and writes no
// tombstone, and a retry completes it. Afterwards a second server that held the
// tail, a blocked finaliser and a restarted server all fail to recreate the row.
func TestRunOutputPG_EraseIsAtomicAndHoldsAcrossARestart(t *testing.T) {
	l := newMaskLab(t)
	a := l.replica()
	r1, r2 := l.run(), l.run()
	ctx := t.Context()
	for _, r := range []types.AgentRun{r1, r2} {
		a.dispatch(t, r, "erase-secret-value-"+r.ID.String()[:4])
		writeExecOutput(t, a.srv.openExecOutput(r, false), "output of "+r.ID.String()+"\n")
		a.srv.FinishRunOutput(ctx, r.ID)
	}
	if l.count(`SELECT count(*) FROM run_outputs WHERE captured_at IS NOT NULL`) != 2 {
		t.Fatal("setup: two final rows expected")
	}

	// C holds a live tail for r1, as a second server mid-run does.
	c := l.replica()
	wc := c.srv.openExecOutput(r1, false)
	writeExecOutput(t, wc, "held on C\n")
	tail := c.srv.tailFor(r1.ID)

	cfg := l.pool.Config().Copy()
	killer := &killOnDelete{admin: l.pool}
	cfg.ConnConfig.Tracer = killer
	flaky, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(flaky.Close)
	killer.armed.Store(true)
	if err := store.NewPG(flaky).EraseRunOutputs(ctx, []uuid.UUID{r1.ID, r2.ID}); err == nil {
		t.Fatal("an erasure whose connection died mid-transaction reported success")
	}
	killer.armed.Store(false)
	if n := l.count(`SELECT count(*) FROM run_outputs WHERE captured_at IS NOT NULL`); n != 2 {
		t.Fatalf("%d rows after the failed erasure, want both still there", n)
	}
	if n := l.count(`SELECT count(*) FROM run_output_erasures`); n != 0 {
		t.Fatalf("%d tombstones after the failed erasure, want none", n)
	}

	if err := a.srv.EraseRunOutputs(ctx, []uuid.UUID{r1.ID, r2.ID}); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if l.count(`SELECT count(*) FROM run_outputs`) != 0 || l.count(`SELECT count(*) FROM run_output_erasures`) != 2 {
		t.Fatal("the retry did not delete both rows and write both tombstones")
	}

	// C's finaliser, with the bytes still in memory, cannot recreate the row, and its tail is dropped, zeroed.
	c.srv.FinishRunOutput(ctx, r1.ID)
	if l.count(`SELECT count(*) FROM run_outputs`) != 0 {
		t.Fatal("a second server's finaliser recreated an erased row")
	}
	if c.srv.tailFor(r1.ID) != nil || tail.ring.buf != nil {
		t.Fatal("the tombstone hit left C's tail in memory")
	}
	if _, err := wc.Write([]byte("more\n")); err != nil {
		t.Fatal(err)
	}

	d := l.replica() // a restart
	for _, r := range []types.AgentRun{r1, r2} {
		if d.srv.openExecOutput(r, false) != nil {
			t.Error("a restarted server opened a tail for an erased run")
		}
		if code, _, body := l.readOutput(d, r.ID); code != http.StatusNotFound || !strings.Contains(body, reasonRunOutputErased) {
			t.Errorf("read after the restart = %d %s, want 404", code, body)
		}
		d.srv.FinishRunOutput(ctx, r.ID)
	}
	if l.count(`SELECT count(*) FROM run_outputs`) != 0 {
		t.Fatal("a finaliser after the restart recreated an erased row")
	}
}

// Rows written by the sweeper's gap path and by a final write that follows it:
// the final row replaces the gap, and a gap never replaces a final row.
func TestRunOutputPG_GapNeverOverwritesAFinalRow(t *testing.T) {
	l := newMaskLab(t)
	a := l.replica()
	run := l.run()
	a.dispatch(t, run, "gap-secret-value-1")
	writeExecOutput(t, a.srv.openExecOutput(run, false), "real bytes\n")
	a.srv.FinishRunOutput(t.Context(), run.ID)

	b := l.replica() // no tail: finalising writes a gap, which must not touch the final row
	b.srv.prepareRunOutput(t.Context(), run.ID, false)
	raw, final := l.storedOutput(run.ID)
	if !final || string(raw) != "real bytes\n" {
		t.Fatalf("row %q final=%v after a gap attempt, want the real bytes", raw, final)
	}
	if n := l.count(`SELECT count(*) FROM run_outputs WHERE run_id=$1 AND capture_gap`, run.ID); n != 0 {
		t.Fatal("a gap flag landed on a final row")
	}
}

// An idle-stopped interactive run has a pane_snapshot row in Postgres in which a
// secret shown in the pane is absent, read with SQL; the covered manifest gives
// the row its scope, and the audit row carries no pane content.
func TestRunOutputPG_PaneSnapshotIsMaskedInTheColumn(t *testing.T) {
	l := newMaskLab(t)
	a := l.replica()
	run := l.run()
	a.dispatch(t, run, "pane-secret-value-123")
	if _, err := l.pool.Exec(t.Context(), `UPDATE agent_runs SET interactive = true WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	a.srv.cfg.Runner = &paneRunner{
		outputRunner: &outputRunner{fakeRunner: &fakeRunner{}}, o: &orderLog{},
		pane: textPane("$ echo pane-secret-value-123\npane-secret-value-123\n$ ", 0),
	}

	a.srv.SnapshotRunPane(t.Context(), run.ID)
	a.srv.FinishRunOutput(t.Context(), run.ID)

	var out []byte
	var source, scope string
	if err := l.pool.QueryRow(t.Context(),
		`SELECT output, source, mask_scope FROM run_outputs WHERE run_id=$1 AND captured_at IS NOT NULL`, run.ID).Scan(&out, &source, &scope); err != nil {
		t.Fatalf("read the snapshot row: %v", err)
	}
	if want := "$ echo <secret-hidden>\n<secret-hidden>\n$ "; source != "pane_snapshot" || string(out) != want || scope != "run" {
		t.Fatalf("row = %q source %q scope %q, want %q pane_snapshot run", out, source, scope, want)
	}
	evs := l.recEvents("run.output.snapshot")
	if len(evs) != 1 || evs[0].Outcome != "success" || strings.Contains(string(evs[0].Data), "echo") {
		t.Fatalf("audit rows %+v, want one success row without pane content", evs)
	}
}

// With persistence off, a finished run's sealed tail is served from memory for
// the TTL even after the sweeper has purged the run's masking manifest: the tail
// was masked at write time and cannot gain bytes, so it needs no manifest. Past
// the TTL the read says it expired.
func TestRunOutputPG_PersistOffSealedTailOutlivesThePurgedManifest(t *testing.T) {
	l := newMaskLab(t)
	a := l.replica()
	const ttl = time.Hour
	clock := &testClock{now: time.Now().UTC()}
	a.srv.cfg.RunOutputPersistOff, a.srv.cfg.ExecOutputTailTTL, a.srv.cfg.Now = true, ttl, clock.Now
	run := l.run()
	a.dispatch(t, run, "purge-me-secret-value")

	w := a.srv.openExecOutput(run, false)
	writeExecOutput(t, w, "line with purge-me-secret-value\n")
	if code, _, body := l.readOutput(a, run.ID); code != http.StatusOK {
		t.Fatalf("a live read while covered = %d %s, want 200", code, body)
	}
	if ok, err := store.NewPG(l.pool).UpdateRunStateIf(t.Context(), run.ID, types.RunRunning, types.RunCompleted); err != nil || !ok {
		t.Fatalf("complete the run: %v %v", ok, err)
	}
	a.srv.FinishRunOutput(t.Context(), run.ID)
	if a.srv.tailFor(run.ID) == nil {
		t.Fatal("persistence off released the sealed tail")
	}

	if err := a.st.PurgeRuns(t.Context(), []uuid.UUID{run.ID}); err != nil { // the leader's sweep, an hour after the end
		t.Fatal(err)
	}
	clock.advance(ttl - time.Minute)
	const want = "line with <secret-hidden>\n"
	code, got, body := l.readOutput(a, run.ID)
	if code != http.StatusOK || got.Output != want || !got.Complete {
		t.Fatalf("the sealed tail after the purge = %d %s, want 200 %q complete", code, body, want)
	}
	for _, ev := range l.rec.snapshot() {
		if ev.Target == "runs.output" && ev.Outcome == "denied" {
			t.Errorf("the read of a sealed tail wrote a denied row: %+v", ev)
		}
	}

	clock.advance(time.Minute)
	if code, _, body := l.readOutput(a, run.ID); code != http.StatusGone || !strings.Contains(body, reasonRunOutputExpired) {
		t.Fatalf("a read after the TTL = %d %s, want 410 %s", code, body, reasonRunOutputExpired)
	}
}
