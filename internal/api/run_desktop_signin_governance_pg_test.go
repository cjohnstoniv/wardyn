// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The governance end reason is stored as it is written (desktop_signins.end_reason has no value list), and the
// closed sign-in leaves the open list. Guarded by WARDYN_TEST_PG.
func TestDesktopSigninSweep_GovernanceReason_PG(t *testing.T) {
	pool := throwawayPGPool(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	run, err := st.CreateRun(ctx, types.AgentRun{ID: uuid.New(), CreatedBy: "alice", Agent: "claude-code",
		ConfinementClass: types.CC1, State: types.RunRunning, RunnerTarget: "docker", Task: "t"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	sum := sha256.Sum256([]byte("cred"))
	si := types.DesktopSignin{ID: uuid.New(), RunID: run.ID, Holder: "alice", CredentialHash: sum[:],
		BeganAt: now, ExpiresAt: now.Add(30 * time.Minute)}
	if err := st.BeginDesktopSignin(ctx, si); err != nil {
		t.Fatal(err)
	}
	if ended, err := st.EndDesktopSignin(ctx, si.ID, signinEndGovernance, now.Add(time.Minute)); err != nil || !ended {
		t.Fatalf("EndDesktopSignin = %v %v, want it closed", ended, err)
	}
	var reason string
	if err := pool.QueryRow(ctx, `SELECT end_reason FROM desktop_signins WHERE id = $1`, si.ID).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != signinEndGovernance {
		t.Fatalf("end_reason = %q, want %q", reason, signinEndGovernance)
	}
	open, err := st.ListOpenDesktopSignins(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range open {
		if o.ID == si.ID {
			t.Fatal("ListOpenDesktopSignins still lists the closed sign-in")
		}
	}
}
