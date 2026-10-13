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

// TestPG_TheIdleReaperScanReadsABackgroundRunAsNeverStopped pins that a
// background task has no idle stop at the reaper itself, whatever its row
// carries: the scan reports 0 for it, and the stored value for an interactive run.
func TestPG_TheIdleReaperScanReadsABackgroundRunAsNeverStopped(t *testing.T) {
	pool := revocationPool(t)
	ctx := context.Background()
	now := time.Now().UTC()
	idleStop := map[types.RunExperience]uuid.UUID{
		types.ExperienceBackground:  uuid.New(),
		types.ExperienceInteractive: uuid.New(),
		"":                          uuid.New(),
	}
	for experience, id := range idleStop {
		if _, err := (store.PG{Pool: pool}).CreateRun(ctx, types.AgentRun{
			ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: "op@example.com", Agent: "claude-code", Task: "idle probe",
			ConfinementClass: types.CC2, State: types.RunRunning, SPIFFEID: "spiffe://wardyn.test/agent-run/" + id.String(),
			RunnerTarget: "docker", SandboxRef: "wardyn-agent-" + id.String(), AutoStopAfterSec: 60, Experience: experience,
		}); err != nil {
			t.Fatalf("create %q run: %v", experience, err)
		}
		id := id
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id=$1`, id) })
	}
	sums, _, err := lifecycleStore{pool: pool}.ListRunningWithPolicy(ctx)
	if err != nil {
		t.Fatalf("idle scan: %v", err)
	}
	got := map[uuid.UUID]int{}
	for _, s := range sums {
		got[s.ID] = s.PolicyAutoStopAfterSec
	}
	for experience, id := range idleStop {
		want := 60
		if experience == types.ExperienceBackground {
			want = 0
		}
		if v, ok := got[id]; !ok || v != want {
			t.Errorf("%q run: scanned idle stop %d (listed=%v), want %d", experience, v, ok, want)
		}
	}
}
