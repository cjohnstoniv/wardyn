// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
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

// cancelledRows returns the approval.cancel events recorded for runID.
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
	return a.eventsFor(runID, "approval.cancel")
}

// TestKillRun_CancelsPendingApprovalsAndIsIdempotent: one kill of a RUNNING run
// with one PENDING approval moves that approval to CANCELLED with
// decided_by=system / reason=run_killed and emits exactly one approval.cancel
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

	if len(fa.cancelledCalls()) != 1 {
		t.Fatalf("the cancel cascade moved rows %d times, want exactly 1", len(fa.cancelledCalls()))
	}
	if got := fa.cancelledCalls()[0]; got.Reason != "run_killed" || got.Count != 1 || got.RunID != runID {
		t.Errorf("cascade call = %+v, want {run:%s reason:run_killed count:1}", got, runID)
	}

	// Re-kill: the cascade re-runs (teardown/revoke are idempotent) but there is
	// nothing left PENDING, so no second row and no rewrite of the reason.
	w = do(t, srv, http.MethodPost, "/api/v1/runs/"+runID.String()+"/kill", adminToken, "")
	if w.Code == http.StatusConflict {
		t.Fatalf("re-kill of a KILLED run 409'd; it must re-run the idempotent cascade. body=%s", w.Body.String())
	}
	if len(fa.cancelledCalls()) != 1 {
		t.Errorf("a re-kill moved rows %d times in total, want still 1 — with nothing left PENDING the "+
			"cascade is a no-op, in the append-only log too", len(fa.cancelledCalls()))
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
	for time.Now().Before(deadline) && len(fa.cancelledCalls()) == 0 {
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
	if len(fa.cancelledCalls()) != 1 {
		t.Errorf("the cascade ran %d times on one clean completion, want 1", len(fa.cancelledCalls()))
	}
	if !audit.has(runID, "run.complete", "success") {
		t.Error("the shared terminal tail must still emit run.complete/success")
	}
}

// TestFailAndRevoke_FromRunningCancelsPendingApprovals is V1-r2-lensS #2. The
// exemption below is correct only BELOW RunRunning, and runs_dispatch.go breaks
// that in three places: the exec-less BYOI refusal, a failed `agent-run
// --selftest` (up to two minutes of a vendor image's own entrypoint) and a failed
// task Exec all call failAndRevoke with from=RunRunning, AFTER the
// STARTING->RUNNING CAS. By then the sandbox and the proxy sidecar are up, so an
// egress_domain approval can already be PENDING — and it was left PENDING: the
// operator's queue held a dead question for up to 24h and then expired it as
// "nobody answered" instead of "the run failed".
func TestFailAndRevoke_FromRunningCancelsPendingApprovals(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	st := &dispatchTestStore{
		run:   types.AgentRun{ID: runID, CreatedBy: "t@example.com", SandboxRef: "sbx-byoi"},
		state: types.RunRunning,
	}
	fa := newFakeApprovals()
	apID := seedPendingApproval(t, fa, runID)
	audit := &syncAudit{}
	cfg := baseTestConfig(h, st)
	cfg.Approvals = fa
	cfg.Audit = audit
	cfg.Broker = h.broker
	srv := New(cfg)

	srv.failAndRevoke(context.Background(), runID, types.RunRunning,
		"the BYOI image failed its agent-run --selftest")

	if got := st.State(); got != types.RunFailed {
		t.Fatalf("state = %q, want FAILED", got)
	}
	ap, err := fa.Get(context.Background(), apID)
	if err != nil {
		t.Fatalf("read the approval back: %v", err)
	}
	if ap.State != types.ApprovalCancelled {
		t.Errorf("a PENDING approval on a RUNNING run is %q after failAndRevoke(RunRunning), want CANCELLED", ap.State)
	}
	if ap.Reason != "run_failed" {
		t.Errorf("reason = %q, want run_failed (the transition that actually won)", ap.Reason)
	}
	// ONE cascade call, one row moved. The approval.cancel audit row itself is
	// emitted inside approval.CancelForRun (internal/approval), which this double
	// stands in for — so the call is what this package can witness, exactly as the
	// kill and completion cases above witness it.
	calls := fa.cancelledCalls()
	if len(calls) != 1 {
		t.Fatalf("the cancel cascade moved rows %d times, want exactly 1 per run transition", len(calls))
	}
	if calls[0].Reason != "run_failed" || calls[0].Count != 1 || calls[0].RunID != runID {
		t.Errorf("cascade call = %+v, want {run:%s reason:run_failed count:1}", calls[0], runID)
	}
	if !audit.has(runID, "run.fail", "success") && !audit.has(runID, "run.fail", "failure") {
		t.Log("no run.fail row recorded; the cascade assertions above are what this test owns")
	}
}

// TestFailAndRevoke_EmitsNoApprovalCancellation is the negative pin for the
// documented exemption, which survives the fix above: handed a `from` BELOW
// RunRunning, failAndRevoke is failing a run on the create/dispatch side, where
// no broker/toolgate approval can exist yet. It
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
		t.Errorf("failAndRevoke emitted %d approval.cancel rows; a run that never reached RUNNING has "+
			"no approval to cancel", len(rows))
	}
	if len(fa.cancelledCalls()) != 0 {
		t.Errorf("failAndRevoke called the cancel cascade %d times from STARTING; below RUNNING it is exempt by "+
			"construction", len(fa.cancelledCalls()))
	}
	if !audit.has(runID, "run.fail", "success") && !audit.has(runID, "run.fail", "failure") {
		t.Log("no run.fail row recorded; the exemption assertion above is what this test owns")
	}
}

// ─── V1-D1: the THIRD terminal writer, and the decide-side backstop ──────────

