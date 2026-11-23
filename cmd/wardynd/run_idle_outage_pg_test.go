// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// idleOutageRunner creates a registered runner and removes it (after the runs below) at cleanup.
func idleOutageRunner(t *testing.T, ctx context.Context, pg store.PG) uuid.UUID {
	t.Helper()
	id := uuid.New()
	most := types.RunnerAmounts{CPUMillis: types.RunnerPoolCPUMillisMax, MemoryMiB: types.RunnerPoolMemoryMiBMax, DiskMiB: types.RunnerPoolDiskMiBMax}
	each := most
	each.DiskMiB = most.DiskMiB / types.RunnerDiskStepMiB * types.RunnerDiskStepMiB
	if _, err := pg.CreateRunner(ctx, types.Runner{
		ID: id, Owner: "idle-outage@example.com", Name: "laptop", PublicKey: make([]byte, 32),
		KeyFingerprint: "fp-" + id.String(), Version: "0.10.0",
		Limits: &types.RunnerLimits{RunsAtOnce: 1, Total: most, PerRun: each},
	}); err != nil {
		t.Fatalf("create runner: %v", err)
	}
	t.Cleanup(func() { _, _ = pg.Pool.Exec(context.Background(), `DELETE FROM runners WHERE id=$1`, id) })
	return id
}

// idleOutageRun creates a RUNNING run with a 60 s idle stop and removes it at cleanup.
func idleOutageRun(t *testing.T, ctx context.Context, pg store.PG, runner *uuid.UUID, mutate func(*types.AgentRun)) uuid.UUID {
	t.Helper()
	now := time.Now().UTC()
	id := uuid.New()
	run := types.AgentRun{
		ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: "op@example.com", Agent: "claude-code", Task: "idle outage probe",
		ConfinementClass: types.CC2, State: types.RunRunning, SPIFFEID: "spiffe://wardyn.test/agent-run/" + id.String(),
		RunnerTarget: "docker", SandboxRef: "wardyn-agent-" + id.String(), AutoStopAfterSec: 60,
	}
	if runner != nil {
		run.Placement, run.RunnerID = types.PlacementLocal, runner
	}
	if mutate != nil {
		mutate(&run)
	}
	if _, err := pg.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Cleanup(func() { _, _ = pg.Pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id=$1`, id) })
	return id
}

func TestPG_TheIdleReaperScanReadsEachRunsRunner(t *testing.T) {
	pool := revocationPool(t)
	ctx := context.Background()
	pg := store.PG{Pool: pool}
	r := idleOutageRunner(t, ctx, pg)
	a := idleOutageRun(t, ctx, pg, &r, nil)
	b := idleOutageRun(t, ctx, pg, nil, nil)

	sums, _, err := lifecycleStore{pool: pool}.ListRunningWithPolicy(ctx)
	if err != nil {
		t.Fatalf("idle scan: %v", err)
	}
	var sawA, sawB bool
	for _, s := range sums {
		switch s.ID {
		case a:
			sawA = true
			if s.RunnerID == nil || *s.RunnerID != r {
				t.Errorf("run on a runner: scanned runner %v, want %s", s.RunnerID, r)
			}
		case b:
			sawB = true
			if s.RunnerID != nil {
				t.Errorf("run on no runner: scanned runner %v, want nil", s.RunnerID)
			}
		}
	}
	if !sawA || !sawB {
		t.Fatalf("scan listed a=%v b=%v, want both", sawA, sawB)
	}
}

func TestPG_StampIdleClocksRestartsOnlyRunsTheReaperCouldStop(t *testing.T) {
	pool := revocationPool(t)
	ctx := context.Background()
	pg := store.PG{Pool: pool}
	r, s := idleOutageRunner(t, ctx, pg), idleOutageRunner(t, ctx, pg)
	runs := map[string]uuid.UUID{
		"a idle-stoppable, no runner": idleOutageRun(t, ctx, pg, nil, nil),
		"b background task":           idleOutageRun(t, ctx, pg, nil, func(x *types.AgentRun) { x.Experience = types.ExperienceBackground }),
		"c no idle stop":              idleOutageRun(t, ctx, pg, nil, func(x *types.AgentRun) { x.AutoStopAfterSec = 0 }),
		"d lost":                      idleOutageRun(t, ctx, pg, nil, nil),
		"e stopped":                   idleOutageRun(t, ctx, pg, nil, func(x *types.AgentRun) { x.State = types.RunStopped }),
		"f on runner R":               idleOutageRun(t, ctx, pg, &r, nil),
		"g on runner S":               idleOutageRun(t, ctx, pg, &s, nil),
	}
	reset := func() {
		t.Helper()
		for name, id := range runs {
			if _, err := pool.Exec(ctx, `UPDATE agent_runs SET updated_at = now() - interval '2 hours' WHERE id = $1`, id); err != nil {
				t.Fatalf("age %s: %v", name, err)
			}
		}
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET lost_at = now(), lost_reason = $2 WHERE id = $1`, runs["d lost"], string(types.LostOutage)); err != nil {
			t.Fatalf("lose d: %v", err)
		}
	}
	// moved reports whether the run's updated_at is within 5 s of the database's now().
	moved := func(name string) bool {
		t.Helper()
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT now() - updated_at < interval '5 seconds' FROM agent_runs WHERE id = $1`, runs[name]).Scan(&ok); err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return ok
	}
	ls := lifecycleStore{pool: pool}

	reset()
	n, err := ls.StampIdleClocks(ctx, nil)
	if err != nil {
		t.Fatalf("stamp all: %v", err)
	}
	for name, want := range map[string]bool{"a idle-stoppable, no runner": true, "f on runner R": true, "g on runner S": true,
		"b background task": false, "c no idle stop": false, "d lost": false, "e stopped": false} {
		if got := moved(name); got != want {
			t.Errorf("stamp of every run: %s moved=%v, want %v", name, got, want)
		}
	}
	if n < 3 {
		t.Errorf("stamp of every run touched %d rows, want at least 3", n)
	}

	reset()
	n, err = ls.StampIdleClocks(ctx, &r)
	if err != nil {
		t.Fatalf("stamp runner: %v", err)
	}
	for name := range runs {
		if got, want := moved(name), name == "f on runner R"; got != want {
			t.Errorf("stamp of runner R: %s moved=%v, want %v", name, got, want)
		}
	}
	if n != 1 {
		t.Errorf("stamp of runner R touched %d rows, want exactly 1", n)
	}

	// A stamp never moves a clock back.
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET updated_at = now() + interval '1 minute' WHERE id = $1`, runs["a idle-stoppable, no runner"]); err != nil {
		t.Fatalf("set a ahead: %v", err)
	}
	if _, err := ls.StampIdleClocks(ctx, nil); err != nil {
		t.Fatalf("stamp again: %v", err)
	}
	var ahead bool
	if err := pool.QueryRow(ctx, `SELECT updated_at > now() + interval '30 seconds' FROM agent_runs WHERE id = $1`, runs["a idle-stoppable, no runner"]).Scan(&ahead); err != nil || !ahead {
		t.Errorf("a stamp moved a later clock back (ahead=%v, err=%v)", ahead, err)
	}
}
