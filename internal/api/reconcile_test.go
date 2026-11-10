// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// errTeardownRunner reuses fakeRunner but makes StopSandbox fail, so we can
// exercise the boot-reconcile teardown-error path (finding N2).
type errTeardownRunner struct {
	*fakeRunner
	stopErr error
}

func (e *errTeardownRunner) StopSandbox(context.Context, string) error { return e.stopErr }

// TestAuditK8sNetpolIfUnenforced covers 6b commit 2: ReconcileOnBoot's
// auditK8sNetpolIfUnenforced writes "k8s.netpol.fail" for the two
// verdicts under which a sandbox runs unconfined (unenforced, acknowledged),
// carrying {verdict, driver} with a nil run id (the apitokens.go token.create/
// token.revoke precedent — a deployment-wide fact, not tied to a run).
// Negative controls: a PROVEN-enforced k8s driver, a non-k8s driver, and a k8s
// driver whose Capabilities() call itself errors must all stay silent.
func TestAuditK8sNetpolIfUnenforced(t *testing.T) {
	fire := func(t *testing.T, rn runner.Runner) *recRecorder {
		t.Helper()
		audit := &recRecorder{}
		srv := New(Config{BaseCtx: testBaseCtx(t), Runner: rn, Audit: audit})
		srv.auditK8sNetpolIfUnenforced(context.Background())
		return audit
	}
	find := func(events []types.AuditEvent) *types.AuditEvent {
		for i := range events {
			if events[i].Action == "k8s.netpol.fail" {
				return &events[i]
			}
		}
		return nil
	}

	for _, tc := range []struct {
		name string
		rn   runner.Runner
	}{
		{"unenforced", k8sRunner{}},
		{"acknowledged", k8sRunner{networkPolicyAcknowledged: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			audit := fire(t, tc.rn)
			ev := find(audit.events)
			if ev == nil {
				t.Fatalf("no k8s.netpol.fail event recorded (have %d events)", len(audit.events))
			}
			if ev.RunID != nil {
				t.Errorf("RunID = %v, want nil (deployment-wide, not run-scoped)", ev.RunID)
			}
			if ev.ActorType != types.ActorSystem {
				t.Errorf("ActorType = %q, want %q", ev.ActorType, types.ActorSystem)
			}
			var data struct {
				Verdict string `json:"verdict"`
				Driver  string `json:"driver"`
			}
			if err := json.Unmarshal(ev.Data, &data); err != nil {
				t.Fatalf("decode Data: %v", err)
			}
			wantVerdict := tc.name
			if data.Verdict != wantVerdict {
				t.Errorf("Data.verdict = %q, want %q", data.Verdict, wantVerdict)
			}
			if data.Driver != "k8s" {
				t.Errorf("Data.driver = %q, want k8s", data.Driver)
			}
		})
	}

	t.Run("negative control: enforced stays silent", func(t *testing.T) {
		audit := fire(t, k8sRunner{networkPolicy: true})
		if ev := find(audit.events); ev != nil {
			t.Errorf("k8s.netpol.fail fired on a PROVEN-enforced verdict: %+v", ev)
		}
	})
	t.Run("negative control: non-k8s driver stays silent", func(t *testing.T) {
		audit := fire(t, &fakeRunner{})
		if ev := find(audit.events); ev != nil {
			t.Errorf("k8s.netpol.fail fired on a non-k8s (Docker) driver: %+v", ev)
		}
	})
	t.Run("negative control: Capabilities() error stays silent", func(t *testing.T) {
		audit := fire(t, k8sRunner{capsErr: errors.New("k8s api unreachable")})
		if ev := find(audit.events); ev != nil {
			t.Errorf("k8s.netpol.fail fired despite a Capabilities() error: %+v", ev)
		}
	})
}

