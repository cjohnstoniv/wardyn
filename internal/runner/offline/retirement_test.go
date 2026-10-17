// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

func retirementFixture(t *testing.T) (*Manager, *localSub, uuid.UUID, *runnerspool.Spool, runnerwire.PendingAction) {
	t.Helper()
	m, sub, _, org := managerFixture(t)
	id := createLocal(t, m, org)
	if err := m.MutationFinished(t.Context(), runnerwire.MethodCreateSandbox, "", id, uuid.Nil, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := m.apply(t.Context(), pendingContainment(m, id, types.RunnerActionKill)); err != nil || got.Outcome != "success" {
		t.Fatalf("kill: %+v %v", got, err)
	}
	spool, err := runnerspool.Open(filepath.Join(m.dir, "spools", id.String()), id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = spool.Close() })
	receipt, err := spool.Receipt(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	a := pendingContainment(m, id, types.RunnerActionRetire)
	a.Retirement = &types.RunnerRetirement{ID: uuid.New(), Spool: receipt}
	return m, sub, id, spool, a
}

func TestRetirementRequiresExactDrainedEvidenceAndRetainsTombstones(t *testing.T) {
	m, sub, id, spool, a := retirementFixture(t)
	decision := egress.DecisionLog{Request: egress.Request{RunID: id}}
	if err := spool.Append(t.Context(), decision); err != nil {
		t.Fatal(err)
	}
	first, err := m.apply(t.Context(), a)
	if err != nil || first.Outcome != "deferred" || m.state.Runs[id].Retired {
		t.Fatalf("undelivered tail: %+v %v", first, err)
	}
	if err := spool.Replay(t.Context(), func(context.Context, []runnerspool.Record) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if again, err := m.apply(t.Context(), a); err != nil || again != first {
		t.Fatal("deferred receipt was not terminal for its action")
	}
	a.ID = uuid.New()
	proof := *a.Retirement
	a.Retirement = &proof
	a.Retirement.Spool, err = spool.Receipt(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.apply(t.Context(), a)
	if err != nil || got.Outcome != "success" {
		t.Fatalf("retire: %+v %v", got, err)
	}
	if !m.state.Runs[id].Retired || m.state.Runs[id].Ref != id.String() || len(m.state.Actions) != 3 {
		t.Fatal("retirement pruned identity or receipts")
	}
	if rows, err := allState(t.Context(), m); err != nil || len(rows) != 0 {
		t.Fatal("retired history remains live inventory")
	}
	if err := m.BeforeCreate(t.Context(), runner.SandboxSpec{RunID: id}); err == nil {
		t.Fatal("retired run ID reused")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(m.dir, m.runnerID, sub, uuid.UUID.String, noOutput, noErase)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if again, err := reopened.apply(t.Context(), a); err != nil || again != got {
		t.Fatal("retirement receipt changed after restart")
	}
	bad := a
	bad.Retirement = &types.RunnerRetirement{ID: uuid.New()}
	if _, err := reopened.apply(t.Context(), bad); err == nil {
		t.Fatal("same action accepted changed settlement")
	}
}

func TestRetirementDefersLiveOutputAndRejectsWrongSettlement(t *testing.T) {
	m, _, id, _, a := retirementFixture(t)
	for _, change := range []func(*types.RunnerRetirement){
		func(p *types.RunnerRetirement) { p.Generation = uuid.New() },
		func(p *types.RunnerRetirement) { p.ExecID = "foreign" },
		func(p *types.RunnerRetirement) { p.ExitHash = "foreign" },
	} {
		bad := a
		bad.ID = uuid.New()
		p := *a.Retirement
		change(&p)
		bad.Retirement = &p
		if got, err := m.apply(t.Context(), bad); err != nil || got.Outcome != types.RunnerActionRefused {
			t.Fatalf("changed execution/exit proof = %+v %v, want refused", got, err)
		}
	}
	m.outputSettled = func(context.Context, uuid.UUID, string) (int64, bool, error) { return 8, false, nil }
	if got, err := m.apply(t.Context(), a); err != nil || got.Outcome != "deferred" {
		t.Fatalf("live output: %+v %v", got, err)
	}
	if m.state.Runs[id].Retirement != nil {
		t.Fatal("unsettled producer suppressed observations")
	}
	if _, err := New(t.TempDir(), uuid.New(), m.sub, uuid.UUID.String, nil, noErase); err == nil {
		t.Fatal("missing producer settlement callback admitted")
	}
	if _, err := New(t.TempDir(), uuid.New(), m.sub, uuid.UUID.String, noOutput, nil); err == nil {
		t.Fatal("missing output erasure callback admitted")
	}
}

func TestRetirementRecoversSealedSpoolBeforeLocalReceipt(t *testing.T) {
	m, sub, id, spool, a := retirementFixture(t)
	r := m.state.Runs[id]
	r.Retirement = a.Retirement
	m.state.Runs[id] = r
	m.state.Actions[a.ID] = appliedAction{Action: a}
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	if ok, err := spool.Seal(t.Context(), a.Retirement.Spool); err != nil || !ok {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(m.dir, m.runnerID, sub, uuid.UUID.String, noOutput, noErase)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if rows, err := allState(t.Context(), reopened); err != nil || len(rows) != 1 || rows[0].SandboxPresent {
		t.Fatal("sealed retirement restarted evidence producers")
	}
	if got, err := reopened.apply(t.Context(), a); err != nil || got.Outcome != "success" {
		t.Fatalf("crash receipt: %+v %v", got, err)
	}
}

func TestRetirementStateRefusesLostOrAlteredSettlement(t *testing.T) {
	for _, change := range []func(*localRun){
		func(r *localRun) { r.Retired = true; r.Retirement = nil },
		func(r *localRun) { p := *r.Retirement; p.Generation = uuid.New(); r.Retirement = &p },
		func(r *localRun) { p := *r.Containment; p.Contained = false; r.Containment = &p },
		func(r *localRun) { r.Quarantine = &types.RunnerQuarantine{ID: uuid.New()} },
	} {
		m, sub, id, _, a := retirementFixture(t)
		if _, err := m.apply(t.Context(), a); err != nil {
			t.Fatal(err)
		}
		r := m.state.Runs[id]
		change(&r)
		m.state.Runs[id] = r
		if err := m.save(); err != nil {
			t.Fatal(err)
		}
		if err := m.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := New(m.dir, m.runnerID, sub, uuid.UUID.String, noOutput, noErase)
		if err == nil {
			reopened.Close()
			t.Fatal("invalid retirement settlement survived restart")
		}
	}
}
