// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The end of a run that never started, and the drive it kept: the claim EndUnstartedRun takes in the statement
// that ends the run, the cleanup that finishes it, and the ends that take no claim. The PG tests are guarded by
// WARDYN_TEST_PG like their neighbours (a queue env or the resident dispatch fixture): skipped cleanly when unset,
// must PASS when set.

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/maskmanifest"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

var _ unstartedEndStore = store.PG{}

// t2Row is a kept drive's row as the claim columns read: whether it exists, its delete mark and its issue count.
type t2Row struct {
	exists  bool
	started *time.Time
	issues  int
}

type t2Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func t2DriveRow(t *testing.T, q t2Querier, runID uuid.UUID) t2Row {
	t.Helper()
	var r t2Row
	err := q.QueryRow(context.Background(), `SELECT delete_started_at, delete_issues FROM user_drives WHERE id = $1`,
		keptWorkFolderDriveID(runID)).Scan(&r.started, &r.issues)
	if errors.Is(err, pgx.ErrNoRows) {
		return r
	}
	if err != nil {
		t.Fatal(err)
	}
	r.exists = true
	return r
}

// t2Reclaims is the keep env's runner counting the substrate reclaims it is asked for, and answering each with err
// (nil: the object was already absent).
type t2Reclaims struct {
	*keepPoolRunner
	n   *atomic.Int32
	err error
}

func (r t2Reclaims) ReclaimDrive(ctx context.Context, m types.DriveMount) (runner.DriveReclaimOutcome, error) {
	r.n.Add(1)
	if r.err != nil {
		return "", r.err
	}
	return r.keepPoolRunner.ReclaimDrive(ctx, m)
}

// t2AfterEnd lands hook right after EndUnstartedRun committed: the instant between a run's FAILED write and
// whatever its caller does next (the revoke cascade, the cleanup).
type t2AfterEnd struct {
	store.PG
	hook func()
}

func (s *t2AfterEnd) EndUnstartedRun(ctx context.Context, id uuid.UUID, from, to types.RunState) (bool, error) {
	applied, err := s.PG.EndUnstartedRun(ctx, id, from, to)
	if hook := s.hook; hook != nil {
		s.hook = nil
		hook()
	}
	return applied, err
}

// pendingKeepRun is a keep_work_folder run that was queued and is now a freshly created PENDING one: its queue
// row is gone, as it is for a run the create handler has written and not yet dispatched. Its drive is the one
// the queued create made with it.
func (e *queueEnv) pendingKeepRun(pool types.RunnerPool) types.AgentRun {
	e.t.Helper()
	run := e.run(e.queuedLaunch(e.person, e.reread(pool), `,"keep_work_folder":true`).ID)
	e.exec(`DELETE FROM run_queue WHERE run_id = $1`, run.ID)
	e.exec(`UPDATE agent_runs SET state = 'PENDING' WHERE id = $1`, run.ID)
	return e.run(run.ID)
}