// TestAuditK8sNetpolChange: a re-check that changes the verdict writes one
// "k8s.netpol.update" row, in either direction, naming both verdicts and the
// check time; an unchanged verdict, an unreadable one and the first one read
// write nothing.
func TestAuditK8sNetpolChange(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	rn := &k8sRecheckRunner{check: runner.NetPolCheck{Verdict: runner.NetPolEnforced, At: at}}
	audit := &recRecorder{}
	srv := New(Config{BaseCtx: testBaseCtx(t), Runner: rn, Audit: audit})
	ctx := context.Background()
	updates := func() []types.AuditEvent {
		var out []types.AuditEvent
		for _, ev := range audit.snapshot() {
			if ev.Action == "k8s.netpol.update" {
				out = append(out, ev)
			}
		}
		return out
	}

	last := srv.auditK8sNetpolIfUnenforced(ctx)
	if last != "enforced" {
		t.Fatalf("boot verdict %q, want enforced", last)
	}
	last = srv.auditK8sNetpolChange(ctx, last)
	if n := len(updates()); n != 0 {
		t.Fatalf("%d rows for an unchanged verdict, want none", n)
	}

	rn.check = runner.NetPolCheck{Verdict: runner.NetPolRefused, At: at.Add(10 * time.Minute)}
	last = srv.auditK8sNetpolChange(ctx, last)
	last = srv.auditK8sNetpolChange(ctx, last)
	rn.check = runner.NetPolCheck{Verdict: runner.NetPolEnforced, At: at.Add(20 * time.Minute)}
	last = srv.auditK8sNetpolChange(ctx, last)
	rows := updates()
	if len(rows) != 2 || last != "enforced" {
		t.Fatalf("%d rows, last %q; want 2 (one per change) and enforced", len(rows), last)
	}
	for i, want := range []struct {
		outcome, verdict, previous string
		at                         time.Time
	}{
		{"failure", "refused", "enforced", at.Add(10 * time.Minute)},
		{"success", "enforced", "refused", at.Add(20 * time.Minute)},
	} {
		var data struct {
			Verdict, Previous, Driver string
			CheckedAt                 string `json:"checked_at"`
		}
		if err := json.Unmarshal(rows[i].Data, &data); err != nil {
			t.Fatalf("row %d: decode Data: %v", i, err)
		}
		got := struct {
			outcome, verdict, previous string
			at                         time.Time
		}{rows[i].Outcome, data.Verdict, data.Previous, time.Time{}}
		got.at, _ = time.Parse(time.RFC3339, data.CheckedAt)
		if got != want || data.Driver != "k8s" || rows[i].RunID != nil || rows[i].ActorType != types.ActorSystem {
			t.Errorf("row %d: %+v driver %q run %v actor %q; want %+v, k8s, nil, system", i, got, data.Driver, rows[i].RunID, rows[i].ActorType, want)
		}
	}

	rn.capsErr = errors.New("k8s api unreachable")
	if got := srv.auditK8sNetpolChange(ctx, "enforced"); got != "enforced" || len(updates()) != 2 {
		t.Errorf("unreadable verdict: kept %q with %d rows, want enforced and no new row", got, len(updates()))
	}
	rn.capsErr = nil
	rn.check.Verdict = runner.NetPolNotProven
	if got := srv.auditK8sNetpolChange(ctx, ""); got != "not_proven" || len(updates()) != 2 {
		t.Errorf("first read: %q with %d rows, want not_proven as the baseline and no new row", got, len(updates()))
	}
}

// TestAuditRunnerNetpolChange: against the last verdict this process took from a Kubernetes runner, a
// changed verdict writes one "k8s.netpol.update" row and the same verdict none; caps with no verdict, an
// unknown word or no runs namespaces are not taken and keep the last verdict; a first verdict writes no
// row when it is "enforced" and one "k8s.netpol.fail" row otherwise. Every row names the runner, and a
// check time past the clock skew is dropped.
func TestAuditRunnerNetpolChange(t *testing.T) {
	audit := &recRecorder{}
	srv := New(Config{BaseCtx: testBaseCtx(t), Runner: &fakeRunner{}, Audit: audit})
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	rows := func(action string) []types.AuditEvent {
		var out []types.AuditEvent
		for _, ev := range audit.snapshot() {
			if ev.Action == action {
				out = append(out, ev)
			}
		}
		return out
	}
	type rowData struct {
		Verdict, Previous, Driver string
		RunnerID                  string `json:"runner_id"`
		CheckedAt                 string `json:"checked_at"`
	}
	decode := func(t *testing.T, ev types.AuditEvent) rowData {
		t.Helper()
		var d rowData
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatalf("decode Data: %v", err)
		}
		return d
	}
	// caps is runner id's caps event; a Kubernetes runner's names its runs namespaces.
	caps := func(id uuid.UUID, k8s bool, check runner.NetPolCheck) {
		t.Helper()
		c := runnerwire.Caps{Support: substrate.ClassSupport{Classes: []types.ConfinementClass{types.CC2}, NetworkPolicyCheck: check}}
		if k8s {
			c.RunsNamespaces = []string{"wardyn-runs"}
		}
		data, err := runnerwire.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		srv.capsFromRunner(ctx, id, "", runnerwire.Event{Kind: runnerwire.EventCaps, Data: data})
	}

	a := uuid.New()
	caps(a, true, runner.NetPolCheck{Verdict: runner.NetPolEnforced, At: at})
	caps(a, true, runner.NetPolCheck{Verdict: runner.NetPolEnforced, At: at.Add(10 * time.Minute)})
	if n := len(audit.snapshot()); n != 0 {
		t.Fatalf("%d rows for a first enforced verdict and the same again, want none", n)
	}
	caps(a, true, runner.NetPolCheck{})                                       // no verdict: keeps enforced
	caps(a, true, runner.NetPolCheck{Verdict: "newer-word", At: at})          // unknown: keeps enforced
	caps(a, false, runner.NetPolCheck{Verdict: runner.NetPolRefused, At: at}) // not a k8s runner's caps
	if n := len(audit.snapshot()); n != 0 {
		t.Fatalf("%d rows for caps with nothing to take, want none", n)
	}
	caps(a, true, runner.NetPolCheck{Verdict: runner.NetPolRefused, At: at.Add(20 * time.Minute)})
	up := rows("k8s.netpol.update")
	if len(up) != 1 {
		t.Fatalf("%d update rows after enforced, no verdict, refused; want 1", len(up))
	}
	d := decode(t, up[0])
	if want := at.Add(20 * time.Minute).Format(time.RFC3339); up[0].Outcome != "failure" || d.Verdict != "refused" || d.Previous != "enforced" ||
		d.Driver != "k8s" || d.RunnerID != a.String() || d.CheckedAt != want || up[0].RunID != nil || up[0].ActorType != types.ActorSystem {
		t.Errorf("row %+v data %+v; want failure, refused from enforced, k8s, runner %s, checked %s, nil run, system", up[0], d, a, want)
	}
	caps(a, true, runner.NetPolCheck{Verdict: runner.NetPolRefused, At: at.Add(30 * time.Minute)})
	if n := len(rows("k8s.netpol.update")); n != 1 {
		t.Fatalf("%d update rows after the same verdict again, want still 1", n)
	}

	b := uuid.New()
	caps(b, true, runner.NetPolCheck{Verdict: runner.NetPolUnenforced, At: at})
	c := uuid.New()
	caps(c, true, runner.NetPolCheck{Verdict: runner.NetPolRefused, At: time.Now().Add(2 * runnerClockSkew)})
	fails := rows("k8s.netpol.fail")
	if len(fails) != 2 {
		t.Fatalf("%d fail rows for two first verdicts that are not enforced, want 2", len(fails))
	}
	if d := decode(t, fails[0]); fails[0].Outcome != "failure" || d.Verdict != "unenforced" || d.RunnerID != b.String() || d.CheckedAt != at.Format(time.RFC3339) {
		t.Errorf("first fail row %+v data %+v; want failure, unenforced, runner %s, checked %s", fails[0], d, b, at.Format(time.RFC3339))
	}
	if d := decode(t, fails[1]); d.Verdict != "refused" || d.RunnerID != c.String() || d.CheckedAt != "" {
		t.Errorf("second fail row data %+v; want refused, runner %s, no check time (it was in the future)", d, c)
	}
	if n := len(rows("k8s.netpol.update")); n != 1 {
		t.Errorf("%d update rows after first verdicts, want still 1", n)
	}
}

