// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Postgres-backed tests for run_model_usage: one row per call id, the per-run sums, and a runner's replay
// of a usage row inside its tx. Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func usageCall(host, model string, in, out int64) egress.ModelUsage {
	return egress.ModelUsage{CallID: uuid.New(), Vendor: "anthropic", Host: host, Model: model, Input: in, Output: out, State: "counted"}
}

func TestPG_ModelUsageDuplicateCallIsStoredOnce(t *testing.T) {
	_, st, _, run, _ := spoolPGFixture(t)
	ctx := context.Background()
	u := usageCall("api.anthropic.com", "claude-x", 10, 5)
	var stored atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := st.InsertModelUsage(ctx, run.ID, u, time.Now(), "")
			if err != nil {
				t.Error(err)
			}
			if ok {
				stored.Add(1)
			}
		}()
	}
	wg.Wait()
	if stored.Load() != 1 {
		t.Fatalf("one call id stored %d times", stored.Load())
	}
	sum, err := st.RunModelUsage(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Groups) != 1 || sum.Groups[0].Calls != 1 || sum.Groups[0].Input != 10 || sum.Groups[0].Output != 5 {
		t.Fatalf("a retried record counted twice: %+v", sum.Groups)
	}
}

func TestPG_ModelUsageSumsByModelAndKeepsStatesApart(t *testing.T) {
	_, st, _, run, _ := spoolPGFixture(t)
	ctx := context.Background()
	other := newRun(types.RunRunning)
	other, err := st.CreateRun(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	a1, a2 := usageCall("api.anthropic.com", "claude-x", 10, 5), usageCall("api.anthropic.com", "claude-x", 1, 2)
	a2.CacheRead, a2.CacheWrite = 7, 3
	b := usageCall("api.anthropic.com", "claude-y", 4, 4)
	b.State, b.Reason, b.Input, b.Output = "partial", "stream_cut", 4, 0
	c := egress.ModelUsage{CallID: uuid.New(), Vendor: "openai", Host: "api.openai.com", Model: "gpt-x", Reasoning: 9, Input: 20, Output: 6, State: "counted"}
	d := egress.ModelUsage{CallID: uuid.New(), Vendor: "openai", Host: "api.openai.com", Model: "gpt-x", State: "uncounted", Reason: "stream_without_usage"}
	e := egress.ModelUsage{CallID: uuid.New(), Vendor: "openai", Host: "api.openai.com", Model: "gpt-x", State: "uncounted", Reason: "stream_without_usage"}
	foreign := usageCall("api.anthropic.com", "claude-x", 1000, 1000)
	for _, u := range []egress.ModelUsage{a1, a2, b, c, d, e} {
		if _, err := st.InsertModelUsage(ctx, run.ID, u, time.Now(), ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.InsertModelUsage(ctx, other.ID, foreign, time.Now(), ""); err != nil {
		t.Fatal(err)
	}
	sum, err := st.RunModelUsage(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sum.UpdatedAt == nil || len(sum.Groups) != 3 {
		t.Fatalf("want 3 groups and a time, got %+v", sum)
	}
	x, y, g := sum.Groups[0], sum.Groups[1], sum.Groups[2] // ordered by vendor, host, model
	if x.Model != "claude-x" || x.Input != 11 || x.Output != 7 || x.CacheRead != 7 || x.CacheWrite != 3 || x.Calls != 2 || x.UncountedCalls != 0 {
		t.Errorf("claude-x: %+v", x)
	}
	if y.Model != "claude-y" || y.PartialCalls != 1 || y.Calls != 1 {
		t.Errorf("claude-y: %+v", y)
	}
	if g.Vendor != "openai" || g.Input != 20 || g.Reasoning != 9 || g.Calls != 3 || g.UncountedCalls != 2 || g.UncountedReasons["stream_without_usage"] != 2 {
		t.Errorf("gpt-x: %+v", g)
	}
	empty, err := st.RunModelUsage(ctx, uuid.New())
	if err != nil || len(empty.Groups) != 0 || empty.UpdatedAt != nil {
		t.Errorf("a run with no rows: %+v %v", empty, err)
	}
}

func TestPG_ModelUsageRefusesAnInvalidRecord(t *testing.T) {
	_, st, _, run, _ := spoolPGFixture(t)
	bad := usageCall("api.anthropic.com", "m", -1, 0)
	if _, err := st.InsertModelUsage(context.Background(), run.ID, bad, time.Now(), ""); err == nil {
		t.Fatal("a negative count was stored")
	}
	bad = usageCall("api.anthropic.com", "m", 1, 1)
	bad.State = "guessed"
	if _, err := st.InsertModelUsage(context.Background(), run.ID, bad, time.Now(), ""); err == nil {
		t.Fatal("a state outside the closed set was stored")
	}
}

// A replayed usage row is stored in the replay tx, advances the cursor, and writes no audit row.
func TestPG_RunnerSpoolUsageRowStoresUsageAndNoAudit(t *testing.T) {
	pool, st, rn, run, row := spoolPGFixture(t)
	ctx := context.Background()
	u := usageCall("api.anthropic.com", "claude-x", 12, 3)
	row.Decision.RuleSource = "llm:usage"
	row.Decision.Usage = &u
	row.Decision.Request.Time = time.Now().UTC().Truncate(time.Microsecond)
	row = rehashSpoolRow(t, row)
	first, err := st.IngestRunnerDecision(ctx, rn.ID, row, prepareSpoolTest)
	if err != nil || !first {
		t.Fatalf("replay: %v %v", first, err)
	}
	if c, a := spoolCounts(t, pool, run.ID); c != 1 || a != 0 {
		t.Fatalf("want a cursor and no audit row, got %d cursor/%d audit", c, a)
	}
	var n int
	var evidence string
	if err := pool.QueryRow(ctx, `SELECT count(*), max(evidence_source) FROM run_model_usage WHERE run_id=$1`, run.ID).Scan(&n, &evidence); err != nil {
		t.Fatal(err)
	}
	if n != 1 || evidence != string(types.RunEvidenceRunnerAsserted) {
		t.Fatalf("usage rows %d, evidence %q", n, evidence)
	}
	if first, err := st.IngestRunnerDecision(ctx, rn.ID, row, prepareSpoolTest); err != nil || first {
		t.Fatalf("a second replay of the row: %v %v", first, err)
	}
	if ok, err := st.InsertModelUsage(ctx, run.ID, u, time.Now(), ""); err != nil || ok {
		t.Fatalf("the same call id posted directly after the replay: %v %v", ok, err)
	}
	sum, err := st.RunModelUsage(ctx, run.ID)
	if err != nil || len(sum.Groups) != 1 || sum.Groups[0].Calls != 1 {
		t.Fatalf("summary %+v %v", sum, err)
	}
}

// A replay whose cursor update fails, after the usage row was inserted, leaves neither the usage row nor
// the cursor: the throwaway database gets a trigger that refuses the update the replay ends with.
func TestPG_RunnerSpoolUsageRowRollsBackWithTheCursor(t *testing.T) {
	pool, st, rn, run, row := spoolPGFixture(t)
	ctx := context.Background()
	for _, stmt := range []string{
		`CREATE FUNCTION refuse_cursor_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'cursor update refused'; END $$`,
		`CREATE TRIGGER refuse_cursor_update BEFORE UPDATE ON runner_spool_cursors FOR EACH ROW EXECUTE FUNCTION refuse_cursor_update()`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	u := usageCall("api.anthropic.com", "claude-x", 1, 1)
	row.Decision.RuleSource = "llm:usage"
	row.Decision.Usage = &u
	row.Decision.Request.Time = time.Now().UTC().Truncate(time.Microsecond)
	row = rehashSpoolRow(t, row)
	if _, err := st.IngestRunnerDecision(ctx, rn.ID, row, prepareSpoolTest); err == nil {
		t.Fatal("the replay succeeded with its cursor update refused")
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_model_usage WHERE call_id=$1`, u.CallID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the usage row outlived its rolled-back replay: %d %v", n, err)
	}
	if c, a := spoolCounts(t, pool, run.ID); c != 0 || a != 0 {
		t.Fatalf("a failed replay advanced %d cursor/%d audit", c, a)
	}
}

// With the opt-out on, a replayed usage row is acknowledged and stored nowhere.
func TestPG_RunnerSpoolUsageRowIsDroppedWhenOff(t *testing.T) {
	pool, st, rn, run, row := spoolPGFixture(t)
	ctx := context.Background()
	st.ModelUsageOff = true
	u := usageCall("api.anthropic.com", "claude-x", 3, 3)
	row.Decision.RuleSource = "llm:usage"
	row.Decision.Usage = &u
	row = rehashSpoolRow(t, row)
	if first, err := st.IngestRunnerDecision(ctx, rn.ID, row, prepareSpoolTest); err != nil || !first {
		t.Fatalf("replay: %v %v", first, err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_model_usage WHERE run_id=$1`, run.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("usage rows with counting off: %d %v", n, err)
	}
	if c, a := spoolCounts(t, pool, run.ID); c != 1 || a != 0 {
		t.Fatalf("want the row acknowledged and no audit row, got %d cursor/%d audit", c, a)
	}
}

func TestPG_RunOpaqueTunnelsCountsBypassRows(t *testing.T) {
	_, st, rn, run, row := spoolPGFixture(t)
	ctx := context.Background()
	if n, err := st.RunOpaqueTunnels(ctx, run.ID); err != nil || n != 0 {
		t.Fatalf("before: %d %v", n, err)
	}
	row.Decision.Scan = &egress.ScanSummary{Action: "bypass", Coverage: "tunneled-opaque"}
	row = rehashSpoolRow(t, row)
	if _, err := st.IngestRunnerDecision(ctx, rn.ID, row, prepareSpoolTest); err != nil {
		t.Fatal(err)
	}
	if n, err := st.RunOpaqueTunnels(ctx, run.ID); err != nil || n != 1 {
		t.Fatalf("after one bypass row: %d %v", n, err)
	}
}
