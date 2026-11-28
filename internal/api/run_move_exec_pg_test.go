// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The move's executor on a bare store.PG: the commit's fences, the placement history, the caps, the orphan rule and
// the trust class, with the ports set to their fakes and a runner that exports and imports in memory.

// movePGFixture is a RUNNING interactive run on a real database, recorded at dispatch, with run_move on.
type movePGFixture struct {
	ctx   context.Context
	st    store.PG
	srv   *Server
	mr    *moveRunner
	fk    *moveFakes
	audit *syncAudit
	run   types.AgentRun
}

// newMoveRunner is the in-memory runner with no fixture under it.
func newMoveRunner() *moveRunner {
	base := &reviveRunner{lostRunner: &lostRunner{leaseRunner: &leaseRunner{finalizeTailRunner: &finalizeTailRunner{fakeRunner: &fakeRunner{}}}}}
	return &moveRunner{reviveRunner: base, drained: true, files: []moveFile{moveDir("home/agent/work"), moveReg("home/agent/work/README.md", "hello")}}
}

// pgMoveRows is the fakes' test-only statements on agent_runs.
type pgMoveRows struct{ st store.PG }

func (r pgMoveRows) moveMarkStopped(ctx context.Context, runID uuid.UUID, at time.Time) error {
	_, err := r.st.Pool.Exec(ctx, `UPDATE agent_runs SET paused_at = $2, paused_reason = 'stopped' WHERE id = $1`, runID, at)
	return err
}

func (r pgMoveRows) moveStampToken(ctx context.Context, runID uuid.UUID) error {
	_, err := r.st.Pool.Exec(ctx, `UPDATE agent_runs SET token_renewed_at = now() WHERE id = $1`, runID)
	return err
}

