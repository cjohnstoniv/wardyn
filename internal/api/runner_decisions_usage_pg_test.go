// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Postgres-backed tests of the token metric for usage rows a runner replays from its spool. Guarded by
// WARDYN_TEST_PG; skipped cleanly when unset.
package api

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type replayFixture struct {
	srv    *Server
	pg     store.PG
	runner types.Runner
	run    types.AgentRun
	spool  *runnerspool.Spool
	next   int64
}

func newReplayFixture(t *testing.T, off bool) *replayFixture {
	t.Helper()
	pool := throwawayPGPool(t)
	pg := store.NewPG(pool)
	pg.ModelUsageOff = off
	ctx := context.Background()
	most := types.RunnerAmounts{CPUMillis: types.RunnerPoolCPUMillisMax, MemoryMiB: types.RunnerPoolMemoryMiBMax, DiskMiB: types.RunnerPoolDiskMiBMax}
	each := most
	each.DiskMiB = most.DiskMiB / int64(types.RunnerMaxRunsCeiling) / types.RunnerDiskStepMiB * types.RunnerDiskStepMiB
	id := uuid.New()
	rn, err := pg.CreateRunner(ctx, types.Runner{ID: id, Owner: "replay-owner", Name: "laptop", PublicKey: make([]byte, 32), KeyFingerprint: "fp-" + id.String(), Version: "0.9.0",
		Limits: &types.RunnerLimits{RunsAtOnce: types.RunnerMaxRunsCeiling, Total: most, PerRun: each}})
	if err != nil {
		t.Fatal(err)
	}
	if rn, err = pg.ClaimRunner(ctx, rn.ID, rn.Owner, rn.KeyFingerprint, time.Now()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	runID := uuid.New()
	run, err := pg.CreateRun(ctx, types.AgentRun{ID: runID, CreatedAt: now, UpdatedAt: now, CreatedBy: rn.Owner, Agent: "claude-code", Repo: "octocat/Hello-World",
		Task: "replay usage", ConfinementClass: types.CC2, State: types.RunRunning, SPIFFEID: "spiffe://wardyn.test/agent-run/" + runID.String(), RunnerTarget: "docker",
		Placement: types.PlacementLocal, RunnerID: &rn.ID, EvidenceSource: types.RunEvidenceRunnerAsserted})
	if err != nil {
		t.Fatal(err)
	}
	spool, err := runnerspool.Open(filepath.Join(t.TempDir(), "spool"), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { spool.Close() })
	srv := &Server{cfg: Config{Store: pg, ModelUsageOff: off,
		RunnerAuditPrepare: func(_ context.Context, ev types.AuditEvent) (types.AuditEvent, error) { return ev, nil }}}
	return &replayFixture{srv: srv, pg: pg, runner: rn, run: run, spool: spool, next: 1}
}

// usageEvent spools one usage record and returns it as the event the runner sends.
func (f *replayFixture) usageEvent(t *testing.T, u egress.ModelUsage) runnerwire.Event {
	t.Helper()
	ctx := context.Background()
	err := f.spool.Append(ctx, egress.DecisionLog{Request: egress.Request{RunID: f.run.ID, Host: u.Host, Method: "POST", Time: time.Now().UTC().Truncate(time.Microsecond)},
		Decision: egress.Allow, RuleSource: "llm:usage", Usage: &u})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := f.spool.After(ctx, f.next-1, 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("read the spooled row: %v %d", err, len(rows))
	}
	f.next++
	b, err := json.Marshal(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	return runnerwire.Event{Kind: runnerwire.EventSpool, RunID: &f.run.ID, Data: b}
}

func (f *replayFixture) tokens(vendor, direction string) int64 {
	f.srv.metrics.mu.Lock()
	defer f.srv.metrics.mu.Unlock()
	return f.srv.metrics.modelTok[[2]string{vendor, direction}]
}

func anthropicCall(in, out int64) egress.ModelUsage {
	return egress.ModelUsage{CallID: uuid.New(), Vendor: "anthropic", Host: "api.anthropic.com", Model: "claude-x", Input: in, Output: out, State: "counted"}
}

// A replayed usage row moves wardyn_model_tokens_total, as the direct path does, and moves it once: the same
// row delivered again, and another spool row carrying the same call id, count nothing more.
func TestDecisionIngestFromRunner_UsageRowCountsTokensOncePerCall(t *testing.T) {
	f := newReplayFixture(t, false)
	ctx := context.Background()
	u := anthropicCall(100, 50)
	first := f.usageEvent(t, u)
	if err := f.srv.DecisionIngestFromRunner(ctx, f.runner.ID, first); err != nil {
		t.Fatal(err)
	}
	if in, out := f.tokens("anthropic", "input"), f.tokens("anthropic", "output"); in != 100 || out != 50 {
		t.Fatalf("after one replay: input %d output %d, want 100 and 50", in, out)
	}
	if err := f.srv.DecisionIngestFromRunner(ctx, f.runner.ID, first); err != nil {
		t.Fatal(err)
	}
	if in := f.tokens("anthropic", "input"); in != 100 {
		t.Fatalf("a retried replay counted again: input %d", in)
	}
	// A new spool sequence for a call id already stored.
	if err := f.srv.DecisionIngestFromRunner(ctx, f.runner.ID, f.usageEvent(t, u)); err != nil {
		t.Fatal(err)
	}
	if in := f.tokens("anthropic", "input"); in != 100 {
		t.Fatalf("a second spool row with the same call id counted again: input %d", in)
	}
	// A different call counts.
	if err := f.srv.DecisionIngestFromRunner(ctx, f.runner.ID, f.usageEvent(t, anthropicCall(7, 3))); err != nil {
		t.Fatal(err)
	}
	if in := f.tokens("anthropic", "input"); in != 107 {
		t.Fatalf("a new call: input %d, want 107", in)
	}
}

// A replay that rolls back counts nothing: the metric moves only after the commit.
func TestDecisionIngestFromRunner_RolledBackUsageRowCountsNothing(t *testing.T) {
	f := newReplayFixture(t, false)
	ctx := context.Background()
	pool := f.pg.Pool
	for _, stmt := range []string{
		`CREATE FUNCTION refuse_cursor_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'cursor update refused'; END $$`,
		`CREATE TRIGGER refuse_cursor_update BEFORE UPDATE ON runner_spool_cursors FOR EACH ROW EXECUTE FUNCTION refuse_cursor_update()`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.srv.DecisionIngestFromRunner(ctx, f.runner.ID, f.usageEvent(t, anthropicCall(100, 50))); err == nil {
		t.Fatal("the replay succeeded with its cursor update refused")
	}
	if in := f.tokens("anthropic", "input"); in != 0 {
		t.Fatalf("a rolled-back replay counted %d input tokens", in)
	}
}

// With the opt-out on, a replayed usage row is acknowledged and counts nothing.
func TestDecisionIngestFromRunner_OptOutUsageRowCountsNothing(t *testing.T) {
	f := newReplayFixture(t, true)
	if err := f.srv.DecisionIngestFromRunner(context.Background(), f.runner.ID, f.usageEvent(t, anthropicCall(100, 50))); err != nil {
		t.Fatal(err)
	}
	if in := f.tokens("anthropic", "input"); in != 0 {
		t.Fatalf("counted %d input tokens with the opt-out on", in)
	}
}
