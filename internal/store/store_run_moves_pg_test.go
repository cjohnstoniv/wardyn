// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Moves of a paused run, their placement history and the recorded sandbox spec, against a real Postgres. Guarded by
// WARDYN_TEST_PG; each test works in its own throwaway database.
package store_test

import (
	"bytes"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// moveRig is one RUNNING run on its claimed source runner, in a stopped pause, and a claimed destination runner.
type moveRig struct {
	t        *testing.T
	pool     *pgxpool.Pool
	st       store.PG
	src, dst types.Runner
	run      types.AgentRun
	ref      string
	gen      int64
}

func newMoveRig(t *testing.T) *moveRig {
	t.Helper()
	pool, st, src, run, _ := spoolPGFixture(t)
	g := &moveRig{t: t, pool: pool, st: st, src: src, run: run, ref: "runner:" + src.ID.String() + "/sandbox"}
	g.dst = g.runner("move-dest-owner")
	if err := st.SetSandboxRef(t.Context(), run.ID, g.ref); err != nil {
		t.Fatal(err)
	}
	paused := time.Now().UTC().Truncate(time.Microsecond)
	g.exec(`UPDATE agent_runs SET paused_at = $2, paused_reason = 'stopped' WHERE id = $1`, run.ID, paused)
	g.gen = paused.UnixMicro()
	return g
}

// runner is a new claimed runner of owner.
func (g *moveRig) runner(owner string) types.Runner {
	g.t.Helper()
	rn, err := g.st.CreateRunner(g.t.Context(), newRunner(owner))
	if err != nil {
		g.t.Fatal(err)
	}
	if rn, err = g.st.ClaimRunner(g.t.Context(), rn.ID, rn.Owner, rn.KeyFingerprint, time.Now()); err != nil {
		g.t.Fatal(err)
	}
	return rn
}

func (g *moveRig) exec(q string, args ...any) {
	g.t.Helper()
	if _, err := g.pool.Exec(g.t.Context(), q, args...); err != nil {
		g.t.Fatal(err)
	}
}

func (g *moveRig) count(q string, args ...any) int {
	g.t.Helper()
	var n int
	if err := g.pool.QueryRow(g.t.Context(), q, args...).Scan(&n); err != nil {
		g.t.Fatal(err)
	}
	return n
}

// admit admits a move of the run from one runner to another at generation gen.
func (g *moveRig) admit(from, to types.Runner, fromRef string, gen int64, limits store.RunMoveLimits) (types.RunMove, error) {
	g.t.Helper()
	return g.st.AdmitRunMove(g.t.Context(), types.RunMove{RunID: g.run.ID, PauseGen: gen,
		FromPlacement: types.PlacementLocal, FromRunnerID: &from.ID, FromSandboxRef: fromRef, FromEvidenceSource: types.RunEvidenceRunnerAsserted,
		ToPlacement: types.PlacementLocal, ToRunnerID: &to.ID, ToEvidenceSource: types.RunEvidenceRunnerAsserted, ToTrustClass: types.RunnerTrustPersonal,
		MovedBy: g.run.CreatedBy}, limits)
}

// importing takes an admitted move to importing with its destination sandbox and create-time token id recorded.
func (g *moveRig) importing(mv types.RunMove, toRef, jti string) types.RunMove {
	g.t.Helper()
	ctx := g.t.Context()
	for _, step := range [][2]types.MoveState{{types.MoveAdmitted, types.MoveExporting}, {types.MoveExporting, types.MoveImporting}} {
		if ok, err := g.st.AdvanceRunMove(ctx, mv.ID, step[0], step[1]); err != nil || !ok {
			g.t.Fatalf("advance %s to %s: %v %v", step[0], step[1], ok, err)
		}
	}
	mv.ToSandboxRef, mv.ToTokenJTI = toRef, jti
	if ok, err := g.st.SetRunMoveDestination(ctx, mv); err != nil || !ok {
		g.t.Fatalf("set destination: %v %v", ok, err)
	}
	mv.State = types.MoveImporting
	return mv
}

func (g *moveRig) commit(mv types.RunMove) error {
	return g.st.CommitRunMove(g.t.Context(), store.RunMoveCommit{MoveID: mv.ID, RunID: g.run.ID, PlacementFilled: true})
}

// committed admits, imports and commits one move from one runner to another, and releases its hold.
func (g *moveRig) committed(from, to types.Runner, fromRef string) (types.RunMove, string) {
	g.t.Helper()
	mv, err := g.admit(from, to, fromRef, g.gen, store.RunMoveLimits{})
	if err != nil {
		g.t.Fatal(err)
	}
	toRef := "runner:" + to.ID.String() + "/moved-" + mv.ID.String()[:8]
	mv = g.importing(mv, toRef, "jti-"+mv.ID.String())
	if err := g.commit(mv); err != nil {
		g.t.Fatalf("commit: %v", err)
	}
	return mv, toRef
}

func TestPG_RunMoves_OneInFlightPerRun(t *testing.T) {
	g := newMoveRig(t)
	ctx := t.Context()
	mv, err := g.admit(g.src, g.dst, g.ref, g.gen, store.RunMoveLimits{})
	if err != nil || mv.State != types.MoveAdmitted {
		t.Fatalf("admit: %+v %v", mv, err)
	}
	if held, err := g.st.RunMoveInFlight(ctx, g.run.ID); err != nil || !held {
		t.Fatalf("in flight = %v %v, want held", held, err)
	}
	if _, err := g.admit(g.src, g.dst, g.ref, g.gen, store.RunMoveLimits{}); !errors.Is(err, store.ErrRunMoveBusy) {
		t.Fatalf("second admit: %v, want ErrRunMoveBusy", err)
	}
	// The partial unique index holds the rule under any writer, not only under admission's lock.
	if _, err := g.pool.Exec(ctx, `INSERT INTO run_moves (id, run_id, state, pause_gen) VALUES ($1, $2, 'importing', 0)`, uuid.New(), g.run.ID); err == nil {
		t.Fatal("a second move in flight was written past the unique index")
	}

	// The caps count moves in flight across the install and onto one runner.
	other := newRun(types.RunRunning)
	other.CreatedBy = g.run.CreatedBy
	other, err = g.st.CreateRun(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	second := &moveRig{t: t, pool: g.pool, st: g.st, run: other}
	if _, err := second.admit(g.src, g.dst, "", 1, store.RunMoveLimits{Install: 1}); !errors.Is(err, store.ErrRunMoveBusy) {
		t.Errorf("admit past the install cap: %v, want ErrRunMoveBusy", err)
	}
	if _, err := second.admit(g.src, g.dst, "", 1, store.RunMoveLimits{PerRunner: 1}); !errors.Is(err, store.ErrRunMoveBusy) {
		t.Errorf("admit past the per-runner cap: %v, want ErrRunMoveBusy", err)
	}

	// A move whose executor stopped answering no longer holds the run, and an ended move never does.
	g.exec(`UPDATE run_moves SET heartbeat_at = now() - interval '10 minutes' WHERE id = $1`, mv.ID)
	if held, err := g.st.RunMoveInFlight(ctx, g.run.ID); err != nil || held {
		t.Errorf("stale move in flight = %v %v, want not held", held, err)
	}
	if ok, err := g.st.FailStaleRunMove(ctx, mv.ID, "move_orphaned"); err != nil || !ok {
		t.Fatalf("fail stale: %v %v", ok, err)
	}
	if next, err := g.admit(g.src, g.dst, g.ref, g.gen, store.RunMoveLimits{Install: 1, PerRunner: 1}); err != nil {
		t.Fatalf("admit after the move ended: %v", err)
	} else if ok, err := g.st.EndRunMove(ctx, next.ID, types.MoveRefused, "move_destination_refused", types.MoveCleanupNone); err != nil || !ok {
		t.Fatalf("end: %v %v", ok, err)
	}
	if held, err := g.st.RunMoveInFlight(ctx, g.run.ID); err != nil || held {
		t.Errorf("after a refused move in flight = %v %v, want not held", held, err)
	}
}

func TestPG_RunMoves_PlacementsAppendOnly(t *testing.T) {
	g := newMoveRig(t)
	ctx := t.Context()
	if got, err := g.st.ListRunPlacements(ctx, g.run.ID); err != nil || len(got) != 0 {
		t.Fatalf("an unmoved run has placements %+v %v", got, err)
	}
	first, toRef := g.committed(g.src, g.dst, g.ref)
	got, err := g.st.ListRunPlacements(ctx, g.run.ID)
	if err != nil || len(got) != 2 {
		t.Fatalf("after one move: %+v %v", got, err)
	}
	origin := got[0]
	if origin.Seq != 1 || origin.Cause != types.PlacementCauseOrigin || origin.RunnerID == nil || *origin.RunnerID != g.src.ID || origin.MoveID != nil {
		t.Errorf("origin placement %+v, want seq 1 on the source", origin)
	}
	if p := got[1]; p.Seq != 2 || p.Cause != types.PlacementCauseMove || p.RunnerID == nil || *p.RunnerID != g.dst.ID || p.MoveID == nil || *p.MoveID != first.ID {
		t.Errorf("move placement %+v, want seq 2 on the destination", p)
	}
	if ok, err := g.st.ReleaseRunMove(ctx, first.ID, false); err != nil || !ok {
		t.Fatalf("release: %v %v", ok, err)
	}
	back, _ := g.committed(g.dst, g.src, toRef)
	got, err = g.st.ListRunPlacements(ctx, g.run.ID)
	if err != nil || len(got) != 3 || got[0].Seq != 1 || got[0].Cause != types.PlacementCauseOrigin || !got[0].StartedAt.Equal(origin.StartedAt) ||
		got[2].Seq != 3 || got[2].RunnerID == nil || *got[2].RunnerID != g.src.ID || got[2].MoveID == nil || *got[2].MoveID != back.ID {
		t.Errorf("after a second move: %+v %v, want the history kept and seq 3 appended", got, err)
	}
	if _, err := g.pool.Exec(ctx, `INSERT INTO run_placements (run_id, seq, cause) VALUES ($1, 1, 'move')`, g.run.ID); err == nil {
		t.Error("a second seq 1 was written over the origin")
	}
}

func TestPG_RunMoves_CommitWritesDestinationColumns(t *testing.T) {
	g := newMoveRig(t)
	ctx := t.Context()
	if _, err := g.st.NotePauseSettle(ctx, g.run.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	settles := func() int { return g.count(`SELECT count(*) FROM run_pause_settles WHERE run_id = $1`, g.run.ID) }

	// A refused commit changes nothing: a move at another pause generation, and one not yet importing.
	stale, err := g.admit(g.src, g.dst, g.ref, g.gen+1, store.RunMoveLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.commit(stale); !errors.Is(err, store.ErrRunMoveNotImporting) {
		t.Errorf("commit of an admitted move: %v, want ErrRunMoveNotImporting", err)
	}
	stale = g.importing(stale, "runner:"+g.dst.ID.String()+"/stale", "jti-stale")
	if err := g.commit(stale); !errors.Is(err, store.ErrRunMovePauseChanged) {
		t.Errorf("commit at another generation: %v, want ErrRunMovePauseChanged", err)
	}
	if run, _ := g.st.GetRun(ctx, g.run.ID); settles() != 1 || run.RunnerID == nil || *run.RunnerID != g.src.ID || run.SandboxRef != g.ref {
		t.Fatalf("a refused commit changed the run (runner %v ref %q) or its settle record (%d)", run.RunnerID, run.SandboxRef, settles())
	}
	if ok, err := g.st.EndRunMove(ctx, stale.ID, types.MoveFailed, "move_pause_changed", types.MoveCleanupPending); err != nil || !ok {
		t.Fatalf("end: %v %v", ok, err)
	}

	g.exec(`UPDATE agent_runs SET token_renewed_at = now() - interval '67 minutes', sandbox_absent_at = now(), token_prev_jti = 'old' WHERE id = $1`, g.run.ID)
	var before time.Time
	if err := g.pool.QueryRow(ctx, `SELECT now()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	mv, toRef := g.committed(g.src, g.dst, g.ref)
	var (
		runnerID        uuid.UUID
		ref, jti        string
		prev            *string
		renewed         time.Time
		absent          *time.Time
		filled          bool
		evidence, trust string
		pausedReason    string
		state           types.RunState
	)
	if err := g.pool.QueryRow(ctx, `SELECT runner_id, sandbox_ref, token_jti, token_prev_jti, token_renewed_at, sandbox_absent_at,
			placement_filled, evidence_source, COALESCE(runner_trust_class, ''), paused_reason, state FROM agent_runs WHERE id = $1`, g.run.ID).
		Scan(&runnerID, &ref, &jti, &prev, &renewed, &absent, &filled, &evidence, &trust, &pausedReason, &state); err != nil {
		t.Fatal(err)
	}
	if runnerID != g.dst.ID || ref != toRef || jti != "jti-"+mv.ID.String() || prev != nil || renewed.Before(before) || absent != nil ||
		!filled || evidence != string(types.RunEvidenceRunnerAsserted) || trust != string(types.RunnerTrustPersonal) {
		t.Errorf("run row: runner %s ref %q jti %q prev %v renewed %s (commit began after %s) absent %v filled %v evidence %q trust %q",
			runnerID, ref, jti, prev, renewed, before, absent, filled, evidence, trust)
	}
	if pausedReason != "stopped" || state != types.RunRunning {
		t.Errorf("run left %s %q, want RUNNING in the stopped pause the destination resumes", state, pausedReason)
	}
	if settles() != 0 {
		t.Error("the run's pause settle record outlived the commit")
	}
	got, err := g.st.GetRunMove(ctx, mv.ID)
	if err != nil || got.State != types.MoveCommitted || got.SourceRevoke != types.MoveSourceRevokePending || got.CommittedAt == nil || got.ReleasedAt != nil {
		t.Errorf("move row %+v %v, want committed with the source revoke pending", got, err)
	}
	if held, err := g.st.RunMoveInFlight(ctx, g.run.ID); err != nil || !held {
		t.Errorf("a committed move not yet released holds the run = %v %v, want held", held, err)
	}
	if err := g.commit(mv); !errors.Is(err, store.ErrRunMoveNotImporting) {
		t.Errorf("a second commit: %v, want ErrRunMoveNotImporting", err)
	}
}

func TestPG_RunSandboxSpecs_SealedRoundTrip(t *testing.T) {
	g := newMoveRig(t)
	ctx := t.Context()
	if _, err := g.st.GetRunSandboxSpec(ctx, g.run.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a run with no record: %v, want ErrNotFound", err)
	}
	sealed := []byte{0x01, 0x00, 0xfe, 'x', 0x00}
	if err := g.st.PutRunSandboxSpec(ctx, g.run.ID, sealed); err != nil {
		t.Fatal(err)
	}
	if got, err := g.st.GetRunSandboxSpec(ctx, g.run.ID); err != nil || !bytes.Equal(got, sealed) {
		t.Errorf("read back %x %v, want %x", got, err, sealed)
	}
	if err := g.st.PutRunSandboxSpec(ctx, g.run.ID, []byte("second")); err == nil {
		t.Error("a run's record was written twice")
	}
	if got, _ := g.st.GetRunSandboxSpec(ctx, g.run.ID); !bytes.Equal(got, sealed) {
		t.Errorf("the record changed to %x", got)
	}
}

func TestPG_RunnerStateIntake_IgnoresMovedRunRows(t *testing.T) {
	g := newMoveRig(t)
	row := runnerwire.RunState{RunID: g.run.ID, Ref: g.ref, SandboxPresent: true}
	page := func(rn types.Runner, rows ...runnerwire.RunState) (store.RunnerInventoryResult, error) {
		t.Helper()
		return finalPage(t, g.st, rn.ID, rows...)
	}
	wantIgnored := func(what string, rn types.Runner, row runnerwire.RunState) {
		t.Helper()
		if res, err := page(rn, row); err != nil || !slices.Equal(res.MoveIgnored, []uuid.UUID{g.run.ID}) {
			t.Errorf("%s: %+v %v, want the page accepted and the run's row ignored", what, res, err)
		}
	}
	wantRefused := func(what string, rn types.Runner, row runnerwire.RunState) {
		t.Helper()
		if res, err := page(rn, row); err == nil {
			t.Errorf("%s: %+v, want the page refused", what, res)
		}
	}

	// A runner no move names, and the destination of a refused move, which never had a sandbox, are refused.
	stranger, refusedDst := g.runner("move-stranger"), g.runner("move-refused-dest")
	wantRefused("a runner no move names", stranger, row)
	refused, err := g.admit(g.src, refusedDst, g.ref, g.gen, store.RunMoveLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := g.st.EndRunMove(t.Context(), refused.ID, types.MoveRefused, "move_destination_refused", types.MoveCleanupNone); err != nil || !ok {
		t.Fatalf("end: %v %v", ok, err)
	}
	wantRefused("a refused move's destination", refusedDst, row)

	// Before commit the destination's row is skipped; after it the source's is, and the destination's is taken.
	mv, err := g.admit(g.src, g.dst, g.ref, g.gen, store.RunMoveLimits{})
	if err != nil {
		t.Fatal(err)
	}
	toRef := "runner:" + g.dst.ID.String() + "/moved"
	mv = g.importing(mv, toRef, "jti-moved")
	dstRow := runnerwire.RunState{RunID: g.run.ID, Ref: toRef, SandboxPresent: true}
	wantIgnored("the destination before commit", g.dst, dstRow)
	if res, err := page(g.src, row); err != nil || len(res.MoveIgnored) != 0 {
		t.Errorf("the source before commit: %+v %v, want its bound run judged as before", res, err)
	}
	if err := g.commit(mv); err != nil {
		t.Fatal(err)
	}
	wantIgnored("the source after commit", g.src, row)
	if res, err := page(g.dst, dstRow); err != nil || len(res.MoveIgnored) != 0 {
		t.Errorf("the destination after commit: %+v %v, want its row taken", res, err)
	}
	wantRefused("a runner no move names, after commit", stranger, row)

	// A revoked runner's page fails before any row is read, named by a move or not.
	g.exec(`UPDATE runners SET state = 'revoked', revoked_at = now() WHERE id = $1`, g.src.ID)
	wantRefused("the revoked source", g.src, row)
}

// A run's sealed sandbox spec goes after its proxy config when the run goes terminal: in the delete, and in the purge
// behind it, which also finds a terminal run that has a spec and no proxy config. A live run's spec is kept, and a
// spec delete that fails never keeps the proxy config.
func TestPG_RunSandboxSpecs_PurgedAtTerminal(t *testing.T) {
	g := newMoveRig(t)
	ctx := t.Context()
	run := func(state types.RunState) uuid.UUID {
		t.Helper()
		r := newRun(types.RunRunning)
		r.CreatedBy = g.run.CreatedBy
		r, err := g.st.CreateRun(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		if err := g.st.PutRunSandboxSpec(ctx, r.ID, []byte("sealed")); err != nil {
			t.Fatal(err)
		}
		if state != types.RunRunning {
			if ok, err := g.st.UpdateRunStateIf(ctx, r.ID, types.RunRunning, state); err != nil || !ok {
				t.Fatalf("UpdateRunStateIf: %v %v", ok, err)
			}
		}
		return r.ID
	}
	specs := func(id uuid.UUID) int { return g.count(`SELECT count(*) FROM run_sandbox_specs WHERE run_id = $1`, id) }

	deleted := run(types.RunKilled)
	if err := g.st.PutRunProxyConfig(ctx, deleted, []byte("proxy")); err != nil {
		t.Fatal(err)
	}
	if err := g.st.DeleteRunProxyConfig(ctx, deleted); err != nil {
		t.Fatal(err)
	}
	if specs(deleted) != 0 || g.count(`SELECT count(*) FROM run_proxy_configs WHERE run_id = $1`, deleted) != 0 {
		t.Error("the terminal delete left the run's spec or proxy config")
	}

	specOnly, live := run(types.RunKilled), run(types.RunRunning)
	if _, err := g.st.PurgeTerminalRunProxyConfigs(ctx); err != nil {
		t.Fatal(err)
	}
	if specs(specOnly) != 0 {
		t.Error("the purge left a terminal run's spec")
	}
	if specs(live) != 1 {
		t.Error("the purge removed a live run's spec")
	}

	// A spec delete the database refuses, as on a split-role install whose app role has no DELETE on run_sandbox_specs
	// yet, is returned as ErrRunSandboxSpecKept and never keeps the proxy config. The refusal is a trigger raising the
	// permission error once per statement, as a missing grant does, since the fixture connects as a role no grant
	// binds; the database is this test's own.
	refused := run(types.RunKilled)
	g.exec(`CREATE FUNCTION refuse_spec_delete() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'permission denied for table run_sandbox_specs' USING ERRCODE = '42501'; END $$`)
	g.exec(`CREATE TRIGGER refuse_spec_delete BEFORE DELETE ON run_sandbox_specs FOR EACH STATEMENT EXECUTE FUNCTION refuse_spec_delete()`)
	configs := func(id uuid.UUID) int { return g.count(`SELECT count(*) FROM run_proxy_configs WHERE run_id = $1`, id) }
	if err := g.st.PutRunProxyConfig(ctx, refused, []byte("proxy")); err != nil {
		t.Fatal(err)
	}
	if err := g.st.DeleteRunProxyConfig(ctx, refused); !errors.Is(err, store.ErrRunSandboxSpecKept) {
		t.Errorf("DeleteRunProxyConfig of a refused spec delete = %v, want ErrRunSandboxSpecKept", err)
	}
	if configs(refused) != 0 || specs(refused) != 1 {
		t.Errorf("a refused spec delete: %d proxy configs and %d specs left, want 0 and 1", configs(refused), specs(refused))
	}
	if err := g.st.PutRunProxyConfig(ctx, refused, []byte("proxy")); err != nil {
		t.Fatal(err)
	}
	if n, err := g.st.PurgeTerminalRunProxyConfigs(ctx); !errors.Is(err, store.ErrRunSandboxSpecKept) || n < 1 {
		t.Errorf("PurgeTerminalRunProxyConfigs = %d, %v; want this run's proxy config and the refused spec purge's error", n, err)
	}
	if configs(refused) != 0 || specs(refused) != 1 {
		t.Errorf("a refused spec purge: %d proxy configs and %d specs left, want 0 and 1", configs(refused), specs(refused))
	}
}
