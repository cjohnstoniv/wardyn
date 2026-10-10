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
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

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
	pg := store.NewPG(pool)
	created, err := pg.CreateRun(ctx, r)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id=$1`, r.ID) })
	assertRunPlacement(t, created, r)

	got, err := pg.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	assertRunPlacement(t, got, r)
	if changed, err := pg.UpdateRunStateIf(ctx, r.ID, types.RunStarting, types.RunRunning); err != nil || !changed {
		t.Fatalf("state transition: changed=%v err=%v", changed, err)
	}
	runs, err := pg.ListRuns(ctx)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	for _, got := range runs {
		if got.ID == r.ID {
			assertRunPlacement(t, got, r)
			return
		}
	}
	t.Fatal("created run missing from list")
}

func assertRunPlacement(t *testing.T, got, want types.AgentRun) {
	t.Helper()
	if got.Placement != want.Placement || got.PlacementFilled != want.PlacementFilled ||
		got.EvidenceSource != want.EvidenceSource || got.RunnerID == nil || *got.RunnerID != *want.RunnerID {
		t.Errorf("placement fields = %q/%v/%v/%q, want %q/%v/%v/%q", got.Placement, got.PlacementFilled,
			got.RunnerID, got.EvidenceSource, want.Placement, want.PlacementFilled, want.RunnerID, want.EvidenceSource)
	}
}

// TestPG_RunPlacement_LegacyRowReadsEmpty: a row that predates 0.9 carries no
// placement at all, and every read of it must say so rather than guess one.
func TestPG_RunPlacement_LegacyRowReadsEmpty(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()

	r := newRun(types.RunPending)
	// Omit all four columns, as a writer predating the migration would.
	_, err := pool.Exec(ctx, `INSERT INTO agent_runs
		(id, created_at, created_by, agent, repo, task, confinement_class, state, spiffe_id, runner_target)
		VALUES ($1, now(), 'legacy@example.com', 'claude-code', 'acme/r', 't', 'CC1', 'PENDING', 'spiffe://x', 'docker')`, r.ID)
	if err != nil {
		t.Fatalf("insert legacy-shaped row: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id=$1`, r.ID) })
	got, err := store.NewPG(pool).GetRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("get legacy row: %v", err)
	}
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
		_, err := pool.Exec(ctx,
			`INSERT INTO agent_runs (id, created_at, created_by, agent, repo, task, confinement_class, state, spiffe_id, runner_target, `+bad.column+`)
			 VALUES ($1, now(), 'x', 'claude-code', 'acme/r', 't', 'CC1', 'PENDING', 'spiffe://x', 'docker', $2)`,
			r.ID, bad.value)
		assertPlacementConstraint(t, err, "23514", "agent_runs_"+bad.column+"_check")
	}

	r := newRun(types.RunPending)
	unknown := uuid.New()
	r.RunnerID = &unknown
	_, err := store.NewPG(pool).CreateRun(ctx, r)
	assertPlacementConstraint(t, err, "23503", "agent_runs_runner_id_fkey")
}

func assertPlacementConstraint(t *testing.T, err error, code, constraint string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code || pgErr.ConstraintName != constraint {
		t.Errorf("err = %v, want SQLSTATE %s from %s", err, code, constraint)
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