// Test 11, row 11: a run that fails PENDING->FAILED claims its kept drive in the statement that fails it. A later
// run of the person that asks for the drive between that write and the cleanup is refused, and the cleanup then
// reclaims the drive once. When the owner's own delete already holds the drive (row 8) the statement claims
// nothing and the cleanup makes no call.
func TestPG_UnstartedEnd_PendingAbortClaimsTheKeptDriveItself(t *testing.T) {
	t.Run("a later run asks between the failed write and the cleanup", func(t *testing.T) {
		e, pool := keepEnv(t, 3)
		var calls atomic.Int32
		e.srv.cfg.Runner = t2Reclaims{keepPoolRunner: &keepPoolRunner{e.runner}, n: &calls}
		run := e.pendingKeepRun(pool)
		var late *httpAnswer
		e.srv.cfg.Store = &t2AfterEnd{PG: e.pg, hook: func() {
			w := e.askForKeptDrive(pool, run)
			late = &httpAnswer{w.Code, w.Body.String()}
		}}
		e.srv.abortHalfBuiltRun(context.Background(), run.ID, true)("test: the create failed")
		if got := e.run(run.ID); got.State != types.RunFailed {
			t.Fatalf("run is %s, want FAILED by the abort", got.State)
		}
		if late == nil {
			t.Fatal("the abort never reached EndUnstartedRun, so the hook between the write and the cleanup never ran")
		}
		if late.code != http.StatusUnprocessableEntity || !strings.Contains(late.body, string(authz.ReasonDriveNotFound)) {
			t.Errorf("a later run asking for the drive after the FAILED write = %d %s, want 422 drive_not_found: the statement must have claimed it", late.code, late.body)
		}
		if e.keptDriveExists(run) || calls.Load() != 1 {
			t.Errorf("after the abort the kept drive exists = %v with %d reclaims, want it gone with one", e.keptDriveExists(run), calls.Load())
		}
	})
	t.Run("the owner's own delete already holds the drive", func(t *testing.T) {
		e, pool := keepEnv(t, 3)
		var calls atomic.Int32
		e.srv.cfg.Runner = t2Reclaims{keepPoolRunner: &keepPoolRunner{e.runner}, n: &calls}
		run := e.pendingKeepRun(pool)
		_, started, err := e.pg.BeginMemberDriveDelete(context.Background(), types.CanonicalUserSubject(run.CreatedBy), keptWorkFolderDriveID(run.ID))
		if err != nil {
			t.Fatalf("the owner's delete begins: %v", err)
		}
		e.srv.abortHalfBuiltRun(context.Background(), run.ID, true)("test: the create failed")
		if got := e.run(run.ID); got.State != types.RunFailed {
			t.Fatalf("run is %s, want FAILED by the abort", got.State)
		}
		row := t2DriveRow(t, e.poolEnv.pool, run.ID)
		if !row.exists || row.started == nil || !row.started.Equal(started) || row.issues != 0 || calls.Load() != 0 {
			t.Errorf("after the abort: drive %+v, %d reclaims; want the owner's mark %v untouched, count 0 and no call", row, calls.Load(), started)
		}
	})
}

type httpAnswer struct {
	code int
	body string
}

// Test 16: a run that ran and failed from RUNNING keeps its folder. The end is the plain state write, which takes
// no claim: the drive is unmarked, no reclaim is asked for, and a later run of the person is offered it.
func TestPG_UnstartedEnd_RunThatRanAndFailedKeepsItsFolder(t *testing.T) {
	e, pool := keepEnv(t, 3)
	ctx := context.Background()
	var calls atomic.Int32
	e.srv.cfg.Runner = t2Reclaims{keepPoolRunner: &keepPoolRunner{e.runner}, n: &calls}
	waiting := e.queuedLaunch(e.person, e.reread(pool), `,"keep_work_folder":true`)
	var occupant uuid.UUID
	if err := e.poolEnv.pool.QueryRow(ctx, `SELECT id FROM agent_runs WHERE created_by = 'occupant'`).Scan(&occupant); err != nil {
		t.Fatal(err)
	}
	e.free(e.run(occupant))
	e.sweep()
	e.waitStarted(waiting.ID)
	e.eventually("the kept run to be RUNNING", func() bool { return e.run(waiting.ID).State == types.RunRunning })
	if !e.srv.failAndRevoke(ctx, waiting.ID, types.RunRunning, "test: the agent could not start") {
		t.Fatal("the RUNNING failure was not applied")
	}
	run := e.run(waiting.ID)
	if run.State != types.RunFailed {
		t.Fatalf("run is %s, want FAILED", run.State)
	}
	if row := t2DriveRow(t, e.poolEnv.pool, run.ID); !row.exists || row.started != nil || row.issues != 0 {
		t.Errorf("after a RUNNING failure the drive row is %+v, want it kept, unmarked, count 0", row)
	}
	if calls.Load() != 0 {
		t.Errorf("a RUNNING failure asked for %d reclaims, want none", calls.Load())
	}
	if w := e.askForKeptDrive(e.reread(pool), run); w.Code != http.StatusCreated {
		t.Errorf("a later run asking for the kept drive after the run failed = %d %s, want 201", w.Code, w.Body)
	}
}

