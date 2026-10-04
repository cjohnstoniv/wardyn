// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The live state a second wardynd must see (ha-l2.4), against a real Postgres: two Servers over
// one database are two wardynds, each with its own memory. What reaches a table is read with SQL,
// never through the API alone. Guarded by WARDYN_TEST_PG (throwawayPGPool); skipped cleanly when
// unset.

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/livebus"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// haWait polls cond until it holds or d passes.
func haWait(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", d, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// liveReplica is a replica with a notice bus of its own.
func (l *maskLab) liveReplica(name string) replica {
	l.t.Helper()
	return l.replicaBuilt(l.pool, livebus.New(l.pool, name))
}

// storedChunks is every chunk's bytes of runID, read with SQL.
func (l *maskLab) storedChunks(runID uuid.UUID) string {
	l.t.Helper()
	var out string
	if err := l.pool.QueryRow(context.Background(),
		`SELECT coalesce(string_agg(convert_from(bytes_masked, 'UTF8'), '' ORDER BY seq), '') FROM run_output_chunks WHERE run_id = $1`, runID).Scan(&out); err != nil {
		l.t.Fatal(err)
	}
	return out
}

// Output written through server A is readable live from server B, masked before it was inserted
// (the column is read with SQL), and the final row replaces the chunks.
func TestHALivePG_OutputWrittenOnAIsReadLiveOnB(t *testing.T) {
	l := newMaskLab(t)
	a, b := l.replica(), l.replica()
	run := l.run()
	a.dispatch(t, run, "pg-secret-value-123")

	w := a.srv.openExecOutput(run, false)
	writeExecOutput(t, w, "hello pg-secret-value-123 world\n", "split pg-secret-", "value-123 end\n", "held pg-secret-v")

	// The masker withholds the last bytes as a possible secret prefix, so the live tail is
	// everything before them.
	const live = "hello <secret-hidden> world\nsplit <secret-hidden> end\nheld "
	haWait(t, 5*time.Second, "A's output in run_output_chunks", func() bool { return l.storedChunks(run.ID) == live })
	if got := l.storedChunks(run.ID); strings.Contains(got, "pg-secret-") {
		t.Fatalf("the chunk column holds the secret: %q", got)
	}
	code, got, body := l.readOutput(b, run.ID)
	if code != http.StatusOK || got.Output != live || got.Complete || got.Source != "stdout" || got.MaskScope != "run" {
		t.Fatalf("B's live read = %d %s, want A's tail, not complete, mask_scope run", code, body)
	}
	if a.srv.tailFor(run.ID) == nil || b.srv.tailFor(run.ID) != nil {
		t.Fatal("the tail should live in A's memory only")
	}
	if code, got, _ := l.readOutput(b, run.ID); code != http.StatusOK || got.Output != live {
		t.Fatalf("?tail is not the point here: second read = %d %q", code, got.Output)
	}

	// The final row replaces the chunks in one transaction, and B serves it.
	a.srv.FinishRunOutput(t.Context(), run.ID)
	const final = live + "<secret-hidden>"
	if code, got, body := l.readOutput(b, run.ID); code != http.StatusOK || got.Output != final || !got.Complete || got.CaptureGap {
		t.Fatalf("B's read after the finish = %d %s, want the complete final row", code, body)
	}
	if n := l.count(`SELECT count(*) FROM run_output_chunks WHERE run_id=$1`, run.ID); n != 0 {
		t.Fatalf("%d chunks survived the final row", n)
	}
}

// After A crashes, B serves the tail A wrote before the crash, and when B ends the run the row it
// writes from those chunks is an incomplete capture gap: what A had not flushed (the bytes its
// masker still withheld) is the gap.
func TestHALivePG_AfterACrashBServesTheTailAndTheGapCoversTheRest(t *testing.T) {
	l := newMaskLab(t)
	a, b := l.replica(), l.replica()
	run := l.run()
	a.dispatch(t, run, "pg-secret-value-123")
	w := a.srv.openExecOutput(run, false)
	writeExecOutput(t, w, "before the crash\n", "held pg-secret-v")
	haWait(t, 5*time.Second, "the chunks", func() bool { return l.storedChunks(run.ID) == "before the crash\nheld " })
	// A is gone: nothing more is written, nothing is finished.

	if code, got, body := l.readOutput(b, run.ID); code != http.StatusOK || got.Output != "before the crash\nheld " || got.Complete {
		t.Fatalf("B's read after A's crash = %d %s, want the tail A wrote", code, body)
	}

	// The run ends and B finalises it: it holds no tail and the runner cannot re-read the
	// substrate, so the row comes from the chunks.
	b.srv.FinishRunOutput(t.Context(), run.ID)
	row, found, err := store.NewPG(l.pool).GetRunOutput(t.Context(), run.ID)
	if err != nil || !found || row.CapturedAt == nil || !row.CaptureGap || !row.Incomplete || string(row.Output) != "before the crash\nheld " || row.MaskScope != "run" {
		t.Fatalf("B's row = %+v found=%v err=%v, want an incomplete capture gap holding A's tail", row, found, err)
	}
	if strings.Contains(string(row.Output), "pg-secret-") {
		t.Fatalf("the row holds the secret: %q", row.Output)
	}
	if code, got, body := l.readOutput(a, run.ID); code != http.StatusOK || !got.Complete || !got.CaptureGap || !got.Incomplete || got.Output != "before the crash\nheld " {
		t.Fatalf("a read after B's finish = %d %s, want the row, flagged incomplete and a capture gap", code, body)
	}
	if n := l.count(`SELECT count(*) FROM run_output_chunks WHERE run_id=$1`, run.ID); n != 0 {
		t.Fatalf("%d chunks survived the row written from them", n)
	}
	var audited bool
	for _, ev := range l.rec.snapshot() {
		if ev.Action == "run.output.finalize" && ev.RunID != nil && *ev.RunID == run.ID && strings.Contains(string(ev.Data), `"from_chunks":true`) {
			audited = true
		}
	}
	if !audited {
		t.Errorf("no run.output.finalize row says the capture gap came from the chunks: %s", auditDump(l.rec.snapshot(), run.ID))
	}
}

// An erasure deletes the chunks, and no late write recreates them: A's queue meets the tombstone
// and fences its tail.
func TestHALivePG_ErasureDeletesTheChunksAndNoLateWriteRecreatesThem(t *testing.T) {
	l := newMaskLab(t)
	a, b := l.replica(), l.replica()
	run := l.run()
	a.dispatch(t, run, "pg-secret-value-123")
	w := a.srv.openExecOutput(run, false)
	writeExecOutput(t, w, "printed before the erasure\n")
	haWait(t, 5*time.Second, "the chunks", func() bool { return l.storedChunks(run.ID) != "" })

	if err := b.srv.EraseRunOutputs(t.Context(), []uuid.UUID{run.ID}); err != nil {
		t.Fatal(err)
	}
	if got := l.storedChunks(run.ID); got != "" {
		t.Fatalf("chunks after the erasure: %q", got)
	}
	writeExecOutput(t, w, "printed after the erasure\n")
	haWait(t, 5*time.Second, "A's tail to be fenced by the tombstone", func() bool { return a.srv.tailFor(run.ID) == nil })
	if got := l.storedChunks(run.ID); got != "" {
		t.Fatalf("a late write recreated the chunks: %q", got)
	}
	if code, _, body := l.readOutput(b, run.ID); code != http.StatusNotFound || !strings.Contains(body, reasonRunOutputErased) {
		t.Fatalf("a read after the erasure = %d %s, want 404 %s", code, body, reasonRunOutputErased)
	}
}

// A kill served by server B cancels A's in-flight CreateSandbox within 2s.
func TestHALivePG_AKillOnBCancelsAsInFlightCreate(t *testing.T) {
	l := newMaskLab(t)
	a, b := l.liveReplica("replica-a"), l.liveReplica("replica-b")
	run := l.run()
	createCtx, end := a.srv.creates.track(t.Context(), run.ID)
	defer end()

	// The bus connects asynchronously: kill until A hears it (a kill is idempotent).
	start := time.Now()
	haWait(t, 10*time.Second, "A's create to be cancelled", func() bool {
		b.srv.cancelCreate(run.ID)
		select {
		case <-createCtx.Done():
			return true
		case <-time.After(50 * time.Millisecond):
			return false
		}
	})
	t.Logf("the create was cancelled %s after the first notice", time.Since(start))

	// And through the real kill route: the run is KILLED by B, and A's create (a second one) ends.
	run2 := l.run()
	createCtx2, end2 := a.srv.creates.track(t.Context(), run2.ID)
	defer end2()
	if w := do(t, b.srv, http.MethodPost, "/api/v1/runs/"+run2.ID.String()+"/kill", adminToken, ""); w.Code >= 300 {
		t.Fatalf("kill on B = %d %s", w.Code, w.Body)
	}
	select {
	case <-createCtx2.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("a kill served by B did not cancel A's in-flight CreateSandbox within 2s")
	}
	if st := l.runState(run2.ID); st != types.RunKilled {
		t.Fatalf("run state = %s, want KILLED", st)
	}
}

func (l *maskLab) runState(id uuid.UUID) types.RunState {
	l.t.Helper()
	var s string
	if err := l.pool.QueryRow(context.Background(), `SELECT state FROM agent_runs WHERE id=$1`, id).Scan(&s); err != nil {
		l.t.Fatal(err)
	}
	return types.RunState(s)
}

// ctxProbeRunner records whether the context CreateSandbox ran under was cancelled by the time
// it returned.
type ctxProbeRunner struct {
	*killRaceRunner
	cancelled atomic.Bool
}

func (r *ctxProbeRunner) CreateSandbox(ctx context.Context, spec runner.SandboxSpec) (runner.Sandbox, error) {
	sb, err := r.killRaceRunner.CreateSandbox(ctx, spec)
	r.cancelled.Store(ctx.Err() != nil)
	return sb, err
}

// With the notice dropped (A has no bus), the kill still wins: the create is not cancelled, but
// dispatch's STARTING to RUNNING compare loses, so it tears the sandbox down and the run stays
// KILLED. The guard that makes a lost NOTIFY safe.
func TestHALivePG_ALostKillNoticeStillEndsKilledWithTheSandboxTornDown(t *testing.T) {
	rn := &ctxProbeRunner{killRaceRunner: &killRaceRunner{fakeRunner: &fakeRunner{}}}
	srv, st, audit, run := dispatchTeardownFixture(t, rn, types.RunPending)
	rn.onCreate = func() {
		// B's kill: the store cell moves, and the notice that would have cancelled this create is lost.
		st.mu.Lock()
		st.state = types.RunKilled
		st.mu.Unlock()
	}
	srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
		RunToken: "run-token", Image: "wardyn/claude-code:latest",
		Policy: types.RunPolicySpec{MinConfinementClass: types.CC1},
	})
	if rn.cancelled.Load() {
		t.Fatal("the create was cancelled: this test is about the case where the notice never arrived")
	}
	if rn.stopCount() != 1 {
		t.Fatalf("the sandbox was torn down %d times, want exactly once", rn.stopCount())
	}
	if got := st.State(); got != types.RunKilled {
		t.Fatalf("state = %s, want KILLED", got)
	}
	if findAudit(audit.events, run.ID, "run.dispatch", "failure") == nil {
		t.Fatal("no run.dispatch failure row for the aborted dispatch")
	}
}