// TestReconcileFinalize_TeardownErrorAudited covers finding N2: when the boot
// reconciler finalizes a stranded run and the sandbox teardown fails, it must
// audit a run.reconcile/failure event carrying teardown_error — otherwise the
// live/routable container is abandoned forever (the run is now terminal, so the
// next boot skips it) with no record. PG-gated.
func TestReconcileFinalize_TeardownErrorAudited(t *testing.T) {
	fr := &errTeardownRunner{fakeRunner: &fakeRunner{}, stopErr: errors.New("docker: no such container")}
	srv, pool := pgHarnessWithRunner(t, fr)
	audit := srv.cfg.Audit.(*recRecorder)
	ctx := context.Background()

	// A stranded RUNNING run with a sandbox ref (as a crash would leave it).
	runID := uuid.New()
	now := time.Now().UTC()
	run := types.AgentRun{
		ID:               runID,
		CreatedAt:        now,
		UpdatedAt:        now,
		CreatedBy:        "tester@example.com",
		Agent:            "claude-code",
		ConfinementClass: types.CC2,
		State:            types.RunRunning,
		SPIFFEID:         "spiffe://wardyn.local/agent-run/" + runID.String(),
		RunnerTarget:     "docker",
		SandboxRef:       "container-" + runID.String(),
	}
	if _, err := store.NewPG(pool).CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id=$1`, runID) })

	srv.reconcileFinalize(ctx, runID, types.RunFailed, run.SandboxRef, "reconciled after restart")

	// The run must still be finalized terminal despite the teardown failure.
	if got, _ := store.NewPG(pool).GetRun(ctx, runID); got.State != types.RunFailed {
		t.Errorf("run state = %q, want FAILED (finalized regardless of teardown)", got.State)
	}

	// A run.reconcile/failure event carrying teardown_error MUST have been emitted.
	var failure *types.AuditEvent
	for i := range audit.events {
		ev := &audit.events[i]
		if ev.Action == "run.reconcile" && ev.Outcome == "failure" && ev.RunID != nil && *ev.RunID == runID {
			failure = ev
		}
	}
	if failure == nil {
		t.Fatal("no run.reconcile/failure event emitted for the abandoned sandbox")
	}
	if !strings.Contains(string(failure.Data), "teardown_error") {
		t.Errorf("run.reconcile/failure data should carry teardown_error, got %s", failure.Data)
	}
	if !strings.Contains(string(failure.Data), "no such container") {
		t.Errorf("run.reconcile/failure should include the teardown error text, got %s", failure.Data)
	}
}

// compile-time check that errTeardownRunner is a runner.Runner.
var _ runner.Runner = (*errTeardownRunner)(nil)
