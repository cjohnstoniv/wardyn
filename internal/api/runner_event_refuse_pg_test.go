// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

const eventRefuseRowsSQL = `SELECT count(*) FROM audit_events WHERE action='runner.event.refuse' AND actor=$1`

// The livelock witness end to end (#3648): a real runner session over the production Hub sends an exit the
// organisation refuses for good (a generation it never issued). The exit is acknowledged once, audited once,
// and the same session stays up.
func TestPG_RefusedExitDoesNotLivelock(t *testing.T) {
	f := newAdmissionFixture(t)
	s := f.connect(t, nil, f.liveRow())
	if !f.waitReady(s) {
		t.Fatalf("session never reached READY: %v", s.result())
	}
	s.mu.Lock()
	peer := s.peer
	s.mu.Unlock()
	if peer == nil {
		t.Fatal("no runner peer after READY")
	}
	exit := runnerwire.AgentExit{Generation: uuid.New(), ExecID: "exec-1", Ref: f.run.SandboxRef, ObservedAt: time.Now().UTC()}
	data, err := runnerwire.Marshal(exit)
	if err != nil {
		t.Fatal(err)
	}
	if err := peer.EventAck(f.ctx, runnerwire.Event{Kind: runnerwire.EventAgentExit, RunID: &f.run.ID, Data: data}); err != nil {
		t.Fatalf("refused exit was not acknowledged: %v", err)
	}
	link := f.srv.runnerEndpoint(f.rn.ID).link
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		s.mu.Lock()
		same := s.peer == peer
		s.mu.Unlock()
		if !same || !peer.Online() || !link.Online() || s.result() != nil {
			t.Fatalf("the session ended after the refusal (same peer %v, runner online %v, link online %v): %v",
				same, peer.Online(), link.Online(), s.result())
		}
	}
	var rows int
	if err := f.pool.QueryRow(f.ctx, eventRefuseRowsSQL, "runner:"+f.rn.ID.String()).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("%d runner.event.refuse rows (%v), want one: the refused exit was handled more than once", rows, err)
	}
	if n := len(syncAuditRows(f.srv.cfg.Audit.(*syncAudit), placement.ActionRunnerDisconnect)); n != 0 {
		t.Fatalf("%d runner.disconnect rows, want none", n)
	}
}

// K6's guard (DESIGN §7): an exit for a run whose finalize cleared its ref is a receipt, not a refusal, so
// DR-0436 writes no row for it and acknowledges it as accepted.
func TestPG_TerminalRefClearedExitIsNotRefused(t *testing.T) {
	pool, cfg, rn, run, exit, _ := runnerExitAPIFixture(t)
	ctx := t.Context()
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET state='FAILED',sandbox_ref='' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	event := runnerwire.Event{Kind: runnerwire.EventAgentExit, RunID: &run.ID, Data: mustJSON(exit)}
	err := New(cfg).EventFromRunner(ctx, rn.ID, event)
	var refusal *runnerwire.EventRefusal
	if err != nil || errors.As(err, &refusal) {
		t.Fatalf("ref-cleared exit = %v, want accepted", err)
	}
	var rows, receipts int
	if err := pool.QueryRow(ctx, eventRefuseRowsSQL, "runner:"+rn.ID.String()).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("%d runner.event.refuse rows (%v), want none", rows, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runner_execution_exits WHERE run_id=$1 AND receipt_only`, run.ID).Scan(&receipts); err != nil || receipts != 1 {
		t.Fatalf("%d receipt-only rows (%v), want one", receipts, err)
	}
}
