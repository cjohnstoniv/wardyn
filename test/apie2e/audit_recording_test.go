// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package apie2e

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestAudit_RunActionsAreQueryableInSeqOrder proves control-plane actions emit
// append-only audit events that are queryable through the public SDK
// (GET /api/v1/audit?run_id=) in append (seq) order. We drive a run through its
// full lifecycle with the fake runner so the server emits a sequence of
// run-scoped events (run.exec -> run.complete -> ...), then assert:
//
//   - the run's audit trail contains the expected control-plane actions;
//   - events come back in non-decreasing seq order (the append-only contract);
//   - the events are bound to OUR run id (isolation in the shared DB).
func TestAudit_RunActionsAreQueryableInSeqOrder(t *testing.T) {
	fr := newFakeRunner()
	fr.waitExit = 0
	neverReap := types.RunPolicySpec{
		AllowedDomains:      []string{"api.anthropic.com"},
		MinConfinementClass: types.CC2,
		AutoStopAfterSec:    -1,
	}
	h := newHarness(t, harnessOpts{withRunner: fr, defaultPolicy: &neverReap})
	ctx := context.Background()

	run, err := h.sdk.CreateRun(ctx, client.CreateRunRequest{
		Agent: "claude-code",
		Repo:  "acme/widgets",
		Task:  "audit me " + uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// Wait for RUNNING, then complete the run so the watcher emits run.complete.
	if !waitFor(t, 5*time.Second, func() bool {
		got, gerr := h.sdk.GetRun(ctx, run.ID)
		return gerr == nil && got.State == types.RunRunning
	}) {
		t.Fatalf("run did not reach RUNNING")
	}
	fr.releaseWait()
	if !waitFor(t, 5*time.Second, func() bool {
		got, gerr := h.sdk.GetRun(ctx, run.ID)
		return gerr == nil && got.State == types.RunCompleted
	}) {
		t.Fatalf("run did not reach COMPLETED")
	}

	// The run.complete event is the last to land; poll until it appears.
	var events []types.AuditEvent
	if !waitFor(t, 5*time.Second, func() bool {
		ev, qerr := h.sdk.AuditEvents(ctx, run.ID)
		if qerr != nil {
			return false
		}
		events = ev
		return slices.ContainsFunc(ev, func(e types.AuditEvent) bool { return e.Action == "run.complete" })
	}) {
		t.Fatalf("run.complete audit event never surfaced via the SDK; got actions=%v", actionsOf(events))
	}

	// Every event must be bound to our run id (no cross-run bleed).
	for _, ev := range events {
		if ev.RunID == nil || *ev.RunID != run.ID {
			t.Fatalf("audit event %q not bound to run %s", ev.Action, run.ID)
		}
	}

	// Expected control-plane actions for a runner-backed completion.
	for _, want := range []string{"run.exec", "run.complete"} {
		if !slices.ContainsFunc(events, func(e types.AuditEvent) bool { return e.Action == want }) {
			t.Errorf("missing audit action %q; got %v", want, actionsOf(events))
		}
	}

	// Append (seq) order: the server returns the run's trail ORDER BY seq ASC.
	// We prove the append ordering two ways:
	//   1. timestamps are non-decreasing (events are appended over time);
	//   2. run.exec (emitted first, at agent launch) precedes run.complete
	//      (emitted later, at terminal transition) by position in the returned
	//      slice — the externally-observable consequence of the seq-ASC order.
	for i := 1; i < len(events); i++ {
		if events[i].Time.Before(events[i-1].Time) {
			t.Fatalf("audit events out of time order at %d: %s before %s",
				i, events[i].Time, events[i-1].Time)
		}
	}
	execIdx := slices.IndexFunc(events, func(e types.AuditEvent) bool { return e.Action == "run.exec" })
	doneIdx := slices.IndexFunc(events, func(e types.AuditEvent) bool { return e.Action == "run.complete" })
	if execIdx < 0 || doneIdx < 0 || execIdx >= doneIdx {
		t.Fatalf("expected run.exec (idx %d) to precede run.complete (idx %d) in seq order; actions=%v",
			execIdx, doneIdx, actionsOf(events))
	}

	_, _ = h.pool.Exec(ctx, `DELETE FROM agent_runs WHERE id=$1`, run.ID)
}

// TestAudit_ApprovalDecisionRecorded asserts a human approval decision made over
// the SDK lands in the run's audit trail (approval.decide) — the provable record
// that human Y decided approval X. End-to-end through the real approval FSM.
func TestAudit_ApprovalDecisionRecorded(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	ctx := context.Background()

	runID := uuid.New()
	seedRun(t, h, runID)
	scopeJSON := []byte(`{"repos":["acme/widgets"]}`)
	grantID := seedGrant(t, h, runID, types.GrantSpec{
		Kind: types.GrantGitHubToken, Scope: scopeJSON, RequiresApproval: true, TTLSeconds: 600,
	})

	_, err := h.broker.MintForGrant(ctx, h.callerClaims(runID), grantID)
	if err == nil {
		t.Fatalf("first mint: expected ErrApprovalPending, got nil")
	}
	pendings, lerr := h.sdk.ListApprovals(ctx, types.ApprovalPending)
	if lerr != nil {
		t.Fatalf("ListApprovals: %v", lerr)
	}
	ap := findApprovalForRun(pendings, runID)
	if ap == nil {
		t.Fatalf("no pending approval for run %s", runID)
	}
	if _, aerr := h.sdk.Approve(ctx, ap.ID, "ok"); aerr != nil {
		t.Fatalf("Approve: %v", aerr)
	}

	if !waitFor(t, 5*time.Second, func() bool {
		ev, qerr := h.sdk.AuditEvents(ctx, runID)
		return qerr == nil && slices.ContainsFunc(ev, func(e types.AuditEvent) bool { return e.Action == "approval.decide" })
	}) {
		ev, _ := h.sdk.AuditEvents(ctx, runID)
		t.Fatalf("approval.decide not in audit trail; got %v", actionsOf(ev))
	}

	_, _ = h.pool.Exec(ctx, `DELETE FROM agent_runs WHERE id=$1`, runID)
}

// TestRecording_UploadThenServe is the recording happy path through the real
// RecordingStore (an FSStore on a per-test temp dir):
//
//  1. the run-token-authed proxy PUTs an asciicast to the internal upload route;
//  2. the admin GETs it back through the public replay route and the bytes match.
//
// This proves the recording surfaces are wired end-to-end over the live server.
func TestRecording_UploadThenServe(t *testing.T) {
	recStore, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("new fs store: %v", err)
	}
	h := newHarness(t, harnessOpts{recordingStore: recStore})
	ctx := context.Background()

	runID := uuid.New()
	seedRun(t, h, runID)

	// A minimal asciicast v2 body (header line + one event). The masking copy is
	// a no-op here (no registered secrets), so the bytes round-trip verbatim.
	cast := []byte("{\"version\":2,\"width\":80,\"height\":24}\n[0.1,\"o\",\"hello apie2e\\r\\n\"]\n")

	tok := h.mintRunToken(runID)
	upStatus := h.putRaw(t, "/api/v1/internal/recordings/"+runID.String(), tok, cast)
	if upStatus != http.StatusNoContent {
		t.Fatalf("upload status = %d, want 204", upStatus)
	}

	// Serve it back through the admin-gated replay route.
	resp := h.getJSON(t, "/api/v1/runs/"+runID.String()+"/recording/"+runID.String(), adminToken)
	if resp.status != http.StatusOK {
		t.Fatalf("serve status = %d, body=%s", resp.status, resp.body)
	}
	if !bytes.Equal([]byte(resp.body), cast) {
		t.Fatalf("served cast != uploaded cast\n got: %q\nwant: %q", resp.body, cast)
	}

	_, _ = h.pool.Exec(ctx, `DELETE FROM agent_runs WHERE id=$1`, runID)
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// actionsOf collects the actions for diagnostic failure messages.
func actionsOf(events []types.AuditEvent) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.Action)
	}
	return out
}

// putRaw PUTs a raw body with a bearer token and returns the status code.
func (h *harness) putRaw(t *testing.T, path, bearer string, body []byte) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, h.srv.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", path, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}
