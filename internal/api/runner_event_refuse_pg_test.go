// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
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

// P2 (DR-0436): a spool record the organisation refuses for good in its place moves the run's cursor past it, so the
// next record is no gap, and the organisation's spool receipt is the one the runner's spool seals against, the
// refused record's hash included.
func TestPG_RefusedSpoolRecordMovesCursorAndSealMatches(t *testing.T) {
	pool, cfg, rn, run, _, _ := runnerExitAPIFixture(t)
	ctx := t.Context()
	srv := New(cfg)
	spool, err := runnerspool.Open(filepath.Join(t.TempDir(), "spool"), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	for _, d := range []egress.Decision{egress.Allow, "bogus", egress.Allow} {
		if err := spool.Append(ctx, egress.DecisionLog{Request: egress.Request{RunID: run.ID, Host: "allowed.test", Time: time.Now().UTC().Truncate(time.Microsecond)},
			Decision: d, RuleSource: "policy:allowed"}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := spool.After(ctx, 0, 3)
	if err != nil || len(rows) != 3 {
		t.Fatalf("spool rows %d %v, want 3", len(rows), err)
	}
	// receipt is the organisation's spool receipt as retirementReceiptTx reads it from the run's cursor.
	receipt := func() types.RunnerSpoolReceipt {
		t.Helper()
		var r types.RunnerSpoolReceipt
		var overflow string
		if err := pool.QueryRow(ctx, `SELECT spool_id, last_seq, COALESCE(recent_hashes->>(last_seq::text), ''), overflow_end::text
			FROM runner_spool_cursors WHERE run_id=$1 AND runner_id=$2`, run.ID, rn.ID).Scan(&r.SpoolID, &r.Seq, &r.Hash, &overflow); err != nil {
			t.Fatal(err)
		}
		drops, err := strconv.ParseUint(overflow, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		r.ReportedDrops = drops
		return r
	}
	send := func(i int) error {
		t.Helper()
		data, err := rows[i].Marshal()
		if err != nil {
			t.Fatal(err)
		}
		return srv.eventFromRunner(ctx, rn.ID, "", runnerwire.Event{Kind: runnerwire.EventSpool, RunID: &run.ID, Data: data})
	}
	// ack is the runner's side of an acknowledged record: it commits its cursor there (runnerspool Set).
	ack := func(i int) {
		t.Helper()
		if err := spool.Set(ctx, rows[i].Position()); err != nil {
			t.Fatal(err)
		}
	}
	if err := send(0); err != nil {
		t.Fatalf("seq 1 = %v, want accepted", err)
	}
	ack(0)
	var refusal *runnerwire.EventRefusal
	if err := send(1); !errors.As(err, &refusal) || refusal.Code != runnerwire.RefusalStoreRefused {
		t.Fatalf("seq 2 = %v, want refused for good (%s)", err, runnerwire.RefusalStoreRefused)
	}
	ack(1)
	if org := receipt(); org.Seq != 2 || org.Hash != rows[1].Hash {
		t.Fatalf("cursor %+v after the refused record, want seq 2 at its hash %s", org, rows[1].Hash)
	}
	if held, err := spool.Receipt(ctx); err != nil || held != receipt() {
		t.Fatalf("runner's spool receipt %+v (%v), organisation's %+v", held, err, receipt())
	}
	if err := send(2); err != nil {
		t.Fatalf("seq 3 after the refused record = %v, want accepted", err)
	}
	ack(2)
	org := receipt()
	if org.Seq != 3 || org.Hash != rows[2].Hash {
		t.Fatalf("cursor %+v, want seq 3 at %s", org, rows[2].Hash)
	}
	if sealed, err := spool.Seal(ctx, org); err != nil || !sealed {
		t.Fatalf("runner's spool against the organisation's receipt %+v: sealed=%v err=%v", org, sealed, err)
	}
}

// An overflow summary the organisation refuses for good moves the run's overflow mark as the runner's moves, so the two
// spool receipts still match and the spool seals, and later summaries and records are accepted. An event whose place
// cannot be read moves nothing: the record after it is still next.
func TestPG_RefusedOverflowSummaryMovesMarkAndSealMatches(t *testing.T) {
	pool, cfg, rn, run, _, _ := runnerExitAPIFixture(t)
	ctx := t.Context()
	srv := New(cfg)
	spool, err := runnerspool.Open(filepath.Join(t.TempDir(), "spool"), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	receipt := func() types.RunnerSpoolReceipt {
		t.Helper()
		var r types.RunnerSpoolReceipt
		var overflow string
		if err := pool.QueryRow(ctx, `SELECT spool_id, last_seq, COALESCE(recent_hashes->>(last_seq::text), ''), overflow_end::text
			FROM runner_spool_cursors WHERE run_id=$1 AND runner_id=$2`, run.ID, rn.ID).Scan(&r.SpoolID, &r.Seq, &r.Hash, &overflow); err != nil {
			t.Fatal(err)
		}
		drops, err := strconv.ParseUint(overflow, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		r.ReportedDrops = drops
		return r
	}
	event := func(data []byte) runnerwire.Event {
		return runnerwire.Event{Kind: runnerwire.EventSpool, RunID: &run.ID, Data: data}
	}
	// refuseSummary sends the spool's next summary with a field this organisation does not know, as a later release
	// could write it: refused for its shape, and acknowledged, so the runner moves its own mark.
	refuseSummary := func() runnerspool.Record {
		t.Helper()
		if err := spool.Refuse(ctx); err != nil {
			t.Fatal(err)
		}
		row, err := spool.Overflow(ctx)
		if err != nil || row == nil {
			t.Fatalf("overflow %+v %v", row, err)
		}
		data, err := row.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		var refusal *runnerwire.EventRefusal
		if err := srv.eventFromRunner(ctx, rn.ID, "", event(append(data[:len(data)-1:len(data)-1], []byte(`,"later":1}`)...))); !errors.As(err, &refusal) || refusal.Code != runnerwire.RefusalShape {
			t.Fatalf("summary = %v, want refused for good (%s)", err, runnerwire.RefusalShape)
		}
		if err := spool.AckOverflow(ctx, *row); err != nil {
			t.Fatal(err)
		}
		return *row
	}
	first := refuseSummary()
	if org := receipt(); org.ReportedDrops != first.DropEnd {
		t.Fatalf("mark %d after the refused summary, want %d", org.ReportedDrops, first.DropEnd)
	}
	if held, err := spool.Receipt(ctx); err != nil || held != receipt() {
		t.Fatalf("runner's spool receipt %+v (%v), organisation's %+v", held, err, receipt())
	}
	second := refuseSummary()
	if org := receipt(); org.ReportedDrops != second.DropEnd {
		t.Fatalf("mark %d after the second refused summary, want %d", org.ReportedDrops, second.DropEnd)
	}
	// A summary the organisation accepts follows on from the moved mark.
	if err := spool.Refuse(ctx); err != nil {
		t.Fatal(err)
	}
	row, err := spool.Overflow(ctx)
	if err != nil || row == nil {
		t.Fatalf("overflow %+v %v", row, err)
	}
	data, err := row.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.eventFromRunner(ctx, rn.ID, "", event(data)); err != nil {
		t.Fatalf("summary after the refused ones = %v, want accepted", err)
	}
	if err := spool.AckOverflow(ctx, *row); err != nil {
		t.Fatal(err)
	}
	// An event with no readable place: acknowledged, and the cursor stays where it was.
	before := receipt()
	var refusal *runnerwire.EventRefusal
	if err := srv.eventFromRunner(ctx, rn.ID, "", event([]byte(`{`))); !errors.As(err, &refusal) {
		t.Fatalf("unreadable event = %v, want refused for good", err)
	}
	if org := receipt(); org != before {
		t.Fatalf("an unreadable event moved the cursor %+v to %+v", before, org)
	}
	if err := spool.Append(ctx, egress.DecisionLog{Request: egress.Request{RunID: run.ID, Host: "allowed.test", Time: time.Now().UTC().Truncate(time.Microsecond)},
		Decision: egress.Allow, RuleSource: "policy:allowed"}); err != nil {
		t.Fatal(err)
	}
	rows, err := spool.After(ctx, 0, 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("spool rows %d %v, want 1", len(rows), err)
	}
	if data, err = rows[0].Marshal(); err != nil {
		t.Fatal(err)
	}
	if err := srv.eventFromRunner(ctx, rn.ID, "", event(data)); err != nil {
		t.Fatalf("seq 1 after the unreadable event = %v, want accepted", err)
	}
	if err := spool.Set(ctx, rows[0].Position()); err != nil {
		t.Fatal(err)
	}
	org := receipt()
	if sealed, err := spool.Seal(ctx, org); err != nil || !sealed {
		t.Fatalf("runner's spool against the organisation's receipt %+v: sealed=%v err=%v", org, sealed, err)
	}
}
