// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// B4 — killing (or otherwise ending) a run used to strand its PENDING approvals:
// the header read "1 waiting", the nav badge counted it, and the Approvals tab
// rendered live Approve/Deny buttons on a run the same screen labelled Killed,
// until the 24h stale sweeper eventually aged the row out. The terminal cascade
// now cancels them. These tests drive BOTH call sites of the one function —
// handleKillRun (which does not route through finalizeRunTail) and
// finalizeRunTail itself — plus the deliberate exemption on failAndRevoke.

// seedPendingApproval puts one PENDING approval on runID and returns its id.
func seedPendingApproval(t *testing.T, fa *fakeApprovals, runID uuid.UUID) uuid.UUID {
	t.Helper()
	ap, err := fa.Request(context.Background(), types.ApprovalRequest{
		ID: uuid.New(), RunID: runID, Kind: types.ApprovalEgressDomain,
		RequestedScope: json.RawMessage(`{"host":"api.example.com"}`),
		RequestedAt:    time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("seed approval: %v", err)
	}
	return ap.ID
}

// cancelledRows returns the approval.cancelled events recorded for runID.
//
// The row itself is written by approval.CancelForRun over the approval store's
// own recorder (the same place approval.decide/approval.expire are written), so
// these API-level drives see it only when the wired service emits it — the
// in-memory fake does not. The row's shape, its count and its once-per-batch
// rule are pinned where it is emitted (internal/approval/approval_test.go's
// TestCancelForRun_* cases); what these tests own is the WIRING: that each
// terminal writer calls the cascade, with the right reason, exactly once, and
// that the exempt one does not call it at all.
func cancelledRows(a *syncAudit, runID uuid.UUID) []types.AuditEvent {
	return a.eventsFor(runID, "approval.cancelled")
}

// TestKillRun_CancelsPendingApprovalsAndIsIdempotent: one kill of a RUNNING run
// with one PENDING approval moves that approval to CANCELLED with
// decided_by=system / reason=run_killed and emits exactly one approval.cancelled
// audit row carrying count:1 — and a re-kill of the now-KILLED run re-runs the
// cascade without moving or recording anything a second time.
func TestKillRun_CancelsPendingApprovalsAndIsIdempotent(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	st := &dispatchTestStore{
		run:   types.AgentRun{ID: runID, CreatedBy: "t@example.com", SandboxRef: "sbx-1"},
		state: types.RunRunning,
	}
	fa := newFakeApprovals()
	apID := seedPendingApproval(t, fa, runID)
	audit := &syncAudit{}
	cfg := baseTestConfig(h, st)
	cfg.Runner = &fakeRunner{}
	cfg.Broker = h.broker
	cfg.Approvals = fa
	cfg.Audit = audit
	srv := New(cfg)

	w := do(t, srv, http.MethodPost, "/api/v1/runs/"+runID.String()+"/kill", adminToken, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("kill: code = %d, want 202. body=%s", w.Code, w.Body.String())
	}

	ap, err := fa.Get(context.Background(), apID)
	if err != nil {
		t.Fatalf("read approval back: %v", err)
	}
	if ap.State != types.ApprovalCancelled {
		t.Fatalf("approval state after kill = %q, want CANCELLED — the Approvals tab otherwise keeps "+
			"rendering live Approve/Deny buttons on a run labelled Killed", ap.State)
	}
	if ap.DecidedBy != "system" || ap.Reason != "run_killed" {
		t.Errorf("approval decided_by/reason = %q/%q, want system/run_killed", ap.DecidedBy, ap.Reason)
	}

	if len(fa.cancelled) != 1 {
		t.Fatalf("the cancel cascade moved rows %d times, want exactly 1", len(fa.cancelled))
	}
	if got := fa.cancelled[0]; got.Reason != "run_killed" || got.Count != 1 || got.RunID != runID {
		t.Errorf("cascade call = %+v, want {run:%s reason:run_killed count:1}", got, runID)
	}

	// Re-kill: the cascade re-runs (teardown/revoke are idempotent) but there is
	// nothing left PENDING, so no second row and no rewrite of the reason.
	w = do(t, srv, http.MethodPost, "/api/v1/runs/"+runID.String()+"/kill", adminToken, "")
	if w.Code == http.StatusConflict {
		t.Fatalf("re-kill of a KILLED run 409'd; it must re-run the idempotent cascade. body=%s", w.Body.String())
	}
	if len(fa.cancelled) != 1 {
		t.Errorf("a re-kill moved rows %d times in total, want still 1 — with nothing left PENDING the "+
			"cascade is a no-op, in the append-only log too", len(fa.cancelled))
	}
	if again, _ := fa.Get(context.Background(), apID); again.Reason != "run_killed" {
		t.Errorf("a re-kill rewrote the cancelled row's reason to %q", again.Reason)
	}
}

// TestFinalizeRunTail_CancelsPendingApprovalsOnCompletion is the SECOND call
// site: a run that ends on its own (the completion watcher, exit 0) reaches
// finalizeRunTail, never handleKillRun. §8b.3's owner default is ALL terminal
// transitions, not kill only — the reason names the transition that actually
// won, read back off the run row.
func TestFinalizeRunTail_CancelsPendingApprovalsOnCompletion(t *testing.T) {
	runID := uuid.New()
	st := &dispatchTestStore{
		run:   types.AgentRun{ID: runID, CreatedBy: "t@example.com", SandboxRef: "ref-c"},
		state: types.RunRunning,
	}
	fa := newFakeApprovals()
	apID := seedPendingApproval(t, fa, runID)
	audit := &syncAudit{}
	rn := &finalizeTailRunner{fakeRunner: &fakeRunner{}}
	srv := newFinalizeTailServer(t, st, &raceBroker{}, rn, audit)
	srv.cfg.Approvals = fa

	srv.startCompletionWatcher(runID, "ref-c", "exec-c")

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && len(fa.cancelled) == 0 {
		time.Sleep(10 * time.Millisecond)
	}

	if got := st.State(); got != types.RunCompleted {
		t.Fatalf("watcher: state = %q, want COMPLETED", got)
	}
	ap, err := fa.Get(context.Background(), apID)
	if err != nil {
		t.Fatalf("read approval back: %v", err)
	}
	if ap.State != types.ApprovalCancelled {
		t.Fatalf("a COMPLETED run left its approval %q; every terminal transition cancels, not only kill", ap.State)
	}
	if ap.Reason != "run_completed" {
		t.Errorf("reason = %q, want run_completed (the transition that actually won)", ap.Reason)
	}
	if len(fa.cancelled) != 1 {
		t.Errorf("the cascade ran %d times on one clean completion, want 1", len(fa.cancelled))
	}
	if !audit.has(runID, "run.complete", "success") {
		t.Error("the shared terminal tail must still emit run.complete/success")
	}
}

// TestFailAndRevoke_EmitsNoApprovalCancellation is the negative pin for the
// documented exemption: failAndRevoke fails a run BEFORE it reached RUNNING (the
// create/dispatch paths), where no broker/toolgate approval can exist yet. It
// must not call the cancel cascade at all — a list-all-PENDING read per failed
// dispatch that can only ever find nothing, and an audit row that would claim a
// cancellation that never happened.
func TestFailAndRevoke_EmitsNoApprovalCancellation(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	st := &dispatchTestStore{
		run:   types.AgentRun{ID: runID, CreatedBy: "t@example.com"},
		state: types.RunStarting,
	}
	fa := newFakeApprovals()
	audit := &syncAudit{}
	cfg := baseTestConfig(h, st)
	cfg.Approvals = fa
	cfg.Audit = audit
	cfg.Broker = h.broker
	srv := New(cfg)

	srv.failAndRevoke(context.Background(), runID, types.RunStarting, "image pull failed")

	if got := st.State(); got != types.RunFailed {
		t.Fatalf("state = %q, want FAILED", got)
	}
	if rows := cancelledRows(audit, runID); len(rows) != 0 {
		t.Errorf("failAndRevoke emitted %d approval.cancelled rows; a run that never reached RUNNING has "+
			"no approval to cancel", len(rows))
	}
	if len(fa.cancelled) != 0 {
		t.Errorf("failAndRevoke called the cancel cascade %d times; it is exempt by construction", len(fa.cancelled))
	}
	if !audit.has(runID, "run.fail", "success") && !audit.has(runID, "run.fail", "failure") {
		t.Log("no run.fail row recorded; the exemption assertion above is what this test owns")
	}
}
