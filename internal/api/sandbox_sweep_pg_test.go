// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The kill-tail recovery in SweepTerminalSandboxes against a REAL Postgres: a
// KILLED run whose kill already settled must never be re-killed, however long
// its audit trail is. The per-run audit read is `ORDER BY seq ASC LIMIT n`
// there, which an in-memory double does not model — a window read of the
// OLDEST rows missed a settled run.kill behind more than a window's worth of
// earlier events and re-killed the run on every sweep.
//
// Guarded by WARDYN_TEST_PG (throwawayPGPool); skipped cleanly when unset.

package api

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// goneSandboxRunner is killCountRunner (kill_rekill_test.go) reporting every
// sandbox as already gone, so the sweep's ref-probe leaves the run alone and
// any KillSandbox call can only come from a kill-tail recovery.
type goneSandboxRunner struct {
	*killCountRunner
}

func (r *goneSandboxRunner) Status(context.Context, string) (runner.Status, error) {
	return runner.Status{State: types.RunStopped}, nil
}

func TestPG_SweepLeavesSettledKilledRunsAlone(t *testing.T) {
	cases := []struct {
		name    string
		prior   int    // audit rows written before the settling row
		action  string // the row that settles the run
		outcome string
	}{
		// More prior rows than any fixed read window: the settled run.kill is
		// the NEWEST row, past the oldest-first window.
		{"run.kill after a long trail", 60, "run.kill", "success"},
		// reclaimProbeRun: KILLED through finalizeRunTail, which writes its own
		// action and never run.kill.
		{"probe reclaimed", 3, "site_config.probe.kill", "failure"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := throwawayPGPool(t)
			ctx := context.Background()
			pg := store.NewPG(pool)

			id := uuid.New()
			now := time.Now().UTC()
			if _, err := pg.CreateRun(ctx, types.AgentRun{
				ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: "t@example.com", Agent: "claude-code",
				ConfinementClass: types.CC1, State: types.RunKilled, RunnerTarget: "docker",
				SandboxRef: "sbx-settled", SPIFFEID: "spiffe://wardyn.local/agent-run/" + id.String(),
			}); err != nil {
				t.Fatalf("create run: %v", err)
			}
			// Age the KILLED transition past killTailRecoveryGrace, on the
			// database's clock like every other updated_at writer.
			age := int((killTailRecoveryGrace + time.Minute).Seconds())
			if _, err := pool.Exec(ctx,
				`UPDATE agent_runs SET updated_at = now() - $2 * interval '1 second' WHERE id=$1`, id, age); err != nil {
				t.Fatalf("age run: %v", err)
			}
			seed := func(action, outcome string) {
				t.Helper()
				ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), RunID: &id,
					ActorType: types.ActorSystem, Actor: "wardynd", Action: action, Target: id.String(), Outcome: outcome}
				if err := store.InsertAuditEvent(ctx, pool, &ev); err != nil {
					t.Fatalf("seed %s: %v", action, err)
				}
			}
			for i := 0; i < tc.prior; i++ {
				seed("egress.allow", "success")
			}
			seed(tc.action, tc.outcome)
			killRows := func() int {
				t.Helper()
				var n int
				if err := pool.QueryRow(ctx,
					`SELECT count(*) FROM audit_events WHERE run_id=$1 AND action='run.kill'`, id).Scan(&n); err != nil {
					t.Fatalf("count run.kill rows: %v", err)
				}
				return n
			}
			before := killRows()

			h := newHarness(t)
			cfg := baseTestConfig(h, pg)
			// The sweep's own audit rows land in the same table it reads, so a
			// re-kill on pass 1 is visible to pass 2 exactly as in production.
			cfg.Audit = &store.Recorder{Pool: pool}
			rr := &goneSandboxRunner{&killCountRunner{fakeRunner: &fakeRunner{}}}
			cfg.Runner = rr
			cfg.Broker = h.broker
			srv := New(cfg)

			for pass := 1; pass <= 2; pass++ {
				swept, err := srv.SweepTerminalSandboxes(ctx)
				if err != nil {
					t.Fatalf("sweep pass %d: %v", pass, err)
				}
				if swept != 0 {
					t.Errorf("sweep pass %d: swept = %d, want 0 (the run is settled)", pass, swept)
				}
			}
			if got := rr.kills.Load(); got != 0 {
				t.Errorf("KillSandbox calls = %d, want 0", got)
			}
			if len(h.broker.revoked) != 0 {
				t.Errorf("broker revokes = %v, want none", h.broker.revoked)
			}
			if got := killRows() - before; got != 0 {
				t.Errorf("new run.kill rows = %d, want 0", got)
			}
		})
	}
}
