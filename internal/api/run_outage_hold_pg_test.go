// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The reasons the sweeps give a run they end (run_lost.go): they are asserted exactly, since they are what the run's
// owner reads.
const (
	ohStrictReason     = "the run's strict outage terms end it: its network was not confirmed within its allowance"
	ohUnattendedReason = "control-plane outage: this policy ends unattended runs after an outage"
	ohLapsedReason     = "run token lapsed: the control plane was unreachable past its renew window"
)

var (
	// ohPauses is the terms of a run whose proxy pauses and asks to recover, on the default allowance.
	ohPauses = types.OutageTerms{AllowanceSec: 3600, ProxyPauses: true}
	// ohStrict30 is a strict run with a 30 minute allowance, whose token was last stamped 70 minutes ago in the
	// tests below, so its allowance is spent.
	ohStrict30 = types.OutageTerms{AllowanceSec: 1800, Strict: true, ProxyPauses: true}
)

func ohExec(t *testing.T, st store.PG, q string, args ...any) {
	t.Helper()
	if _, err := st.Pool.Exec(context.Background(), q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// ohTerms records id's outage terms and whether its proxy pauses, as the dispatch and the proxy's loaded report do.
func ohTerms(t *testing.T, st store.PG, id uuid.UUID, terms types.OutageTerms) {
	t.Helper()
	if _, err := st.SetRunOutageTerms(context.Background(), id, terms); err != nil {
		t.Fatalf("record the run's outage terms: %v", err)
	}
	if err := st.SetRunProxyPauses(context.Background(), id, terms.ProxyPauses); err != nil {
		t.Fatalf("record that the run's proxy pauses: %v", err)
	}
}

// ohOpenWindow writes an open outage window that began startedAgo before the database's now.
func ohOpenWindow(t *testing.T, st store.PG, kind string, runnerID *uuid.UUID, startedAgo time.Duration) {
	t.Helper()
	ohExec(t, st, `INSERT INTO outage_windows (kind, runner_id, started_at) VALUES ($1, $2, now() - $3::interval)`,
		kind, runnerID, startedAgo.String())
}

// ohClosedWindow writes an outage window that began startedAgo and ended endedAgo before the database's now.
func ohClosedWindow(t *testing.T, st store.PG, kind string, runnerID *uuid.UUID, startedAgo, endedAgo time.Duration) {
	t.Helper()
	ohExec(t, st, `INSERT INTO outage_windows (kind, runner_id, started_at, ended_at) VALUES ($1, $2, now() - $3::interval, now() - $4::interval)`,
		kind, runnerID, startedAgo.String(), endedAgo.String())
}

// ohBeat sets the control plane's last heartbeat to ago before the database's now.
func ohBeat(t *testing.T, st store.PG, ago time.Duration) {
	t.Helper()
	ohExec(t, st, `UPDATE cp_heartbeat SET beat_at = now() - $1::interval`, ago.String())
}

func ohRun(t *testing.T, st store.PG, id uuid.UUID) types.AgentRun {
	t.Helper()
	run, err := st.GetRun(context.Background(), id)
	if err != nil {
		t.Fatalf("read run %s: %v", id, err)
	}
	return run
}

// ohCheck is the run's recovery facts on the database's clock, with the store tracker not recovering.
func ohCheck(t *testing.T, st store.PG, id uuid.UUID) store.RecoveryCheck {
	t.Helper()
	rc, err := st.RunRecoveryCheck(context.Background(), id, false)
	if err != nil {
		t.Fatalf("read run %s's recovery facts: %v", id, err)
	}
	return rc
}

// ohRequireMarked asserts the sweep took its action before this change: the run is lost to an outage and still RUNNING.
func ohRequireMarked(t *testing.T, st store.PG, run types.AgentRun, what string) {
	t.Helper()
	got := ohRun(t, st, run.ID)
	if got.State != types.RunRunning || got.LostAt == nil || got.LostReason != types.LostOutage {
		t.Fatalf("%s: state %s lost %v %q; want RUNNING, lost (outage)", what, got.State, got.LostAt, got.LostReason)
	}
}

// ohRequireUnmarked asserts the sweep left the run alone: RUNNING, not lost, and no run.lost or run.reconcile row.
func ohRequireUnmarked(t *testing.T, st store.PG, audit *syncAudit, run types.AgentRun, what string) {
	t.Helper()
	got := ohRun(t, st, run.ID)
	if got.State != types.RunRunning || got.LostAt != nil {
		t.Fatalf("%s: state %s lost %v; want RUNNING and not lost", what, got.State, got.LostAt)
	}
	for _, action := range []string{"run.lost", "run.reconcile"} {
		if n := len(audit.eventsFor(run.ID, action)); n != 0 {
			t.Errorf("%s: %d %s rows, want 0", what, n, action)
		}
	}
}

// ohRequireEnded asserts a sweep ended the run through reconcileFinalize: FAILED, one run.reconcile row carrying
// exactly reason, and no run.lost row (it was never kept).
func ohRequireEnded(t *testing.T, st store.PG, audit *syncAudit, run types.AgentRun, reason string) {
	t.Helper()
	if got := ohRun(t, st, run.ID); got.State != types.RunFailed {
		t.Fatalf("state %s, want FAILED", got.State)
	}
	rows := audit.eventsFor(run.ID, "run.reconcile")
	if len(rows) != 1 {
		t.Fatalf("run.reconcile rows = %d, want 1", len(rows))
	}
	if d := leaseAuditData(t, rows[0]); d["reason"] != reason || d["to"] != string(types.RunFailed) {
		t.Errorf("run.reconcile data = %v, want reason %q and to FAILED", d, reason)
	}
	if n := len(audit.eventsFor(run.ID, "run.lost")); n != 0 {
		t.Errorf("an ended run wrote %d run.lost rows, want 0", n)
	}
}

// ohServerFixture is the lapse sweep's fixture on a server whose identity provider verifies an expired run token
// (recovers), or one that implements no identity.RecoveryVerifier, the SPIRE mode. The fixture wraps the embedded
// provider in a counting wrapper that hides VerifyExpired, so recovers swaps the provider back in.
func ohServerFixture(t *testing.T, recovers bool) *lapseSweepFixture {
	t.Helper()
	f := newLapseSweepFixture(t)
	if recovers {
		f.srv.cfg.Identity = f.idp.Provider
	}
	if got := f.srv.recoverySupported(); got != recovers {
		t.Fatalf("recoverySupported = %v, want %v", got, recovers)
	}
	return f
}

// ohRunnerRun is a RUNNING run on the fixture's runner, token stamped stampedAgo ago, with its sandbox ref set as a
// runner placement sets it (no execution is issued) and its terms recorded.
func ohRunnerRun(t *testing.T, f *runnerLeaseFixture, stampedAgo time.Duration, terms types.OutageTerms) types.AgentRun {
	t.Helper()
	run := f.localRun(t, stampedAgo)
	f.running(t, &run, nil)
	run.SandboxRef = placement.RunnerRefPrefix(f.rn.ID.String()) + "fake-" + run.ID.String()
	if err := f.st.SetSandboxRef(f.ctx, run.ID, run.SandboxRef); err != nil {
		t.Fatal(err)
	}
	ohTerms(t, f.st, run.ID, terms)
	return run
}

// ohRunnerFixture is the runner lease fixture with its runner's link down, so a stop or an end for its runs is queued,
// and with the approval and broker doubles the loss mark cancels and revokes through.
func ohRunnerFixture(t *testing.T) *runnerLeaseFixture {
	t.Helper()
	f := newRunnerLeaseFixture(t)
	f.srv.cfg.Approvals, f.srv.cfg.Broker = f.h.approvals, f.h.broker
	f.offline(t)
	return f
}

// ohPage submits a final STATE page that reports run's sandbox present, its proxy up or stopped.
func ohPage(t *testing.T, f *runnerLeaseFixture, run types.AgentRun, proxyStopped bool) error {
	t.Helper()
	row := runnerwire.RunState{RunID: run.ID, Ref: run.SandboxRef, SandboxPresent: true}
	if proxyStopped {
		stopped := time.Now().Add(-time.Second)
		row.ProxyStoppedAt = &stopped
	}
	return finalStatePage(t, f.srv, f.rn.ID, row)
}

// ohObservation is what the org holds of run's latest STATE report: whether the judge asked for the run's loss, and
// whether a containment request was issued for it.
func ohObservation(t *testing.T, f *runnerLeaseFixture, run types.AgentRun) (needsLoss, containment bool) {
	t.Helper()
	if err := f.st.Pool.QueryRow(f.ctx, `SELECT needs_loss, containment IS NOT NULL FROM runner_inventory_observations WHERE runner_id = $1 AND run_id = $2`,
		f.rn.ID, run.ID).Scan(&needsLoss, &containment); err != nil {
		t.Fatalf("read the run's STATE observation: %v", err)
	}
	return needsLoss, containment
}

// ohRequireReady asserts a page for run is READY and the judge did not ask for its loss.
func ohRequireReady(t *testing.T, f *runnerLeaseFixture, run types.AgentRun, proxyStopped bool, what string) {
	t.Helper()
	if err := ohPage(t, f, run, proxyStopped); err != nil {
		t.Fatalf("%s: STATE page = %v, want READY", what, err)
	}
	if needs, _ := ohObservation(t, f, run); needs {
		t.Fatalf("%s: the judge asked for the run's loss, want it not", what)
	}
}

// ohRequireNeedsLoss asserts a page for run is refused with the judge asking for its loss.
func ohRequireNeedsLoss(t *testing.T, f *runnerLeaseFixture, run types.AgentRun, proxyStopped bool, what string) {
	t.Helper()
	if err := ohPage(t, f, run, proxyStopped); !errors.Is(err, store.ErrRunnerReconciliationRequired) {
		t.Fatalf("%s: STATE page = %v, want reconciliation required", what, err)
	}
	if needs, _ := ohObservation(t, f, run); !needs {
		t.Fatalf("%s: the judge did not ask for the run's loss, want it to", what)
	}
}

// ohQueued is how many open actions of kind the org holds for run.
func ohQueued(t *testing.T, f *runnerLeaseFixture, run types.AgentRun, kind types.RunnerActionKind) int {
	t.Helper()
	var n int
	if err := f.st.Pool.QueryRow(f.ctx, `SELECT count(*) FROM runner_pending_actions WHERE run_id = $1 AND kind = $2 AND applied_at IS NULL`,
		run.ID, string(kind)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ohLapseSweep is one pass of the lapse sweep.
func ohLapseSweep(t *testing.T, f *runnerLeaseFixture) {
	t.Helper()
	if err := f.srv.sweepLapsedRunTokens(f.ctx); err != nil {
		t.Fatalf("sweepLapsedRunTokens: %v", err)
	}
}

// ohOfflineSweep is one pass of the runner's offline sweep.
func ohOfflineSweep(t *testing.T, f *runnerLeaseFixture) {
	t.Helper()
	if err := f.srv.OfflineFromRunner(f.ctx, f.rn.ID); err != nil {
		t.Fatalf("OfflineFromRunner: %v", err)
	}
}

// ohServerRunOn stores a RUNNING run on the server's own substrate (no runner), its token last stamped ago before.
func ohServerRunOn(t *testing.T, st store.PG, interactive bool, ago time.Duration) types.AgentRun {
	t.Helper()
	run, err := st.CreateRun(context.Background(), types.AgentRun{
		ID: uuid.New(), CreatedAt: time.Now().Add(-48 * time.Hour), UpdatedAt: time.Now(), CreatedBy: "hold@example.com",
		Agent: "claude-code", Task: "hold", ConfinementClass: types.CC2, State: types.RunRunning,
		RunnerTarget: "docker", SandboxRef: "wardyn-agent-" + uuid.NewString()[:8], Interactive: interactive,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	ohExec(t, st, `UPDATE agent_runs SET token_renewed_at = now() - $2::interval WHERE id = $1`, run.ID, ago.String())
	return run
}

// A run is not held past the hold's absolute end: once its token expired more than 24 hours ago (the recover bound,
// store.RecoveryLimit) the sweep marks it, whatever window is open.
func TestOutageRecoveryHold_EndsAtTheRecoverBound_PG(t *testing.T) {
	f := ohServerFixture(t, true)
	ohOpenWindow(t, f.st, store.OutageKindServer, nil, 30*time.Hour)
	past := f.serverRun(t, true, 25*time.Hour+time.Minute)
	inside := f.serverRun(t, true, 24*time.Hour+30*time.Minute)
	ohTerms(t, f.st, past.ID, ohPauses)
	ohTerms(t, f.st, inside.ID, ohPauses)

	if rc := ohCheck(t, f.st, past.ID); !rc.Covered || rc.Held {
		t.Fatalf("a run stamped 25 h 1 min ago under an open window: covered %v held %v; want covered and not held", rc.Covered, rc.Held)
	}
	if rc := ohCheck(t, f.st, inside.ID); !rc.Covered || !rc.Held {
		t.Fatalf("a run stamped 24 h 30 min ago under the same window: covered %v held %v; want covered and held", rc.Covered, rc.Held)
	}

	f.sweepTokens(t)

	ohRequireMarked(t, f.st, past, "the run past the bound")
	ohRequireUnmarked(t, f.st, f.audit, inside, "the run inside the bound")
}

// A window that ended 25 minutes ago or more no longer holds its runs: 26 minutes after it ends the sweep marks the
// run; 24 minutes after, it is still held.
func TestOutageRecoveryHold_EndsAfterGrace_PG(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endedAgo time.Duration
		held     bool
	}{
		{"ended 26 minutes ago", 26 * time.Minute, false},
		{"ended 24 minutes ago", 24 * time.Minute, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := ohServerFixture(t, true)
			run := f.serverRun(t, true, 2*time.Hour) // its token expired an hour ago
			ohTerms(t, f.st, run.ID, ohPauses)
			ohClosedWindow(t, f.st, store.OutageKindControlPlane, nil, 3*time.Hour, tc.endedAgo)
			if rc := ohCheck(t, f.st, run.ID); !rc.Covered || rc.Held != tc.held {
				t.Fatalf("covered %v held %v; want covered and held %v", rc.Covered, rc.Held, tc.held)
			}

			f.sweepTokens(t)

			if tc.held {
				ohRequireUnmarked(t, f.st, f.audit, run, tc.name)
			} else {
				ohRequireMarked(t, f.st, run, tc.name)
			}
		})
	}
}

// The verdict is computed before the hold is asked: in one outage pass a run held for its proxy is left alone, a run
// whose terms end it when unattended is ended (the verdict says outage, with no window on record), and a run with no
// terms is kept as before. The pass's hold row counts only the kept run.
func TestLapseSweep_HoldAfterVerdict_PG(t *testing.T) {
	f := ohServerFixture(t, true)
	f.srv.cfg.StoreHealth = recoveringTracker(lapseTestGrace)
	held := f.serverRun(t, true, 70*time.Minute)
	ended := f.serverRun(t, false, 70*time.Minute)
	kept := f.serverRun(t, false, 70*time.Minute)
	ohTerms(t, f.st, held.ID, ohPauses)
	ohTerms(t, f.st, ended.ID, types.OutageTerms{AllowanceSec: 3600, EndUnattended: true, ProxyPauses: true})

	f.sweepTokens(t)

	ohRequireUnmarked(t, f.st, f.audit, held, "the held run")
	ohRequireEnded(t, f.st, f.audit, ended, ohUnattendedReason)
	f.requireKept(t, kept)
	rows := holdRows(f.audit)
	if len(rows) != 1 {
		t.Fatalf("run.lapse_sweep.hold rows = %d, want 1", len(rows))
	}
	if d := leaseAuditData(t, rows[0]); d["held"] != float64(1) {
		t.Errorf("run.lapse_sweep.hold held = %v, want 1: only the kept run is counted", d["held"])
	}
}

// A heartbeat gap that is still open holds a lapsed pausing run with no window and no tracker: the gap began before
// the token's expiry. A beat 10 seconds back is no gap.
func TestOutageRecoveryHold_OpenHeartbeatGapHolds_PG(t *testing.T) {
	t.Run("the sweep", func(t *testing.T) {
		f := ohServerFixture(t, true)
		run := f.serverRun(t, true, 70*time.Minute)
		ohTerms(t, f.st, run.ID, ohPauses)
		ohBeat(t, f.st, 70*time.Minute)
		if rc := ohCheck(t, f.st, run.ID); !rc.Held || rc.Covered {
			t.Fatalf("beat 70 min back: held %v covered %v; want held by the gap alone", rc.Held, rc.Covered)
		}

		f.sweepTokens(t)
		ohRequireUnmarked(t, f.st, f.audit, run, "beat 70 minutes back")

		ohBeat(t, f.st, 10*time.Second)
		f.sweepTokens(t)
		ohRequireMarked(t, f.st, run, "beat 10 seconds back")
	})
	t.Run("the STATE judge", func(t *testing.T) {
		f := ohRunnerFixture(t)
		run := ohRunnerRun(t, f, 70*time.Minute, ohPauses)
		ohBeat(t, f.st, 70*time.Minute)
		ohRequireReady(t, f, run, false, "beat 70 minutes back")

		ohBeat(t, f.st, 10*time.Second)
		ohRequireNeedsLoss(t, f, run, false, "beat 10 seconds back")
	})
}

// A run its runner holds contained is not held, whatever window covers it: the runner refuses it any lease, so no
// recovery reaches it. The sweep marks it.
func TestOutageRecoveryHold_ContainedOnRunnerNotHeld_PG(t *testing.T) {
	for name, contain := range map[string]func(t *testing.T, f *runnerLeaseFixture, run types.AgentRun){
		"a queued quarantine": func(t *testing.T, f *runnerLeaseFixture, run types.AgentRun) {
			id := uuid.New()
			ohExec(t, f.st, `INSERT INTO runner_pending_actions (id, runner_id, run_id, kind, ref, binding_version, owner, proof)
				VALUES ($1, $2, $3, 'quarantine_run', $4, 1, $5, $6)`,
				id, f.rn.ID, run.ID, run.SandboxRef, f.rn.Owner, `{"id":"`+uuid.NewString()+`","generation":"`+uuid.Nil.String()+`"}`)
		},
		"a containment pending row": func(t *testing.T, f *runnerLeaseFixture, run types.AgentRun) {
			ohExec(t, f.st, `INSERT INTO run_containment_pending (run_id, runner_id, kind, since) VALUES ($1, $2, 'agent_only', now())`,
				run.ID, f.rn.ID)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := ohRunnerFixture(t)
			run := ohRunnerRun(t, f, 70*time.Minute, ohPauses)
			ohOpenWindow(t, f.st, store.OutageKindRunnerLink, &f.rn.ID, 90*time.Minute)
			if rc := ohCheck(t, f.st, run.ID); !rc.Held {
				t.Fatalf("the run under its runner's open window is not held before it is contained")
			}

			contain(t, f, run)

			if rc := ohCheck(t, f.st, run.ID); rc.Held || !rc.Covered {
				t.Fatalf("contained on its runner: held %v covered %v; want covered and not held", rc.Held, rc.Covered)
			}
			ohLapseSweep(t, f)
			ohRequireMarked(t, f.st, run, "the contained run")
		})
	}
}

// A local run whose runner was deleted (a null runner id, placement kept) is not a server run: a server_outage
// window does not hold it, and it still holds a server run beside it.
func TestOutageRecoveryHold_DeletedRunnersLocalRunNotHeldByServerWindow_PG(t *testing.T) {
	f := ohServerFixture(t, true)
	deleted := f.serverRun(t, true, 70*time.Minute)
	server := f.serverRun(t, true, 70*time.Minute)
	ohTerms(t, f.st, deleted.ID, ohPauses)
	ohTerms(t, f.st, server.ID, ohPauses)
	ohExec(t, f.st, `UPDATE agent_runs SET placement = 'local' WHERE id = $1`, deleted.ID)
	ohClosedWindow(t, f.st, store.OutageKindServer, nil, 90*time.Minute, 10*time.Minute)

	if rc := ohCheck(t, f.st, deleted.ID); rc.Covered || rc.Held {
		t.Fatalf("the deleted runner's local run: covered %v held %v; want neither, a server window is not its window", rc.Covered, rc.Held)
	}
	if rc := ohCheck(t, f.st, server.ID); !rc.Covered || !rc.Held {
		t.Fatalf("the server run: covered %v held %v; want both", rc.Covered, rc.Held)
	}

	f.sweepTokens(t)

	ohRequireMarked(t, f.st, deleted, "the deleted runner's local run")
	ohRequireUnmarked(t, f.st, f.audit, server, "the server run")
}

// A strict run past its allowance is ended, never recovered: its window is on record and its proxy pauses, and the
// sweep ends it with the strict reason.
func TestLapseSweep_StrictPastAllowanceEnds_PG(t *testing.T) {
	f := ohServerFixture(t, true)
	run := f.serverRun(t, true, 70*time.Minute)
	ohTerms(t, f.st, run.ID, ohStrict30)
	ohClosedWindow(t, f.st, store.OutageKindControlPlane, nil, 3*time.Hour, 10*time.Minute)
	if rc := ohCheck(t, f.st, run.ID); !rc.StrictSpent || rc.Held || !rc.Covered {
		t.Fatalf("strict 30 min, stamped 70 min ago: spent %v held %v covered %v; want spent, covered, not held", rc.StrictSpent, rc.Held, rc.Covered)
	}

	f.sweepTokens(t)

	ohRequireEnded(t, f.st, f.audit, run, ohStrictReason)
}

// A strict run inside its allowance is held like any other: unmarked, not ended.
func TestLapseSweep_StrictInsideAllowanceHeld_PG(t *testing.T) {
	f := ohServerFixture(t, true)
	run := f.serverRun(t, true, 70*time.Minute)
	ohTerms(t, f.st, run.ID, types.OutageTerms{AllowanceSec: 7200, Strict: true, ProxyPauses: true})
	ohClosedWindow(t, f.st, store.OutageKindControlPlane, nil, 3*time.Hour, 10*time.Minute)
	if rc := ohCheck(t, f.st, run.ID); rc.StrictSpent || !rc.Held {
		t.Fatalf("strict 2 h, stamped 70 min ago: spent %v held %v; want held inside its allowance", rc.StrictSpent, rc.Held)
	}

	f.sweepTokens(t)

	ohRequireUnmarked(t, f.st, f.audit, run, "the strict run inside its allowance")
}

// ohStrictSpentWithoutOutage is the strict-spent fixture with no window and no verdict: the run is interactive, which
// the sweep would keep without its terms.
func ohStrictSpentWithoutOutage(t *testing.T, recovers bool) {
	t.Helper()
	f := ohServerFixture(t, recovers)
	run := f.serverRun(t, true, 70*time.Minute)
	ohTerms(t, f.st, run.ID, ohStrict30)
	if rc := ohCheck(t, f.st, run.ID); !rc.StrictSpent || rc.Covered {
		t.Fatalf("spent %v covered %v; want spent with no window", rc.StrictSpent, rc.Covered)
	}

	f.sweepTokens(t)

	ohRequireEnded(t, f.st, f.audit, run, ohStrictReason)
}

// A spent allowance ends the run with no window and no verdict on record, not the keep an interactive run gets without its terms.
func TestLapseSweep_StrictSpentEndsWithoutOutage_PG(t *testing.T) {
	ohStrictSpentWithoutOutage(t, true)
}

// The same on a server whose identity provider cannot recover (the SPIRE mode): it ends the run on the same path with
// the same reason.
func TestLapseSweep_StrictSpentEndsWithoutRecoveryVerifier_PG(t *testing.T) {
	ohStrictSpentWithoutOutage(t, false)
}

// ohRequireBrokerRevoked asserts the broker double saw the run's revoke.
func ohRequireBrokerRevoked(t *testing.T, f *runnerLeaseFixture, run types.AgentRun) {
	t.Helper()
	f.h.broker.mu.Lock()
	defer f.h.broker.mu.Unlock()
	if !slices.Contains(f.h.broker.revoked, run.ID) {
		t.Errorf("the run's broker grants were not revoked")
	}
}

// ohStrictSpentRunnerRun drives one runner's strict run past its allowance through sweep, then the runner's side of
// it: the loss mark, the proxy stop and the end both queued for the runner, and the runner ready once it has applied
// both and reports its proxy stopped. A revive is refused, before and after the end settles.
func ohStrictSpentRunnerRun(t *testing.T, sweep func(*testing.T, *runnerLeaseFixture)) {
	t.Helper()
	f := ohRunnerFixture(t)
	run := ohRunnerRun(t, f, 70*time.Minute, ohStrict30)
	approval, err := f.h.approvals.Request(f.ctx, types.ApprovalRequest{RunID: run.ID, Kind: types.ApprovalEgressDomain})
	if err != nil {
		t.Fatalf("raise an approval for the run: %v", err)
	}

	sweep(t, f)

	got := ohRun(t, f.st, run.ID)
	if got.State != types.RunRunning || got.LostAt == nil || got.LostReason != types.LostOutage {
		t.Fatalf("after the sweep: state %s lost %v %q; want RUNNING, lost (outage)", got.State, got.LostAt, got.LostReason)
	}
	if n := ohQueued(t, f, run, types.RunnerActionStopProxy); n != 1 {
		t.Fatalf("stop_proxy actions queued = %d, want 1", n)
	}
	if n := ohQueued(t, f, run, types.RunnerActionEnd); n != 1 {
		t.Fatalf("end actions queued = %d, want 1", n)
	}
	if ap, err := f.h.approvals.Get(f.ctx, approval.ID); err != nil || ap.State != types.ApprovalCancelled {
		t.Errorf("the run's pending approval = %+v, %v; want cancelled", ap.State, err)
	}
	ohRequireBrokerRevoked(t, f, run)

	// (1) The sandbox is present and the proxy up: not ready, and the runner is asked to contain the run, not to lose it.
	if err := ohPage(t, f, run, false); !errors.Is(err, store.ErrRunnerReconciliationRequired) {
		t.Fatalf("a page with the proxy up = %v, want reconciliation required", err)
	}
	if needs, contained := ohObservation(t, f, run); needs || !contained {
		t.Fatalf("the page's observation: needs_loss %v, containment request %v; want a containment request and no loss", needs, contained)
	}

	// (2) The runner applies both actions.
	f.answer(t, run, types.RunnerActionStopProxy, types.RunnerActionSuccess)
	f.answer(t, run, types.RunnerActionEnd, types.RunnerActionSuccess)

	// (3) The proxy reported stopped: ready.
	ohRequireReady(t, f, run, true, "the proxy stopped")

	// A revive is refused: its token would restart an allowance the run already spent.
	revive := func(what string) {
		t.Helper()
		_, rerr := f.srv.reviveRunProxy(f.ctx, ohRun(t, f.st, run.ID), types.ActorSystem, "test", false)
		if rerr == nil || rerr.status != http.StatusConflict || rerr.reason != reasonReviveStrictAllowanceSpent {
			t.Fatalf("%s: revive = %+v, want 409 %s", what, rerr, reasonReviveStrictAllowanceSpent)
		}
	}
	revive("before the end settles")

	// The end has settled: the runner holds the run contained with an end marker, which a revive would recover.
	marked := runnerwire.RunState{RunID: run.ID, Ref: run.SandboxRef, SandboxPresent: true,
		Containment: &types.RunnerMutationFailure{ID: uuid.New(), Generation: uuid.Nil, Operation: string(types.RunnerActionEnd), Contained: true}}
	stopped := time.Now().Add(-time.Second)
	marked.ProxyStoppedAt = &stopped
	if err := finalStatePage(t, f.srv, f.rn.ID, marked); err != nil {
		t.Fatalf("a page with the end marker = %v, want READY", err)
	}
	revive("after the end settles")
	if n := ohQueued(t, f, run, types.RunnerActionRecoverContainment); n != 0 {
		t.Errorf("recover_containment actions queued = %d, want 0", n)
	}
}

// The lapse sweep marks a strict runner run lost, stops its proxy, ends it on its runner, and its revive is refused.
func TestLapseSweep_StrictSpentRunnerRunIsEnded_PG(t *testing.T) {
	ohStrictSpentRunnerRun(t, ohLapseSweep)
}

// The runner's offline sweep does the same for the runs it holds.
func TestOfflineFromRunner_StrictSpentRunnerRunIsEnded_PG(t *testing.T) {
	ohStrictSpentRunnerRun(t, ohOfflineSweep)
}

// ohEndUnattended is the outage fixture: the pass's verdict says outage, with no window on record. A headless server
// run whose terms end it is ended with the unattended reason, whatever the server's recovery mode; an interactive
// run with the same terms, and a headless run without them, are kept as before this change.
func ohEndUnattended(t *testing.T, recovers bool) {
	t.Helper()
	f := ohServerFixture(t, recovers)
	f.srv.cfg.StoreHealth = recoveringTracker(lapseTestGrace)
	endTerms := types.OutageTerms{AllowanceSec: 3600, EndUnattended: true, ProxyPauses: true}
	unattended := f.serverRun(t, false, 70*time.Minute)
	interactive := f.serverRun(t, true, 70*time.Minute)
	keeps := f.serverRun(t, false, 70*time.Minute)
	ohTerms(t, f.st, unattended.ID, endTerms)
	ohTerms(t, f.st, interactive.ID, types.OutageTerms{AllowanceSec: 3600, EndUnattended: true}) // its proxy does not pause: nothing holds it

	f.sweepTokens(t)

	ohRequireEnded(t, f.st, f.audit, unattended, ohUnattendedReason)
	f.requireKept(t, interactive)
	f.requireKept(t, keeps)
}

// "end" ends only an unattended server run, and only in an outage.
func TestLapseSweep_EndUnattended_PG(t *testing.T) {
	ohEndUnattended(t, true)

	// Control: outside an outage the same terms end nothing here; a lone dead proxy ends as before its terms.
	f := ohServerFixture(t, true)
	for range 9 {
		f.serverRun(t, false, time.Minute)
	}
	f.srv.cfg.StoreHealth = healthyTracker(lapseTestGrace, lapseTestGrace+time.Minute)
	lone := f.serverRun(t, false, 80*time.Minute)
	ohTerms(t, f.st, lone.ID, types.OutageTerms{AllowanceSec: 3600, EndUnattended: true, ProxyPauses: true})

	f.sweepTokens(t)

	ohRequireEnded(t, f.st, f.audit, lone, ohLapsedReason)
}

// The same outage fixture on a server whose identity provider cannot recover: the same end, with the same reason.
func TestLapseSweep_EndUnattendedWithoutRecoveryVerifier_PG(t *testing.T) {
	ohEndUnattended(t, false)
}

// A runner run is never held on account of the store tracker (the STATE judge cannot read it): a recovering store
// holds a server run, but its runner's lapsed run is marked and contained, and the runner reaches READY once its
// proxy is stopped.
func TestOfflineFromRunner_RecoveringDoesNotHoldRunnerRun_PG(t *testing.T) {
	f := ohRunnerFixture(t)
	f.srv.cfg.StoreHealth = recoveringTracker(lapseTestGrace)
	run := ohRunnerRun(t, f, 70*time.Minute, ohPauses)
	server := ohServerRunOn(t, f.st, true, 70*time.Minute)
	ohTerms(t, f.st, server.ID, ohPauses)
	if rc, err := f.st.RunRecoveryCheck(f.ctx, run.ID, true); err != nil || !rc.Held {
		t.Fatalf("with the tracker's answer the runner run would be held: %+v, %v", rc, err)
	}
	if rc := ohCheck(t, f.st, run.ID); rc.Held {
		t.Fatalf("without the tracker's answer the runner run is held; want it not")
	}

	ohOfflineSweep(t, f)

	ohRequireMarked(t, f.st, run, "the runner's lapsed run")
	if n := ohQueued(t, f, run, types.RunnerActionStopProxy); n != 1 {
		t.Fatalf("stop_proxy actions queued = %d, want 1", n)
	}
	// The page with the sandbox present and the proxy up is not READY: it asks for the run's containment, not its loss.
	if err := ohPage(t, f, run, false); !errors.Is(err, store.ErrRunnerReconciliationRequired) {
		t.Fatalf("a page with the proxy up = %v, want reconciliation required", err)
	}
	if needs, contained := ohObservation(t, f, run); needs || !contained {
		t.Fatalf("the page's observation: needs_loss %v, containment request %v; want a containment request and no loss", needs, contained)
	}
	f.answer(t, run, types.RunnerActionStopProxy, types.RunnerActionSuccess)
	ohRequireReady(t, f, run, true, "the proxy stopped")

	// In the same tracker state the sweep holds the server run.
	ohLapseSweep(t, f)
	ohRequireUnmarked(t, f.st, f.srv.cfg.Audit.(*syncAudit), server, "the server run in a recovering store")
}

// A held runner run is held only while its runner could carry its recovery: when its person owner is suspended, or
// runners are switched off, the next sweep marks it with its stop queued, the window still open.
func TestLapseSweep_InadmissibleRunnerStopsHolding_PG(t *testing.T) {
	for _, tc := range []struct {
		name       string
		inadmitted func(t *testing.T, f *runnerLeaseFixture)
	}{
		{"owner suspended", func(t *testing.T, f *runnerLeaseFixture) {
			ohExec(t, f.st, `INSERT INTO principal_identities (principal, issuer, deactivated_at) VALUES ($1, 'test-issuer', now())`, f.rn.Owner)
		}},
		{"runners switched off", func(t *testing.T, f *runnerLeaseFixture) {
			if _, err := f.st.PutSiteConfig(f.ctx, types.SiteConfig{Runners: &types.RunnerSettings{Enabled: false}}); err != nil {
				t.Fatal(err)
			}
		}},
		{"control: an admissible runner", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := ohRunnerFixture(t)
			run := ohRunnerRun(t, f, 70*time.Minute, ohPauses)
			ohOpenWindow(t, f.st, store.OutageKindRunnerLink, &f.rn.ID, 90*time.Minute)

			ohLapseSweep(t, f)
			ohRequireUnmarked(t, f.st, f.srv.cfg.Audit.(*syncAudit), run, "pass 1")

			want := outageRecoveryHold
			if tc.inadmitted != nil {
				tc.inadmitted(t, f)
				want = outageRecoveryNone
			}
			if got := f.srv.outageRecoveryFor(f.ctx, ohRun(t, f.st, run.ID), lapseVerdict{}, false); got != want {
				t.Fatalf("outageRecoveryFor as OfflineFromRunner calls it = %v, want %v", got, want)
			}

			ohLapseSweep(t, f)

			if tc.inadmitted == nil {
				ohRequireUnmarked(t, f.st, f.srv.cfg.Audit.(*syncAudit), run, "pass 2, admissible runner")
				return
			}
			ohRequireMarked(t, f.st, run, "pass 2")
			if n := ohQueued(t, f, run, types.RunnerActionStopProxy); n != 1 {
				t.Errorf("stop_proxy actions queued = %d, want 1", n)
			}
			var open int
			if err := f.st.Pool.QueryRow(f.ctx, `SELECT count(*) FROM outage_windows WHERE kind = 'runner_link' AND runner_id = $1 AND ended_at IS NULL`,
				f.rn.ID).Scan(&open); err != nil || open != 1 {
				t.Errorf("open runner_link windows = %d, %v; want the window still open", open, err)
			}
		})
	}
}