// TestIdleStopSeam_CancelsWithRunStopped drives CancelTerminalRunApprovals — the
// exported seam cmd/wardynd's idle reaper (lifecycleStopper.StopRun) calls after
// it wins the guarded RUNNING->STOPPED transition, the only place STOPPED is ever
// written.
//
// Before this, that writer ran the revoke half of the cascade and none of the
// approval half: terminalCancelReason's `case types.RunStopped` arm was
// unreachable dead code and docs/AUDIT-ACTIONS.md documented a reason nothing
// emitted. The cost was not cosmetic — an idle-stopped run is typically idle
// BECAUSE its agent is parked on a wait_for_review hold, so its approval stayed
// PENDING (and decidable, and replayable into the workspace allowlist by an
// `always` approve) until the 24h stale sweeper aged it out as "nobody answered".
func TestIdleStopSeam_CancelsWithRunStopped(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	// The reaper has already won the CAS by the time it calls the seam, so the
	// run row reads STOPPED — which is what the reason is derived from.
	st := &dispatchTestStore{
		run:   types.AgentRun{ID: runID, CreatedBy: "t@example.com", SandboxRef: "sbx-idle"},
		state: types.RunStopped,
	}
	fa := newFakeApprovals()
	apID := seedPendingApproval(t, fa, runID)
	audit := &syncAudit{}
	cfg := baseTestConfig(h, st)
	cfg.Approvals = fa
	cfg.Audit = audit
	srv := New(cfg)

	srv.CancelTerminalRunApprovals(context.Background(), runID)

	ap, err := fa.Get(context.Background(), apID)
	if err != nil {
		t.Fatalf("read approval back: %v", err)
	}
	if ap.State != types.ApprovalCancelled {
		t.Fatalf("approval state after an idle stop = %q, want CANCELLED", ap.State)
	}
	if ap.Reason != "run_stopped" {
		t.Errorf("approval reason = %q, want run_stopped — the trail must name the transition that "+
			"actually ended the run, not approval.expire's \"nobody answered\"", ap.Reason)
	}
	calls := fa.cancelledCalls()
	if len(calls) != 1 {
		t.Fatalf("the cascade moved rows %d times on one idle stop, want exactly 1", len(calls))
	}
	if calls[0].Reason != "run_stopped" || calls[0].Count != 1 || calls[0].RunID != runID {
		t.Errorf("cancel call = %+v, want {run:%s reason:run_stopped count:1}", calls[0], runID)
	}
}

// TestDecideOnTerminalRun_409sAndCancelsRatherThanApproving is the defence in
// depth behind that cascade. The cascade is best-effort (a failed CancelForRun is
// logged and audited, never retried) and all three terminal writers race a
// decision already in flight, so a PENDING approval on an ended run CAN reach
// decide(). Deciding it for real is the widening CANCELLED exists to stop: an
// `always` approve is replayed into the workspace allowlist
// (approvals_reconcile.go) on behalf of a sandbox that no longer exists.
func TestDecideOnTerminalRun_409sAndCancelsRatherThanApproving(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state types.RunState
		path  string
	}{
		{"approve a stopped run's approval", types.RunStopped, "approve"},
		{"approve a killed run's approval", types.RunKilled, "approve"},
		{"deny a completed run's approval", types.RunCompleted, "deny"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			runID := uuid.New()
			st := &dispatchTestStore{
				run:   types.AgentRun{ID: runID, CreatedBy: "t@example.com"},
				state: tc.state,
			}
			fa := newFakeApprovals()
			apID := seedPendingApproval(t, fa, runID)
			cfg := baseTestConfig(h, st)
			cfg.Approvals = fa
			cfg.Audit = &syncAudit{}
			srv := New(cfg)

			w := do(t, srv, http.MethodPost, "/api/v1/approvals/"+apID.String()+"/"+tc.path, adminToken, "")
			if w.Code != http.StatusConflict {
				t.Fatalf("decide on a %s run: code = %d, want 409. body=%s", tc.state, w.Code, w.Body.String())
			}
			if body := w.Body.String(); !strings.Contains(body, approvalRunEndedBody) {
				t.Errorf("409 body does not carry the sentence that says why:\n\tgot:  %s\n\twant: %s",
					body, approvalRunEndedBody)
			}
			ap, err := fa.Get(context.Background(), apID)
			if err != nil {
				t.Fatalf("read approval back: %v", err)
			}
			if ap.State != types.ApprovalCancelled {
				t.Fatalf("approval state after a refused decision = %q, want CANCELLED — the refusal must "+
					"also CLOSE the question, or the same stranded row is re-offered on every reload", ap.State)
			}
			if ap.State == types.ApprovalApproved {
				t.Fatal("the approval was APPROVED on an ended run")
			}
		})
	}
}

// TestDecideOnLiveRun_StillDecides is the negative half: the terminal guard must
// not have turned every ordinary decision into a 409.
func TestDecideOnLiveRun_StillDecides(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	st := &dispatchTestStore{
		run:   types.AgentRun{ID: runID, CreatedBy: "t@example.com"},
		state: types.RunRunning,
	}
	fa := newFakeApprovals()
	apID := seedPendingApproval(t, fa, runID)
	cfg := baseTestConfig(h, st)
	cfg.Approvals = fa
	cfg.Audit = &syncAudit{}
	srv := New(cfg)

	w := do(t, srv, http.MethodPost, "/api/v1/approvals/"+apID.String()+"/approve", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("approve on a RUNNING run: code = %d, want 200. body=%s", w.Code, w.Body.String())
	}
	ap, err := fa.Get(context.Background(), apID)
	if err != nil {
		t.Fatalf("read approval back: %v", err)
	}
	if ap.State != types.ApprovalApproved {
		t.Errorf("approval state = %q, want APPROVED", ap.State)
	}
}
