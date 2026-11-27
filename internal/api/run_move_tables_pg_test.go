// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// onSourceRunner binds the fixture's run to a claimed runner of its owner that serves moves: a laptop run moving to
// the organisation's executors.
func (f *movePGFixture) onSourceRunner(t *testing.T) uuid.UUID {
	t.Helper()
	rid := uuid.New()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	rn, err := f.st.CreateRunner(f.ctx, types.Runner{ID: rid, Owner: f.run.CreatedBy, Name: "laptop", PublicKey: pub, KeyFingerprint: rid.String(),
		Version: "0.11.0", OrgURLSHA256: federation.OrgURLSHA256(admissionOrgURL), Limits: roomyRunnerLimits()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.ClaimRunner(f.ctx, rn.ID, rn.Owner, rn.KeyFingerprint, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(f.ctx, `UPDATE agent_runs SET runner_id = $2, placement = 'local' WHERE id = $1`, f.run.ID, rn.ID); err != nil {
		t.Fatal(err)
	}
	registerMoveRunnerCaps(t, f.srv, rn.ID, moveCapsAll)
	f.srv.cfg.RunnerOrgURL = admissionOrgURL
	f.run = f.current(t)
	return rn.ID
}

// TestMoveExec_RunnerBoundTables: one subtest per runner-bound table the source's rows decide at commit or
// admission, for a laptop run moving to the organisation's executors.
func TestMoveExec_RunnerBoundTables(t *testing.T) {
	for _, c := range []struct {
		table, sql, reason string
	}{
		{"runner_execution_exits", `INSERT INTO runner_execution_exits (run_id, generation, runner_id, payload, payload_hash)
			VALUES ($1, gen_random_uuid(), $2, '{}'::jsonb, 'h')`, reasonMoveSourceUnsettled},
		{"run_containment_pending", `INSERT INTO run_containment_pending (run_id, runner_id, kind, since) VALUES ($1, $2, 'agent_only', now())`,
			reasonMoveSourceUnsettled},
		{"run_teardown_holds", `INSERT INTO run_teardown_holds (run_id, runner_id, kind, since) VALUES ($1, $2, 'kill', now())`,
			reasonMoveSourceUnsettled},
		{"runner_pending_actions", "", reasonMoveSourceUnsettled},
	} {
		t.Run(c.table, func(t *testing.T) {
			f := newMovePGFixture(t, nil)
			rid := f.onSourceRunner(t)
			f.mr.onImport = func() {
				var err error
				if c.sql == "" {
					// An open end action for the run's sandbox, queued through the store's own teardown path.
					_, _, err = f.st.QueueRunnerTeardown(f.ctx, rid, types.RunnerActionEnd, f.run.SandboxRef)
				} else {
					_, err = f.st.Pool.Exec(f.ctx, c.sql, f.run.ID, rid)
				}
				if err != nil {
					t.Error(err)
				}
			}
			if code, body := f.move(t, "org"); code != http.StatusAccepted {
				t.Fatalf("move = %d %s", code, body)
			}
			moves := f.moves(t)
			if len(moves) != 1 || moves[0].State != types.MoveFailed || moves[0].Failure != c.reason {
				t.Fatalf("moves = %+v, want one failed %s", moves, c.reason)
			}
			if cur := f.current(t); cur.RunnerID == nil || *cur.RunnerID != rid {
				t.Error("the unsettled run's row left its runner")
			}
		})
	}
	t.Run("runner_spool_cursors", func(t *testing.T) {
		f := newMovePGFixture(t, nil)
		rid := f.onSourceRunner(t)
		if _, err := f.st.Pool.Exec(f.ctx, `INSERT INTO runner_spool_cursors (run_id, runner_id, spool_id) VALUES ($1, $2, gen_random_uuid())`,
			f.run.ID, rid); err != nil {
			t.Fatal(err)
		}
		if code, body := f.move(t, "org"); code != http.StatusAccepted {
			t.Fatalf("move = %d %s", code, body)
		}
		if moves := f.moves(t); len(moves) != 1 || moves[0].State != types.MoveCommitted {
			t.Fatalf("moves = %+v, want one committed", moves)
		}
		var n int
		if err := f.st.Pool.QueryRow(f.ctx, `SELECT count(*) FROM runner_spool_cursors WHERE run_id = $1`, f.run.ID).Scan(&n); err != nil || n != 0 {
			t.Errorf("spool cursors after commit = %d %v, want the run's closed", n, err)
		}
		if cur := f.current(t); cur.RunnerID != nil || cur.Placement != types.PlacementRemote {
			t.Errorf("run row = %s on %v, want the organisation's executor", cur.Placement, cur.RunnerID)
		}
		if f.mr.unbound == nil {
			t.Error("the source runner got no notice of the move")
		}
	})
	// The rows a move keeps: each is seeded on the source runner and is still there after commit.
	for table, seed := range map[string][]string{
		"run_proxy_configs": nil,
		"runner_executions": {`INSERT INTO runner_executions (run_id, runner_id, generation, operation) VALUES ($1, $2, gen_random_uuid(), 'start')`},
		"runner_inventory_candidates": {`INSERT INTO runner_inventories (runner_id, session_id, owner) VALUES ($2, gen_random_uuid(), 'mover@example.com')`,
			`INSERT INTO runner_inventory_candidates (runner_id, run_id, run_version, ref, generation) VALUES ($2, $1, 'v1', 'ref-src', gen_random_uuid())`},
		"runner_inventory_observations": {`INSERT INTO runner_inventories (runner_id, session_id, owner) VALUES ($2, gen_random_uuid(), 'mover@example.com')`,
			`INSERT INTO runner_inventory_observations (runner_id, run_id, assertion) VALUES ($2, $1, '{}'::jsonb)`},
		"runner_retirements": {`INSERT INTO runner_retirements (run_id, runner_id, obligation_id, ref) VALUES ($1, $2, gen_random_uuid(), 'ref-src')`},
		"run_node_records": {`INSERT INTO run_node_records (id, run_id, runner_id, action_id, attestation, source, source_id)
			VALUES (gen_random_uuid(), $1, $2, gen_random_uuid(), 'node_powered_off', 'other', 'node-1')`},
	} {
		t.Run(table, func(t *testing.T) {
			f := newMovePGFixture(t, nil)
			rid := f.onSourceRunner(t)
			for _, sql := range seed {
				if _, err := f.st.Pool.Exec(f.ctx, sql, f.run.ID, rid); err != nil {
					t.Fatal(err)
				}
			}
			count := func() int {
				t.Helper()
				var n int
				if err := f.st.Pool.QueryRow(f.ctx, `SELECT count(*) FROM `+table+` WHERE run_id = $1`, f.run.ID).Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n
			}
			before := count()
			if before == 0 {
				t.Fatal("nothing seeded")
			}
			if code, body := f.move(t, "org"); code != http.StatusAccepted {
				t.Fatalf("move = %d %s", code, body)
			}
			if moves := f.moves(t); len(moves) != 1 || moves[0].State != types.MoveCommitted {
				t.Fatalf("moves = %+v, want one committed", moves)
			}
			if after := count(); after != before {
				t.Errorf("%s rows after commit = %d, want the %d kept", table, after, before)
			}
		})
	}
	t.Run("kept_machines", func(t *testing.T) {
		f := newMovePGFixture(t, nil)
		rid := f.onSourceRunner(t)
		if _, err := f.st.Pool.Exec(f.ctx, `INSERT INTO kept_machines (id, name, owner_subject, runner_id, base, base_prepared_at, state, run_id, expires_at)
			VALUES (gen_random_uuid(), 'kept-1', 'mover@example.com', $2, 'ubuntu', now(), 'in_use', $1, now() + interval '1 day')`, f.run.ID, rid); err != nil {
			t.Fatal(err)
		}
		code, body := f.move(t, "org")
		if reason, _ := moveAnswer(t, body); code != http.StatusConflict || reason != reasonMoveKeptMachine {
			t.Errorf("kept machine = %d %s, want 409 move_kept_machine", code, body)
		}
		if f.fk.count("stop") != 0 || len(f.moves(t)) != 0 || f.mr.exportCount() != 0 {
			t.Errorf("stops %d rows %d exports %d, want none", f.fk.count("stop"), len(f.moves(t)), f.mr.exportCount())
		}
	})
	t.Run("sources", func(t *testing.T) {
		f := newMovePGFixture(t, nil)
		f.onSourceRunner(t)
		if _, err := f.st.Pool.Exec(f.ctx, `DELETE FROM run_sandbox_specs WHERE run_id = $1`, f.run.ID); err != nil {
			t.Fatal(err)
		}
		if err := f.srv.storeRunSandboxSpec(f.ctx, f.run.ID, runSandboxSpecRecord{Image: "img", Interactive: true,
			Mounts: []runner.Mount{{BoundRunner: f.run.RunnerID.String()}}}); err != nil {
			t.Fatal(err)
		}
		code, body := f.move(t, "org")
		if reason, layer := moveAnswer(t, body); code != http.StatusForbidden || reason != reasonMoveDestinationRefused || layer != "bound_folder" {
			t.Errorf("bound folder = %d %s, want 403 move_destination_refused bound_folder", code, body)
		}
	})
	t.Run("runner_resident_runs", func(t *testing.T) {
		f := newMovePGFixture(t, nil)
		f.onSourceRunner(t)
		if _, err := f.st.Pool.Exec(f.ctx, `DELETE FROM run_sandbox_specs WHERE run_id = $1`, f.run.ID); err != nil {
			t.Fatal(err)
		}
		if err := f.srv.storeRunSandboxSpec(f.ctx, f.run.ID, runSandboxSpecRecord{Image: "img", Interactive: true, Resident: true}); err != nil {
			t.Fatal(err)
		}
		code, body := f.move(t, "org")
		if reason, _ := moveAnswer(t, body); code != http.StatusForbidden || reason != reasonMoveResidentCredential {
			t.Errorf("resident run = %d %s, want 403 move_resident_credential", code, body)
		}
	})
}