func newMovePGFixture(t *testing.T, mutate func(*types.AgentRun)) *movePGFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	st := store.NewPG(throwawayPGPool(t))
	if _, err := st.PutSiteConfig(ctx, siteFeaturesDoc(t, runMoveStoredOn)); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.BaseCtx, cfg.RunConfigKey, cfg.ControlPlaneURL = ctx, make([]byte, 32), "http://127.0.0.1:8080"
	cfg.RunnerOrgURL = admissionOrgURL
	f := &movePGFixture{ctx: ctx, st: st, mr: newMoveRunner(), audit: &syncAudit{}}
	cfg.Runner, cfg.Audit = f.mr, f.audit
	f.srv = New(cfg)
	t.Cleanup(f.srv.bg.Wait)
	withMoveGateOpen(t)
	f.fk = withMovePortFakes(t)
	f.fk.srv, f.fk.rw = f.srv, pgMoveRows{st}
	run := newFinalizeRun()
	run.CreatedBy, run.Placement, run.Interactive, run.SandboxRef = "mover@example.com", types.PlacementRemote, true, "ref-src"
	run.EvidenceSource, run.RunnerPoolExecutor = types.RunEvidenceSubstrate, "kind-a"
	if mutate != nil {
		mutate(&run)
	}
	run.State = types.RunRunning
	var err error
	if f.run, err = st.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	pc, err := runner.BuildProxyConfig(f.run.ID, runner.ProxyConfig{RunToken: "tok", ControlPlaneURL: cfg.ControlPlaneURL,
		Policy: types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}}}, runner.ProxyListenPort)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.srv.storeRunProxyConfig(ctx, f.run.ID, pc); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.storeRunSandboxSpec(ctx, f.run.ID, runSandboxSpecRecord{Image: "img", ConfinementClass: types.CC1, Interactive: true}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *movePGFixture) move(t *testing.T, to string) (int, string) {
	t.Helper()
	w := do(t, f.srv, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/move", adminToken, `{"to":"`+to+`"}`)
	f.srv.bg.Wait()
	return w.Code, w.Body.String()
}

func (f *movePGFixture) moves(t *testing.T) []types.RunMove {
	t.Helper()
	moves, err := f.st.ListRunMoves(f.ctx, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return moves
}

func (f *movePGFixture) current(t *testing.T) types.AgentRun {
	t.Helper()
	run, err := f.st.GetRun(f.ctx, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// TestMoveExec_CommitFencedByPauseGen: a Resume that changes the run's pause between the stop and commit fails the
// move move_pause_changed; the run row still names the source and the destination is torn down.
func TestMoveExec_CommitFencedByPauseGen(t *testing.T) {
	f := newMovePGFixture(t, nil)
	f.mr.onImport = func() {
		if _, err := f.st.Pool.Exec(f.ctx, `UPDATE agent_runs SET paused_at = paused_at + interval '1 second' WHERE id = $1`, f.run.ID); err != nil {
			t.Error(err)
		}
	}
	if code, body := f.move(t, "org"); code != http.StatusAccepted {
		t.Fatalf("move = %d %s", code, body)
	}
	moves := f.moves(t)
	if len(moves) != 1 || moves[0].State != types.MoveFailed || moves[0].Failure != reasonMovePauseChanged {
		t.Fatalf("moves = %+v, want one failed move_pause_changed", moves)
	}
	if cur := f.current(t); cur.SandboxRef != "ref-src" || cur.Placement != types.PlacementRemote {
		t.Errorf("run row = %s on %s, want the source", cur.Placement, cur.SandboxRef)
	}
	if ref := moves[0].ToSandboxRef; ref == "" || !slices.Contains(f.mr.killed(), ref) {
		t.Errorf("destination %q not torn down (kills %v)", ref, f.mr.killed())
	}
}

// TestMoveAudit_ShowsBothPlacements: a committed move writes run_placements seq 1 (origin) and 2 (move), and its one
// run.move success row names both placements' evidence sources.
func TestMoveAudit_ShowsBothPlacements(t *testing.T) {
	f := newMovePGFixture(t, nil)
	if code, body := f.move(t, "org"); code != http.StatusAccepted {
		t.Fatalf("move = %d %s", code, body)
	}
	moves := f.moves(t)
	if len(moves) != 1 || moves[0].State != types.MoveCommitted {
		t.Fatalf("moves = %+v, want one committed", moves)
	}
	ps, err := f.st.ListRunPlacements(f.ctx, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 || ps[0].Seq != 1 || ps[0].Cause != types.PlacementCauseOrigin || ps[1].Seq != 2 ||
		ps[1].Cause != types.PlacementCauseMove || ps[1].MoveID == nil || *ps[1].MoveID != moves[0].ID {
		t.Errorf("placements = %+v, want origin then this move", ps)
	}
	var rows int
	for _, ev := range f.audit.snapshot() {
		if ev.Action != "run.move" || ev.Outcome != "success" {
			continue
		}
		rows++
		var data map[string]any
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatal(err)
		}
		if data["evidence_source"] != string(moves[0].ToEvidenceSource) || data["prior_evidence_source"] != string(moves[0].FromEvidenceSource) {
			t.Errorf("success row = %v, want evidence %s prior %s", data, moves[0].ToEvidenceSource, moves[0].FromEvidenceSource)
		}
	}
	if rows != 1 {
		t.Errorf("run.move success rows = %d, want 1", rows)
	}
}

// TestMoveExec_ConcurrencyCap: four moves in flight on the install refuse a fifth 409 move_busy, and nothing of the
// fifth run is stopped.
func TestMoveExec_ConcurrencyCap(t *testing.T) {
	f := newMovePGFixture(t, nil)
	for range moveInstallCap {
		other := newFinalizeRun()
		other.State = types.RunRunning
		o, err := f.st.CreateRun(f.ctx, other)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.st.AdmitRunMove(f.ctx, types.RunMove{RunID: o.ID, ToPlacement: types.PlacementRemote}, store.RunMoveLimits{}); err != nil {
			t.Fatal(err)
		}
	}
	code, body := f.move(t, "org")
	if reason, _ := moveAnswer(t, body); code != http.StatusConflict || reason != reasonMoveBusy {
		t.Fatalf("fifth move = %d %s, want 409 move_busy", code, body)
	}
	if f.fk.count("stop") != 0 || len(f.moves(t)) != 0 {
		t.Errorf("stop %d rows %d, want nothing", f.fk.count("stop"), len(f.moves(t)))
	}
}

// TestMoveExec_OrphanedInFlightRowFails: a row left importing whose hold went stale, and one whose heartbeat was
// never written, are failed move_orphaned by the sweep, their destinations torn down and audited; the run is free
// for a new move, and the old executor's late commit is refused.
func TestMoveExec_OrphanedInFlightRowFails(t *testing.T) {
	f := newMovePGFixture(t, nil)
	mv, err := f.st.AdmitRunMove(f.ctx, types.RunMove{RunID: f.run.ID, ToPlacement: types.PlacementRemote}, store.RunMoveLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(f.ctx, `UPDATE run_moves SET state = 'importing', to_sandbox_ref = 'ref-orphan', to_token_jti = 'jti-orphan',
		heartbeat_at = now() - interval '10 minutes' WHERE id = $1`, mv.ID); err != nil {
		t.Fatal(err)
	}
	other := newFinalizeRun()
	other.State = types.RunRunning
	o, err := f.st.CreateRun(f.ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	never, err := f.st.AdmitRunMove(f.ctx, types.RunMove{RunID: o.ID, ToPlacement: types.PlacementRemote}, store.RunMoveLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(f.ctx, `UPDATE run_moves SET created_at = now() - interval '10 minutes', heartbeat_at = NULL WHERE id = $1`, never.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.sweepRunMoves(f.ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{mv.ID, never.ID} {
		got, err := f.st.GetRunMove(f.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != types.MoveFailed || got.Failure != reasonMoveOrphaned || got.Cleanup != types.MoveCleanupDone {
			t.Errorf("row %s = %s %q cleanup %s, want failed move_orphaned, cleaned", id, got.State, got.Failure, got.Cleanup)
		}
	}
	if !slices.Contains(f.mr.killed(), "ref-orphan") {
		t.Errorf("orphaned destination not torn down (kills %v)", f.mr.killed())
	}
	if held, err := f.st.RunMoveInFlight(f.ctx, f.run.ID); err != nil || held {
		t.Errorf("in flight = %v %v, want the run free", held, err)
	}
	if err := f.st.CommitRunMove(f.ctx, store.RunMoveCommit{MoveID: mv.ID, RunID: f.run.ID}); err != store.ErrRunMoveNotImporting {
		t.Errorf("late commit = %v, want ErrRunMoveNotImporting", err)
	}
	if code, body := f.move(t, "org"); code != http.StatusAccepted {
		t.Errorf("a new move after the orphan = %d %s, want 202", code, body)
	}
}

// laptopMoveAtCommit is a move of the fixture's run to its owner's laptop, judged at step 2 and admitted, at the
// point the executor commits: stopped at its generation, the destination recorded and the row importing.
func (f *movePGFixture) laptopMoveAtCommit(t *testing.T) (*moveExec, types.Runner) {
	t.Helper()
	rid := uuid.New()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	rn, err := f.st.CreateRunner(f.ctx, types.Runner{ID: rid, Owner: f.run.CreatedBy, Name: "laptop", PublicKey: pub, KeyFingerprint: rid.String(),
		Version: "0.11.0", OrgURLSHA256: federation.OrgURLSHA256(admissionOrgURL), Limits: roomyRunnerLimits()})
	if err != nil {
		t.Fatal(err)
	}
	if rn, err = f.st.ClaimRunner(f.ctx, rn.ID, rn.Owner, rn.KeyFingerprint, time.Now()); err != nil {
		t.Fatal(err)
	}
	registerMoveRunnerCaps(t, f.srv, rn.ID, moveCapsAll)
	sc, err := f.st.GetSiteConfig(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := f.srv.loadRunSandboxSpec(f.ctx, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	d, ref, err := f.srv.resolveMoveDestination(f.ctx, f.run, types.MoveDestination{Kind: types.MoveToLaptop}, sc)
	if err != nil || ref != nil || d.RunnerID == nil || *d.RunnerID != rn.ID {
		t.Fatalf("laptop = %+v %+v %v, want the owner's runner", d.RunnerID, ref, err)
	}
	caps, ref, err := f.srv.moveCapsRefusal(f.ctx, f.run, rec, d)
	if err != nil || ref != nil {
		t.Fatalf("caps = %+v %v", ref, err)
	}
	plan, ref, err := f.srv.judgeMoveDestination(f.ctx, f.run, rec, d, caps, sc)
	if err != nil || ref != nil {
		t.Fatalf("judgement = %+v %v", ref, err)
	}
	from, err := f.st.RunRunnerTrustClass(f.ctx, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	mv, err := f.st.AdmitRunMove(f.ctx, types.RunMove{RunID: f.run.ID, FromPlacement: types.PlacementRemote, FromSandboxRef: "ref-src",
		FromEvidenceSource: types.RunEvidenceSubstrate, FromTrustClass: from, ToPlacement: types.PlacementLocal, ToRunnerID: &rn.ID,
		ToEvidenceSource: types.RunEvidenceRunnerAsserted}, store.RunMoveLimits{})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Microsecond)
	if err := (pgMoveRows{f.st}).moveMarkStopped(f.ctx, f.run.ID, at); err != nil {
		t.Fatal(err)
	}
	mv.PauseGen, mv.ToTrustClass, mv.ToSandboxRef, mv.ToTokenJTI = at.UnixMicro(), plan.Trust, "ref-dest", "jti-dest"
	if _, err := f.st.SetRunMovePauseGen(f.ctx, mv.ID, mv.PauseGen); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.SetRunMoveDestination(f.ctx, mv); err != nil {
		t.Fatal(err)
	}
	for _, step := range [][2]types.MoveState{{types.MoveAdmitted, types.MoveExporting}, {types.MoveExporting, types.MoveImporting}} {
		if _, err := f.st.AdvanceRunMove(f.ctx, mv.ID, step[0], step[1]); err != nil {
			t.Fatal(err)
		}
	}
	cur := f.current(t)
	return &moveExec{s: f.srv, ms: f.st, mv: mv, run: cur, rec: rec, dest: d, caps: caps, plan: plan, sc: sc,
		actorType: types.ActorHuman, principal: cur.CreatedBy, maxBytes: 1 << 20, drained: true,
		manifest: runner.CarriedManifest{Digest: "d0"}}, rn
}

// wantPolicyChanged asserts a commit that ended move_policy_changed naming layer, with the run row left at its source.
func (f *movePGFixture) wantPolicyChanged(t *testing.T, end *moveEnd, layer string) {
	t.Helper()
	if end == nil || end.reason != reasonMovePolicyChanged || !strings.HasSuffix(end.msg, ": "+layer) {
		t.Fatalf("commit = %+v, want move_policy_changed naming %s", end, layer)
	}
	if cur := f.current(t); cur.RunnerID != nil || cur.Placement != types.PlacementRemote {
		t.Errorf("run row = %s on %v, want the source", cur.Placement, cur.RunnerID)
	}
}

// TestMoveExec_ClusterToLaptopRevivesWithoutTrustChanged: a run moved from the organisation's executors to its
// owner's laptop commits with the destination's trust class on the run, the move row and its placement, and a revive
// at the destination is not refused trust_changed. A trust class that changes between step 2 and commit fails the
// move move_policy_changed, layer trust_class. The laptop's import and lease are not driven: they need a live runner.
func TestMoveExec_ClusterToLaptopRevivesWithoutTrustChanged(t *testing.T) {
	f := newMovePGFixture(t, nil)
	x, _ := f.laptopMoveAtCommit(t)
	if x.plan.Trust != types.RunnerTrustPersonal {
		t.Errorf("destination trust = %q, want personal", x.plan.Trust)
	}
	if end := x.commit(f.ctx); end != nil {
		t.Fatalf("commit = %+v", end)
	}
	cur := f.current(t)
	if class, err := f.st.RunRunnerTrustClass(f.ctx, f.run.ID); err != nil || class != types.RunnerTrustPersonal {
		t.Errorf("runner_trust_class = %q %v, want personal", class, err)
	}
	if err := f.srv.trustChangedRefusal(f.ctx, cur, types.RunnerTrustPersonal); err != nil {
		t.Errorf("a revive at the destination is refused: %v", err)
	}
	ps, _ := f.st.ListRunPlacements(f.ctx, f.run.ID)
	if len(ps) != 2 || ps[0].TrustClass != "" || ps[1].TrustClass != types.RunnerTrustPersonal {
		t.Errorf("placements = %+v, want seq 1 '' and seq 2 personal", ps)
	}
	mv, err := f.st.GetRunMove(f.ctx, x.mv.ID)
	if err != nil || mv.State != types.MoveCommitted || mv.FromTrustClass != x.mv.FromTrustClass || mv.ToTrustClass != types.RunnerTrustPersonal {
		t.Errorf("move row = %s from %q to %q (%v), want committed from %q to personal", mv.State, mv.FromTrustClass, mv.ToTrustClass, err,
			x.mv.FromTrustClass)
	}
	var absent bool
	if err := f.st.Pool.QueryRow(f.ctx, `SELECT sandbox_absent_at IS NULL FROM agent_runs WHERE id = $1`, f.run.ID).Scan(&absent); err != nil || !absent {
		t.Errorf("sandbox_absent_at cleared = %v %v, want NULL after commit", absent, err)
	}

	t.Run("trust class changed before commit", func(t *testing.T) {
		g := newMovePGFixture(t, nil)
		x, rn := g.laptopMoveAtCommit(t)
		if _, err := store.SetRunnerTrustClassQ(g.ctx, g.st.Pool, rn.ID, types.RunnerTrustManagedDevice, "secops", time.Now()); err != nil {
			t.Fatal(err)
		}
		g.wantPolicyChanged(t, x.commit(g.ctx), "trust_class")
	})
}

// TestMoveExec_CommitJudgesTheRunAsItIsNow: commit reads the site config again: require_managed_device turned on, or
// moving onto a person's runner turned off, between step 2 and commit fails the move move_policy_changed.
func TestMoveExec_CommitJudgesTheRunAsItIsNow(t *testing.T) {
	for name, c := range map[string]struct {
		site  func(*types.SiteConfig)
		layer string
	}{
		"require_managed_device": {func(sc *types.SiteConfig) {
			sc.Runners = &types.RunnerSettings{Enabled: true, RequireManagedDevice: true}
		}, "managed_device"},
		"to_personal_runners": {func(sc *types.SiteConfig) {
			sc.FeatureSwitches = siteFeaturesDoc(t, `{"features":{"run_move":{"enabled":true,"max_carried_gib":9}}}`).FeatureSwitches
		}, reasonMoveToPersonalOff},
	} {
		t.Run(name, func(t *testing.T) {
			f := newMovePGFixture(t, nil)
			x, _ := f.laptopMoveAtCommit(t)
			sc, err := f.st.GetSiteConfig(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			c.site(&sc)
			if _, err := f.st.PutSiteConfig(f.ctx, sc); err != nil {
				t.Fatal(err)
			}
			f.wantPolicyChanged(t, x.commit(f.ctx), c.layer)
		})
	}
}

// TestMovePolicy_CeilingDeniesAtStepTwoAndCommit: a restriction the run holds that now denies a component the run
// uses refuses the move at step 2, before anything stops, with layer component; the same deny written between
// step 2 and commit fails the move move_policy_changed.
func TestMovePolicy_CeilingDeniesAtStepTwoAndCommit(t *testing.T) {
	f := newMovePGFixture(t, nil)
	cs, ok := any(f.st).(store.ComponentStore)
	if !ok {
		t.Fatal("the Postgres store has no component seam")
	}
	compID := uuid.New()
	def := types.ComponentDefinition{Hosts: []string{"github.com"}}
	if _, err := cs.CreateComponent(f.ctx, types.Component{ID: compID, Name: "Prod Deploy", Definition: def}); err != nil {
		t.Fatal(err)
	}
	if err := cs.PutRunComponents(f.ctx, f.run.ID, []types.RunComponent{{RunID: f.run.ID, ComponentID: &compID, Name: "Prod Deploy",
		Definition: def, Version: 1}}); err != nil {
		t.Fatal(err)
	}
	r := types.Restriction{ID: uuid.New(), Name: "move-deny", Limits: types.RestrictionLimits{DenyComponents: strs(uuid.NewString())},
		Contact: &policyref.Contact{Owner: "secops", Email: "secops@corp.example"}}
	deny := func(id string) {
		t.Helper()
		r.Limits.DenyComponents = strs(id)
		if _, err := f.st.PutRestriction(f.ctx, r, "seed"); err != nil {
			t.Fatal(err)
		}
	}
	deny(compID.String())
	if err := f.st.SetRunRestrictionIDs(f.ctx, f.run.ID, []uuid.UUID{r.ID}); err != nil {
		t.Fatal(err)
	}
	code, body := f.move(t, "org")
	if reason, layer := moveAnswer(t, body); code != http.StatusForbidden || reason != reasonMoveDestinationRefused || layer != "component" {
		t.Fatalf("move = %d %s, want 403 move_destination_refused layer component", code, body)
	}
	if f.fk.count("stop") != 0 {
		t.Errorf("stops = %d, want none", f.fk.count("stop"))
	}
	deny(uuid.NewString())
	f.mr.onImport = func() { deny(compID.String()) }
	if code, body := f.move(t, "org"); code != http.StatusAccepted {
		t.Fatalf("second move = %d %s", code, body)
	}
	moves := f.moves(t)
	if len(moves) != 2 || moves[1].State != types.MoveFailed || moves[1].Failure != reasonMovePolicyChanged {
		t.Errorf("moves = %+v, want the second failed move_policy_changed", moves)
	}
}
