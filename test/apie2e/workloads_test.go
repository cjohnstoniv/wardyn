// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package apie2e

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// workloadReq is an apply body whose intent runs task under a stored policy.
func workloadReq(t *testing.T, policyID uuid.UUID, task string) client.WorkloadApplyRequest {
	t.Helper()
	var intent client.TemplateIntent
	for k, v := range map[string]any{"agent": "claude-code", "task": task, "policy_id": policyID.String()} {
		if err := intent.Set(k, v); err != nil {
			t.Fatalf("intent.Set(%s): %v", k, err)
		}
	}
	return client.WorkloadApplyRequest{Intent: intent}
}

// waitRunState waits for the run to reach want and fails the test when it does not.
func waitRunState(t *testing.T, h *harness, id uuid.UUID, want types.RunState) {
	t.Helper()
	var got types.RunState
	if !waitFor(t, 10*time.Second, func() bool {
		run, err := h.sdk.GetRun(context.Background(), id)
		got = run.State
		return err == nil && run.State == want
	}) {
		t.Fatalf("run %s state = %q, want %q", id, got, want)
	}
}

// killReasonOf waits for the run's run.kill audit row and returns the reason it carries.
func killReasonOf(t *testing.T, h *harness, id uuid.UUID) string {
	t.Helper()
	var ev types.AuditEvent
	if !waitFor(t, 10*time.Second, func() bool {
		events, err := h.sdk.AuditEvents(context.Background(), id)
		i := slices.IndexFunc(events, func(e types.AuditEvent) bool { return e.Action == "run.kill" })
		if err != nil || i < 0 {
			return false
		}
		ev = events[i]
		return true
	}) {
		t.Fatalf("run %s has no run.kill audit row", id)
	}
	var d struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		t.Fatalf("run.kill data %s: %v", ev.Data, err)
	}
	return d.Reason
}

func countRunAudit(t *testing.T, h *harness, id uuid.UUID, action string) int {
	t.Helper()
	events, err := h.sdk.AuditEvents(context.Background(), id)
	if err != nil {
		t.Fatalf("AuditEvents(%s): %v", id, err)
	}
	n := 0
	for _, e := range events {
		if e.Action == action {
			n++
		}
	}
	return n
}

// TestWorkloads_ApplyReplaceDelete drives a workload over the SDK, the calls `wardyn apply -f` and
// `wardyn delete` make: an unchanged apply writes no run, a changed one replaces the run (create, then
// end the old one with workload_replaced), and a delete stops the run and removes the workload.
func TestWorkloads_ApplyReplaceDelete(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	ctx := context.Background()

	pol, err := h.sdk.CreatePolicy(ctx, client.PolicyRequest{
		Name: "apie2e-workload-policy-" + uuid.NewString(),
		Spec: types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}, MinConfinementClass: types.CC2},
	})
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
	// A unique name keeps the shared database's other tests, which apply as the same admin owner, apart.
	name := "apie2e-wl-" + uuid.NewString()[:8]
	t.Cleanup(func() { _ = h.sdk.DeleteWorkload(context.Background(), name, "") })

	first, err := h.sdk.ApplyWorkload(ctx, name, workloadReq(t, pol.ID, "serve v1"))
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if first.Outcome != client.WorkloadCreated || first.Launch.State != client.WorkloadLaunched || first.Launch.RunID == nil {
		t.Fatalf("first apply outcome=%q launch=%+v, want created and launched with a run", first.Outcome, first.Launch)
	}
	run1 := *first.Launch.RunID

	// The same spec again is a no-op: the outcome is unchanged, no run is launched, the run stands.
	again, err := h.sdk.ApplyWorkload(ctx, name, workloadReq(t, pol.ID, "serve v1"))
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if again.Outcome != client.WorkloadUnchanged || again.Launch.State != client.WorkloadNoLaunch {
		t.Fatalf("second apply outcome=%q launch=%+v, want unchanged and none", again.Outcome, again.Launch)
	}
	if again.Workload.RunID == nil || *again.Workload.RunID != run1 {
		t.Fatalf("second apply run = %v, want %s kept", again.Workload.RunID, run1)
	}
	if n := countRunAudit(t, h, run1, "workload.launch"); n != 1 {
		t.Fatalf("workload.launch rows on the first run = %d, want 1", n)
	}
	if got, err := h.sdk.GetRun(ctx, run1); err != nil || got.State.IsTerminal() {
		t.Fatalf("first run after the unchanged apply: state=%q err=%v, want it live", got.State, err)
	}

	// A changed spec starts a new run and ends the old one as replaced.
	changed, err := h.sdk.ApplyWorkload(ctx, name, workloadReq(t, pol.ID, "serve v2"))
	if err != nil {
		t.Fatalf("changed apply: %v", err)
	}
	if changed.Outcome != client.WorkloadUpdated || changed.Launch.State != client.WorkloadLaunched || changed.Launch.RunID == nil {
		t.Fatalf("changed apply outcome=%q launch=%+v, want updated and launched with a run", changed.Outcome, changed.Launch)
	}
	run2 := *changed.Launch.RunID
	if run2 == run1 {
		t.Fatalf("changed apply kept run %s, want a new run", run1)
	}
	waitRunState(t, h, run1, types.RunKilled)
	if got := killReasonOf(t, h, run1); got != "workload_replaced" {
		t.Errorf("old run's run.kill reason = %q, want workload_replaced", got)
	}
	if n := countRunAudit(t, h, run2, "workload.replace"); n != 1 {
		t.Errorf("workload.replace rows on the new run = %d, want 1", n)
	}
	if got, err := h.sdk.GetRun(ctx, run2); err != nil || got.State.IsTerminal() {
		t.Errorf("new run: state=%q err=%v, want it live", got.State, err)
	}

	// Delete stops the current run and removes the workload.
	if err := h.sdk.DeleteWorkload(ctx, name, ""); err != nil {
		t.Fatalf("DeleteWorkload: %v", err)
	}
	waitRunState(t, h, run2, types.RunKilled)
	if got := killReasonOf(t, h, run2); got != "workload_deleted" {
		t.Errorf("deleted workload's run.kill reason = %q, want workload_deleted", got)
	}
	if n := countRunAudit(t, h, run2, "workload.delete"); n != 1 {
		t.Errorf("workload.delete rows on the run = %d, want 1", n)
	}
	_, err = h.sdk.GetWorkload(ctx, name, "")
	assertAPIStatus(t, err, http.StatusNotFound)
	list, err := h.sdk.ListWorkloads(ctx, "")
	if err != nil {
		t.Fatalf("ListWorkloads: %v", err)
	}
	if slices.ContainsFunc(list, func(w client.Workload) bool { return w.Name == name }) {
		t.Errorf("ListWorkloads still lists the deleted workload %q", name)
	}
}
