// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Postgres-backed tests for the run placement columns (migration 0138): what
// CreateRun persists, what GetRun reads back, what a pre-0.9 row defaults to,
// and what the closed CHECKs and the runners foreign key reject. Guarded by
// WARDYN_TEST_PG; skipped cleanly when unset.
//
// These COMPLEMENT store_runs_pg_test.go: that file round-trips the run's
// lifecycle columns, this one pins the placement vocabulary's four columns, so
// a dropped column or a transposed pair (placement against evidence_source —
// both TEXT) fails here rather than in an incident review.
package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_RunPlacement_RoundTripsEveryColumn: a local run's four placement
// columns survive create and read. placement and evidence_source are both TEXT,
// so the round trip is also what proves scanRun binds them to the right fields.
func TestPG_RunPlacement_RoundTripsEveryColumn(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()

	runnerID := uuid.New()
	_, err := store.NewPG(pool).CreateRunner(ctx, types.Runner{
		ID: runnerID, Owner: "placement@example.com", Name: "laptop",
		PublicKey: make([]byte, 32), KeyFingerprint: "fp-" + runnerID.String(), Version: "0.9.0",
	})
	if err != nil {
		t.Fatalf("create runner: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM runners WHERE id=$1`, runnerID) })

	r := newRun(types.RunStarting)
	r.Placement = types.PlacementLocal
	r.PlacementFilled = true
	r.RunnerID = &runnerID
	r.EvidenceSource = types.RunEvidenceRunnerAsserted
	persistRun(t, ctx, pool, r)

	got, err := store.NewPG(pool).GetRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.Placement != types.PlacementLocal {
		t.Errorf("placement = %q, want %q", got.Placement, types.PlacementLocal)
	}
	if !got.PlacementFilled {
		t.Error("placement_filled = false, want true (a resolved placement, not a requested one)")
	}
	if got.RunnerID == nil || *got.RunnerID != runnerID {
		t.Errorf("runner_id = %v, want %s", got.RunnerID, runnerID)
	}
	if got.EvidenceSource != types.RunEvidenceRunnerAsserted {
		t.Errorf("evidence_source = %q, want %q", got.EvidenceSource, types.RunEvidenceRunnerAsserted)
	}
}

// TestPG_RunPlacement_LegacyRowReadsEmpty: a row that predates 0.9 carries no
// placement at all, and every read of it must say so rather than guess one.
func TestPG_RunPlacement_LegacyRowReadsEmpty(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()

	r := newRun(types.RunPending)
	// Nothing set: what a create path that has not resolved a placement writes.
	got, err := store.NewPG(pool).CreateRun(ctx, r)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id=$1`, r.ID) })
	if got.Placement != "" || got.PlacementFilled || got.RunnerID != nil || got.EvidenceSource != "" {
		t.Fatalf("an unresolved placement came back as %+v, want all four zero", got)
	}
	var placement, evidence string
	var filled bool
	var runnerID *uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT placement, placement_filled, runner_id, evidence_source FROM agent_runs WHERE id=$1`, r.ID).
		Scan(&placement, &filled, &runnerID, &evidence); err != nil {
		t.Fatalf("read raw columns: %v", err)
	}
	if placement != "" || evidence != "" || filled || runnerID != nil {
		t.Errorf("stored columns = %q/%v/%v/%q, want ''/false/NULL/''", placement, filled, runnerID, evidence)
	}
}

// TestPG_RunPlacement_RemoteRunCarriesNoRunner: a remote run is the substrate's,
// so runner_id stays NULL — the wire omits the key rather than sending "".
func TestPG_RunPlacement_RemoteRunCarriesNoRunner(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()

	r := newRun(types.RunRunning)
	r.Placement = types.PlacementRemote
	r.EvidenceSource = types.RunEvidenceSubstrate
	persistRun(t, ctx, pool, r)

	got, err := store.NewPG(pool).GetRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.Placement != types.PlacementRemote || got.EvidenceSource != types.RunEvidenceSubstrate {
		t.Errorf("placement/evidence_source = %q/%q, want remote/substrate", got.Placement, got.EvidenceSource)
	}
	if got.RunnerID != nil {
		t.Errorf("runner_id = %v, want NULL on a remote run", got.RunnerID)
	}
	if got.PlacementFilled {
		t.Error("placement_filled = true on a run the caller placed explicitly")
	}
}

// TestPG_RunPlacement_ClosedValuesAndRunnerForeignKey: the CHECKs are the only
// thing between a typo and a run whose placement reads as neither, and the FK is
// what stops a run claiming a runner that does not exist.
func TestPG_RunPlacement_ClosedValuesAndRunnerForeignKey(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()

	for _, bad := range []struct {
		column, value string
	}{
		{"placement", "cloud"},
		{"placement", "LOCAL"},
		{"evidence_source", "runner"},
		{"evidence_source", "trusted"},
	} {
		r := newRun(types.RunPending)
		r.ID = uuid.New()
		if _, err := pool.Exec(ctx,
			`INSERT INTO agent_runs (id, created_at, created_by, agent, repo, task, confinement_class, state, spiffe_id, runner_target, `+bad.column+`)
			 VALUES ($1, now(), 'x', 'claude-code', 'acme/r', 't', 'CC1', 'PENDING', 'spiffe://x', 'docker', $2)`,
			r.ID, bad.value); err == nil {
			t.Errorf("%s = %q was accepted; the CHECK is not closed", bad.column, bad.value)
		}
	}

	r := newRun(types.RunPending)
	unknown := uuid.New()
	r.RunnerID = &unknown
	if _, err := store.NewPG(pool).CreateRun(ctx, r); err == nil {
		t.Error("a run naming a runner that does not exist was stored; the foreign key is missing")
	} else if !strings.Contains(err.Error(), "runners") {
		t.Errorf("err = %v, want a runners foreign-key violation", err)
	}
}

// TestPG_RunPlacement_RunnerErasureKeepsTheRun: deleting a runner clears the
// run's runner_id rather than deleting the run — the run row is the audit
// record, and its placement stays readable after the person is erased.
func TestPG_RunPlacement_RunnerErasureKeepsTheRun(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()

	runnerID := uuid.New()
	if _, err := store.NewPG(pool).CreateRunner(ctx, types.Runner{
		ID: runnerID, Owner: "erase@example.com", Name: "laptop",
		PublicKey: make([]byte, 32), KeyFingerprint: "fp-" + runnerID.String(), Version: "0.9.0",
	}); err != nil {
		t.Fatalf("create runner: %v", err)
	}
	r := newRun(types.RunRunning)
	r.Placement = types.PlacementLocal
	r.RunnerID = &runnerID
	r.EvidenceSource = types.RunEvidenceRunnerAsserted
	persistRun(t, ctx, pool, r)

	if _, err := pool.Exec(ctx, `DELETE FROM runners WHERE id=$1`, runnerID); err != nil {
		t.Fatalf("delete runner: %v", err)
	}
	got, err := store.NewPG(pool).GetRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("get run after runner erasure: %v", err)
	}
	if got.RunnerID != nil {
		t.Errorf("runner_id = %v, want NULL once the runner is gone", got.RunnerID)
	}
	if got.Placement != types.PlacementLocal || got.EvidenceSource != types.RunEvidenceRunnerAsserted {
		t.Errorf("erasing the runner changed the run's record: %+v", got)
	}
}

// TestPG_RunPlacement_VocabularyIsThePlacementPackage: the closed value set the
// store persists is placement's, not a second copy — placement.Remote,
// placement.Local and the two evidence sources are what reach the columns.
func TestPG_RunPlacement_VocabularyIsThePlacementPackage(t *testing.T) {
	if types.Placement(placement.Remote) != types.PlacementRemote || types.Placement(placement.Local) != types.PlacementLocal {
		t.Fatal("placement.Remote/Local are not types' values")
	}
	if types.RunEvidenceSource(placement.EvidenceSubstrate) != types.RunEvidenceSubstrate ||
		types.RunEvidenceSource(placement.EvidenceRunnerAsserted) != types.RunEvidenceRunnerAsserted {
		t.Fatal("placement's evidence sources are not types' values")
	}
	if placement.EvidenceFor(types.PlacementLocal) != types.RunEvidenceRunnerAsserted {
		t.Error("a local run's evidence source is not runner_asserted")
	}
}