// Test 20: the queued or direct cleanup run a second time, after the first issued its one call (the count is
// odd, the mark still equals the run's end): it makes no call, so the substrate sees one reclaim. A takeover
// begun after that issues over the odd count, and an answer that touched nothing cannot clear it: the drive
// stays deleting.
func TestPG_UnstartedEnd_SecondCleanupMakesNoCall(t *testing.T) {
	e, pool := keepEnv(t, 3)
	ctx := context.Background()
	var calls atomic.Int32
	run := e.run(e.queuedLaunch(e.person, e.reread(pool), `,"keep_work_folder":true`).ID)
	if ended, err := e.pg.ExitQueuedRun(ctx, run.ID, types.RunFailed, "queue: runner_pool_removed", "pool removed"); err != nil || !ended {
		t.Fatalf("ExitQueuedRun = %v %v, want the run ended", ended, err)
	}
	endedAt := e.run(run.ID).EndedAt
	if endedAt == nil {
		t.Fatal("the ended run has no ended_at")
	}
	if row := t2DriveRow(t, e.poolEnv.pool, run.ID); row.started == nil || !row.started.Equal(*endedAt) || row.issues != 0 {
		t.Fatalf("claim after the exit: %+v, want the mark %v and count 0", row, *endedAt)
	}
	// The first cleanup's call is answered by an error: whether the substrate acted is not known, so the drive
	// stays marked with its one issue outstanding.
	e.srv.cfg.Runner = t2Reclaims{keepPoolRunner: &keepPoolRunner{e.runner}, n: &calls, err: errors.New("the reply was lost")}
	e.srv.finishQueuedKeptDrive(ctx, run.ID)
	if row := t2DriveRow(t, e.poolEnv.pool, run.ID); calls.Load() != 1 || !row.exists || row.started == nil || !row.started.Equal(*endedAt) || row.issues != 1 {
		t.Fatalf("after the first cleanup: %d reclaims, drive %+v; want one reclaim, the mark %v kept and count 1", calls.Load(), row, *endedAt)
	}
	e.srv.finishQueuedKeptDrive(ctx, run.ID)
	if row := t2DriveRow(t, e.poolEnv.pool, run.ID); calls.Load() != 1 || !row.exists || row.started == nil || !row.started.Equal(*endedAt) || row.issues != 1 {
		t.Errorf("after the second cleanup: %d reclaims, drive %+v; want still one reclaim and the row unchanged", calls.Load(), row)
	}
	// A takeover (the mark is aged past a minute) issues over the odd count: 1 becomes 3. The substrate says the
	// drive is in use, which touched nothing, but a claim made over an odd count never clears.
	e.exec(`UPDATE user_drives SET delete_started_at = delete_started_at - interval '2 minutes' WHERE id = $1`, keptWorkFolderDriveID(run.ID))
	d, started, err := e.pg.BeginMemberDriveDeleteAdmin(ctx, keptWorkFolderDriveID(run.ID))
	if err != nil || d.DeleteIssues != 1 {
		t.Fatalf("the takeover begins: count %d, %v; want count 1", d.DeleteIssues, err)
	}
	e.srv.cfg.Runner = t2Reclaims{keepPoolRunner: &keepPoolRunner{e.runner}, n: &calls, err: runner.ErrDriveInUse}
	res := e.srv.deleteMemberDrive(ctx, e.pg, d, started, "test_takeover")
	row := t2DriveRow(t, e.poolEnv.pool, run.ID)
	if res.issued != 3 || res.outcome != driveReclaimOutcomeRefused || calls.Load() != 2 {
		t.Errorf("the takeover: issued %d, outcome %q, %d reclaims; want issued 3, refused, 2 reclaims in all", res.issued, res.outcome, calls.Load())
	}
	if !row.exists || row.started == nil || !row.started.Equal(started) || row.issues != 3 {
		t.Errorf("after the takeover the drive row is %+v, want it still deleting: the mark %v and count 3", row, started)
	}
}

// t2Dispatch is one way a dispatch ends before or at its create. claims is whether a macOS VM keep run's end
// claims its drive there (the Mac was never asked to create a sandbox), and so the cleanup reclaims it once.
type t2Dispatch struct {
	name    string
	claims  bool
	grant   bool                           // the run carries a runner_resident value, handed to the runner before the create
	policy  string                         // the credential delivery policy; residentAllClassesPolicy when empty
	hint    string                         // part of the failure hint of the return that fails the run
	hintFor func(types.MachineKind) string // the same, when it depends on the machine kind
	arrange func(t *testing.T, f *residentFix, run types.AgentRun, p *dispatchParams)
}

const t2PostureRefusedPolicy = `{"classes":{"env_secret":{"mode":"runner_resident","require_posture":{"disk_encrypted":true}}}}`

// t2Exec runs statements on the fixture's database.
func t2Exec(t *testing.T, f *residentFix, stmts ...string) {
	t.Helper()
	for _, q := range stmts {
		if _, err := f.pool.Exec(t.Context(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// t2Returns are the ways a dispatch ends before or at its create, by the return that ends it. Each of the first
// eleven fails the run before the Mac was asked to create a sandbox, so a macOS VM run's end claims the drive it
// kept; the last two keep it (a failed create that was asked for, and a failure before the marker is installed).
var t2Returns = []t2Dispatch{
	{name: "keptMachineAtDispatch refuses the run's machine", claims: true, hint: keptMachineNotFoundMsg("gone"),
		arrange: func(_ *testing.T, _ *residentFix, run types.AgentRun, p *dispatchParams) {
			p.Machine = &keptMachinePlan{mode: keptModeResume, id: uuid.New(), name: "gone", owner: types.CanonicalUserSubject(run.CreatedBy)}
		}},
	{name: "the proxy config cannot be stored (first write)", claims: true, hint: "the run's proxy config could not be stored",
		arrange: func(t *testing.T, f *residentFix, _ types.AgentRun, _ *dispatchParams) {
			t2Exec(t, f,
				`CREATE FUNCTION t2_first_proxy_config() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
					IF NOT EXISTS (SELECT 1 FROM run_proxy_configs WHERE run_id = NEW.run_id) THEN RAISE EXCEPTION 't2: first proxy config write'; END IF;
					RETURN NEW; END $$`,
				`CREATE TRIGGER t2_first_proxy_config BEFORE INSERT ON run_proxy_configs FOR EACH ROW EXECUTE FUNCTION t2_first_proxy_config()`)
		}},
	{name: "the masking manifest cannot be completed", claims: true, hint: "the run's masking manifest could not be committed",
		arrange: func(t *testing.T, f *residentFix, _ types.AgentRun, _ *dispatchParams) {
			f.srv.cfg.MaskManifests = maskmanifest.New(f.pool, noKeys{}, f.srv.cfg.MaskRegistry)
			t2Exec(t, f,
				`CREATE FUNCTION t2_complete_manifest() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
					IF NEW.complete AND NOT OLD.complete THEN RAISE EXCEPTION 't2: manifest completion'; END IF;
					RETURN NEW; END $$`,
				`CREATE TRIGGER t2_complete_manifest BEFORE UPDATE ON run_mask_manifest FOR EACH ROW EXECUTE FUNCTION t2_complete_manifest()`)
		}},
	{name: "the proxy config cannot be stored again", claims: true, hint: "the run's proxy config could not be stored",
		arrange: func(t *testing.T, f *residentFix, _ types.AgentRun, _ *dispatchParams) {
			t2Exec(t, f,
				`CREATE FUNCTION t2_rewrite_proxy_config() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 't2: proxy config rewrite'; END $$`,
				`CREATE TRIGGER t2_rewrite_proxy_config BEFORE UPDATE ON run_proxy_configs FOR EACH ROW EXECUTE FUNCTION t2_rewrite_proxy_config()`)
		}},
	{name: "the credential classification refuses the run (admitAndStampPolicy)", claims: true, grant: true, policy: t2PostureRefusedPolicy},
	{name: "the proxy start cannot be recorded (before the create's lease)", claims: true, hint: "the run's proxy start could not be recorded",
		arrange: func(_ *testing.T, _ *residentFix, _ types.AgentRun, p *dispatchParams) {
			p.RunToken = "not-a-run-token"
		}},
	{name: "the delivery refuses the run (no create asked)", claims: true, grant: true, hint: "This run was not launched on your own runner:",
		arrange: func(_ *testing.T, f *residentFix, _ types.AgentRun, _ *dispatchParams) {
			f.side.mu.Lock()
			f.side.deliver = func(runnerwire.DeliverResidentArgs) error { return errors.New("the runner refused") }
			f.side.mu.Unlock()
		}},
	{name: "the lease is refused before the create is asked for", claims: true, hint: errRunnerContained.Error(),
		arrange: func(t *testing.T, f *residentFix, run types.AgentRun, _ *dispatchParams) {
			t2Exec(t, f, `INSERT INTO run_containment_pending (run_id, runner_id, kind, since) VALUES ('`+run.ID.String()+`', '`+f.rn.ID.String()+`', 'agent_only', now())`)
		}},
	{name: "the start-after-readback hold refuses the boot seed before the create", claims: true, hint: startAfterReadbackSeedHint,
		arrange: func(_ *testing.T, f *residentFix, _ types.AgentRun, _ *dispatchParams) {
			f.srv.cfg.StartAfterReadback = true
		}},
	{name: "recording is required and the recording store is off", claims: true, hint: runnerpool.RecordingStoreOffMsg(),
		arrange: func(t *testing.T, f *residentFix, _ types.AgentRun, _ *dispatchParams) {
			if _, _, err := f.st.PutRunnerPoolOrgDefaults(t.Context(), types.RunnerPoolDefaults{Recording: types.RecordingRequired}, "t2"); err != nil {
				t.Fatal(err)
			}
		}},
	{name: "the fresh start of the kept machine cannot be marked", claims: true,
		hint: "its kept machine's fresh start could not be recorded",
		arrange: func(t *testing.T, f *residentFix, run types.AgentRun, p *dispatchParams) {
			owner, prepared := types.CanonicalUserSubject(run.CreatedBy), time.Now().Add(-time.Hour).UTC()
			m, err := f.st.CreateKeptMachine(t.Context(), types.KeptMachine{Name: "dev", OwnerSubject: owner, RunnerID: f.rn.ID, Base: "base-1",
				BasePreparedAt: prepared, RunID: &run.ID, ExpiresAt: time.Now().Add(30 * 24 * time.Hour).UTC()}, 4)
			if err != nil {
				t.Fatal(err)
			}
			p.Machine = &keptMachinePlan{mode: keptModeFresh, id: m.ID, name: "dev", owner: owner, runner: f.rn.ID, base: "base-1", prepared: prepared, limit: 4, idleDays: 30}
			t2Exec(t, f,
				`CREATE FUNCTION t2_mark_reset() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 't2: mark reset'; END $$`,
				`CREATE TRIGGER t2_mark_reset BEFORE UPDATE ON kept_machines FOR EACH ROW
					WHEN (NEW.reset_run_id IS NOT NULL AND OLD.reset_run_id IS DISTINCT FROM NEW.reset_run_id) EXECUTE FUNCTION t2_mark_reset()`)
		}},
	// The create was asked for, so the runner's or the substrate's own refusal of it is the failure: a macOS VM
	// run is refused by the substrate's VM caps check before the wire (remote.CreateSandbox, checkVMCaps), a
	// container run reaches the runner side, whose handler answers "unexpected create_sandbox".
	{name: "the create was asked for and failed", claims: false, grant: true, hintFor: func(k types.MachineKind) string {
		if k.IsMacOSVM() {
			return remote.MacOSVMNotOffered("This runner")
		}
		return "unexpected " + runnerwire.MethodCreateSandbox
	}},
	{name: "the masking manifest cannot be started (before the marker)", claims: false, hint: "the run's masking manifest could not be committed",
		arrange: func(t *testing.T, f *residentFix, _ types.AgentRun, _ *dispatchParams) {
			dead, err := pgxpool.New(t.Context(), "postgres://wardyn@127.0.0.1:1/wardyn?connect_timeout=1&sslmode=disable")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(dead.Close)
			f.srv.cfg.MaskManifests = maskmanifest.New(dead, noKeys{}, f.srv.cfg.MaskRegistry)
		}},
}

// t2Reclaimer is the dispatch fixture's runner with the drive reclaim a kept drive's cleanup asks for, counted.
type t2Reclaimer struct {
	*fakeRunner
	n *atomic.Int32
}

func (r t2Reclaimer) ReclaimDrive(context.Context, types.DriveMount) (runner.DriveReclaimOutcome, error) {
	r.n.Add(1)
	return runner.DriveReclaimAlreadyAbsent, nil
}

func (t2Reclaimer) CanManageDrives(context.Context) error { return nil }

// t2Dispatched is a whole dispatch of a keep_work_folder run of kind and what it left: the fixture, the run and
// the substrate reclaims asked for. The drive is a Docker volume whatever the kind, so the cleanup's reclaim
// reaches the fixture's runner and is counted; the kind alone decides whether the dispatch installs the marker.
type t2Dispatched struct {
	kind     types.MachineKind
	f        *residentFix
	run      types.AgentRun
	reclaims *atomic.Int32
}

func t2Dispatch1(t *testing.T, kind types.MachineKind, c t2Dispatch) t2Dispatched {
	t.Helper()
	f := newResidentFix(t, nil, cmp.Or(c.policy, residentAllClassesPolicy))
	f.srv.cfg.RunConfigKey = make([]byte, kek.DEKSize)
	f.srv.cfg.DisableGitPATBroker = true
	f.srv.cfg.RunnerTarget = "docker"
	reclaims := new(atomic.Int32)
	f.srv.cfg.Runner = t2Reclaimer{&fakeRunner{}, reclaims}
	f.reportCaps(t, true)
	ctx := t.Context()
	run := f.startingRun(t, func(r *types.AgentRun) {
		r.State, r.MachineKind, r.Interactive = types.RunPending, kind, true
	})
	_, err := f.st.CreateMemberDrive(ctx, types.UserDrive{ID: keptWorkFolderDriveID(run.ID), Name: keptWorkFolderName(run),
		Backend: types.DriveBackendDockerVolume, OwnerSubject: types.CanonicalUserSubject(run.CreatedBy), CreatedBy: run.CreatedBy, KeptFromRun: &run.ID}, 3)
	if err != nil {
		t.Fatal(err)
	}
	p := dispatchParams{RunToken: runToken(t, f.srv, run.ID), Image: "wardyn/claude-code:latest", Interactive: true}
	if c.grant {
		if err := f.cfg.Secrets.(*memSecrets).Put(ctx, "op-secret", []byte("dispatched value")); err != nil {
			t.Fatal(err)
		}
		p.Policy = policyOf(f.classGrant(t, run, residentClassCases[0], "op-secret"))
	}
	if c.arrange != nil {
		c.arrange(t, f, run, &p)
	}
	f.srv.dispatchRun(ctx, run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), p)
	f.srv.WaitBackground()
	return t2Dispatched{kind, f, run, reclaims}
}

// requireFailed checks that the dispatch failed the run for the reason its case names, and returns the run.
func (d t2Dispatched) requireFailed(t *testing.T, c t2Dispatch) types.AgentRun {
	t.Helper()
	got, err := d.f.st.GetRun(t.Context(), d.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != types.RunFailed {
		t.Fatalf("run is %s, want FAILED", got.State)
	}
	if want := cmp.Or(c.hint, func() string {
		if c.hintFor != nil {
			return c.hintFor(d.kind)
		}
		return ""
	}()); want != "" && !strings.Contains(got.FailureHint, want) {
		t.Fatalf("failure hint %q, want it to contain %q: the run failed at some other return", got.FailureHint, want)
	}
	if c.policy == t2PostureRefusedPolicy {
		if refusal := d.f.dispatchRefusal(t, d.run.ID); refusal["reason"] != string(placement.ReasonRunnerPostureUnmet) {
			t.Fatalf("dispatch refusal = %v, want runner_posture_unmet", refusal)
		}
	}
	return got
}

// wantClaimedAndReclaimed: the end claimed the drive and the cleanup finished the claim with one reclaim.
func (d t2Dispatched) wantClaimedAndReclaimed(t *testing.T) {
	t.Helper()
	if row := t2DriveRow(t, d.f.pool, d.run.ID); row.exists || d.reclaims.Load() != 1 {
		t.Errorf("after the dispatch: drive %+v with %d reclaims, want it gone with exactly one", row, d.reclaims.Load())
	}
}

// wantKept: the drive is where it was: unmarked, count 0, visible to its owner, never reclaimed.
func (d t2Dispatched) wantKept(t *testing.T) {
	t.Helper()
	row := t2DriveRow(t, d.f.pool, d.run.ID)
	if !row.exists || row.started != nil || row.issues != 0 || d.reclaims.Load() != 0 {
		t.Errorf("after the dispatch: drive %+v with %d reclaims, want it kept, unmarked, count 0 and never reclaimed", row, d.reclaims.Load())
	}
	// A later run of the owner asking for the drive through the real admission path (authorizeRequestDrives:
	// reachable, not held by its ended run, not held for the upgrade) is offered it, on a mountable target.
	owner, id := types.CanonicalUserSubject(d.run.CreatedBy), keptWorkFolderDriveID(d.run.ID)
	sc, err := d.f.st.GetSiteConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	sc.WorkspaceProviders = keepSite(true, true, 3, 0).WorkspaceProviders
	if _, err := d.f.st.PutSiteConfig(t.Context(), sc); err != nil {
		t.Fatal(err)
	}
	drives, refusal := authorizeDrives(d.f.srv, personCtx(owner), drivesRequest(id.String()))
	if refusal != nil || len(drives) != 1 || drives[0].resolved.Drive.ID != id || drives[0].id != id.String() || drives[0].target == "" {
		t.Errorf("a later run asking for the kept drive = %+v, refusal %+v; want it selected: drive %s on a target", drives, refusal, id)
	}
}

// Test 22, H2: a macOS VM keep_work_folder run that fails at any dispatch return before its create was asked for
// (the six returns, and the create's own exits before the ask: the delivery refusal and the lease failure named)
// has its drive claimed by the statement that ends it, and the cleanup reclaims it once. A run whose create was
// asked and failed keeps the drive, and so does one that failed before the marker was installed.
func TestPG_UnstartedEnd_MacRunClaimsAtEveryReturnBeforeTheAsk(t *testing.T) {
	for _, c := range t2Returns {
		t.Run(c.name, func(t *testing.T) {
			d := t2Dispatch1(t, types.MachineMacOSVM, c)
			d.requireFailed(t, c)
			if c.claims {
				d.wantClaimedAndReclaimed(t)
			} else {
				d.wantKept(t)
			}
		})
	}
}

// Test 21, H1: a Docker keep_work_folder run that fails at any of the same returns never has its drive claimed:
// the marker is a macOS VM run's alone, so the drive stays unmarked, visible and attachable.
func TestPG_UnstartedEnd_ContainerRunNeverClaimsItsDrive(t *testing.T) {
	for _, c := range t2Returns {
		t.Run(c.name, func(t *testing.T) {
			d := t2Dispatch1(t, types.MachineContainer, c)
			d.requireFailed(t, c)
			d.wantKept(t)
		})
	}
}

// Test 17, G1: a macOS VM run that another writer kills during its dispatch is not ended by the dispatch's own
// write (it loses the compare-and-swap), so no claim is taken and the drive is kept. The control is the same
// dispatch ended by its own refusal, which claims and reclaims once.
func TestPG_UnstartedEnd_MacRunKilledByAnotherWriterKeepsItsDrive(t *testing.T) {
	refuse := func(kill bool) t2Dispatch {
		return t2Dispatch{name: "delivery", claims: !kill, grant: true,
			arrange: func(t *testing.T, f *residentFix, run types.AgentRun, _ *dispatchParams) {
				f.side.mu.Lock()
				defer f.side.mu.Unlock()
				f.side.deliver = func(runnerwire.DeliverResidentArgs) error {
					if kill {
						if ok, err := f.srv.casRunState(context.Background(), run.ID, types.RunStarting, types.RunKilled); err != nil || !ok {
							t.Errorf("the other writer's kill: applied %v, %v", ok, err)
						}
					}
					return errors.New("the runner refused")
				}
			}}
	}
	t.Run("the dispatch's own refusal ends the run", func(t *testing.T) {
		c := refuse(false)
		d := t2Dispatch1(t, types.MachineMacOSVM, c)
		d.requireFailed(t, c)
		d.wantClaimedAndReclaimed(t)
	})
	t.Run("another writer killed it during the delivery", func(t *testing.T) {
		d := t2Dispatch1(t, types.MachineMacOSVM, refuse(true))
		if got := d.f.runState(t, d.run.ID); got != types.RunKilled {
			t.Fatalf("run is %s, want KILLED by the other writer", got)
		}
		d.wantKept(t)
	})
}

// A dispatch end of a failAndRevoke is the plain state write unless its context carries the marker and no create
// was asked for yet: only then does it end through EndUnstartedRun, which claims the run's kept drive.
func TestUnstartedEnd_FailAndRevokeRoutesTheEnd(t *testing.T) {
	asked := func(ctx context.Context) context.Context {
		u := &unstartedEnd{}
		u.asked.Store(true)
		return withUnstartedEnd(ctx, u)
	}
	for _, tc := range []struct {
		name         string
		ctx          func(context.Context) context.Context
		plain, ended int32
	}{
		{"no marker", func(ctx context.Context) context.Context { return ctx }, 1, 0},
		{"marker, no create asked", func(ctx context.Context) context.Context { return withUnstartedEnd(ctx, &unstartedEnd{}) }, 0, 1},
		{"marker, create asked", asked, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			st := &t2EndStub{}
			cfg := baseTestConfig(h, st)
			cfg.Broker = h.broker
			srv := New(cfg)
			if !srv.failAndRevoke(tc.ctx(context.Background()), uuid.New(), types.RunStarting, "test: forced failure") {
				t.Fatal("the end was not applied")
			}
			if st.plain.Load() != tc.plain || st.ended.Load() != tc.ended {
				t.Errorf("plain writes %d, unstarted ends %d; want %d and %d", st.plain.Load(), st.ended.Load(), tc.plain, tc.ended)
			}
		})
	}
}

// t2EndStub counts the two run-state statements a failure can end a run with.
type t2EndStub struct {
	store.Store
	plain, ended atomic.Int32
}

func (s *t2EndStub) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	return types.AgentRun{}, nil
}

func (s *t2EndStub) UpdateRunStateIf(context.Context, uuid.UUID, types.RunState, types.RunState) (bool, error) {
	s.plain.Add(1)
	return true, nil
}

func (s *t2EndStub) EndUnstartedRun(context.Context, uuid.UUID, types.RunState, types.RunState) (bool, error) {
	s.ended.Add(1)
	return true, nil
}

func (s *t2EndStub) SetRunFailureHint(context.Context, uuid.UUID, string) error { return nil }
