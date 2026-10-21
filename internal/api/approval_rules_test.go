// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestApprovalRuleDraftDryRunWritesNothing (C3): the draft dry run runs each matching PENDING approval through the
// dry-run decide path and writes no row of any kind: no would-approve, no skip, no authz.denied from a gate. A row
// on an ended run is reported as run_ended without entering decide(), whose run-ended refusal cancels the run's
// approvals: that cancel would move the row and write approval.cancel through the approval store.
func TestApprovalRuleDraftDryRunWritesNothing(t *testing.T) {
	t.Setenv(envApprovalRules, approvalRulesOn)
	f := newRuleFix(t, func(f *ruleFix) { f.st.enf = map[string]bool{capEgressHost: true} })
	ended := f.run
	ended.ID, ended.State = uuid.New(), types.RunCompleted
	f.st.runs[ended.ID] = ended
	seedOn := func(runID uuid.UUID, kind types.ApprovalKind, scope string) uuid.UUID {
		ap, err := f.aps.Request(context.Background(), types.ApprovalRequest{ID: uuid.New(), RunID: runID, Kind: kind, RequestedScope: json.RawMessage(scope)})
		if err != nil {
			t.Fatal(err)
		}
		return ap.ID
	}
	seed := func(kind types.ApprovalKind, scope string) uuid.UUID { return seedOn(f.run.ID, kind, scope) }
	tool, host := seed(types.ApprovalToolCall, ruleToolScope), seed(types.ApprovalEgressDomain, ruleEgressScope)
	late := seedOn(ended.ID, types.ApprovalToolCall, ruleToolScope) // a row the run's end did not cancel
	// The approval store's own writes (ruleApprovals.CancelForRun's approval.cancel, through fsmStore.Record) land
	// in the same recorder as recordAudit's, so this count covers the store as well as the audit path.
	before := len(f.h.audit.snapshot())
	w := do(t, f.srv, http.MethodPost, "/api/v1/governance/approval-rules/dry-run", adminToken, approvalRuleBody)
	if w.Code != http.StatusOK {
		t.Fatalf("dry run = %d %s", w.Code, w.Body)
	}
	if after := f.h.audit.snapshot(); len(after) != before {
		t.Fatalf("the draft dry run wrote %d audit rows: %+v", len(after)-before, after[before:])
	}
	var rep approvalRuleReport
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	outcomes := map[uuid.UUID]string{}
	for _, row := range rep.Pending {
		outcomes[row.ApprovalID] = row.Outcome + "/" + row.Reason
	}
	if outcomes[tool] != "would_approve/" || outcomes[host] != "skip/capability_egress_host" ||
		outcomes[late] != "skip/"+reasonApprovalRunEnded || len(outcomes) != 3 {
		t.Fatalf("report outcomes = %v; want the tool call would_approve, the host skipped and the ended run's row run_ended", outcomes)
	}
	for _, id := range []uuid.UUID{tool, host, late} {
		f.pending(id)
	}
}

// TestApprovalRuleDraftDryRunRunEndingInDecide: a run that ends after the draft dry run's match, on decide()'s own
// read, answers run_ended, and the report-only decide cancels nothing: the row stays PENDING, no approval.cancel.
func TestApprovalRuleDraftDryRunRunEndingInDecide(t *testing.T) {
	t.Setenv(envApprovalRules, approvalRulesOn)
	f := newRuleFix(t, func(f *ruleFix) { f.st.endInDecide = true })
	ap, err := f.aps.Request(context.Background(), types.ApprovalRequest{ID: uuid.New(), RunID: f.run.ID, Kind: types.ApprovalToolCall, RequestedScope: json.RawMessage(ruleToolScope)})
	if err != nil {
		t.Fatal(err)
	}
	before := len(f.h.audit.snapshot()) // recordAudit and the approval store's approval.cancel both land here
	w := do(t, f.srv, http.MethodPost, "/api/v1/governance/approval-rules/dry-run", adminToken, approvalRuleBody)
	var rep approvalRuleReport
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &rep) != nil {
		t.Fatalf("dry run = %d %s", w.Code, w.Body)
	}
	if after := f.h.audit.snapshot(); len(after) != before {
		t.Fatalf("the draft dry run wrote %d audit rows: %+v", len(after)-before, after[before:])
	}
	if len(rep.Pending) != 1 || rep.Pending[0].Outcome+"/"+rep.Pending[0].Reason != "skip/"+reasonApprovalRunEnded {
		t.Fatalf("report = %+v; want the row skipped as %s", rep.Pending, reasonApprovalRunEnded)
	}
	f.pending(ap.ID)
}

// TestApprovalRuleListAnswersTheSwitch: the list route answers the rules and the switch as requests read it.
func TestApprovalRuleListAnswersTheSwitch(t *testing.T) {
	t.Setenv(envApprovalRules, approvalRulesDryRun)
	f := newRuleFix(t)
	w := do(t, f.srv, http.MethodGet, "/api/v1/governance/approval-rules", adminToken, "")
	var body struct {
		Rules  []types.ApprovalRule `json:"approval_rules"`
		Switch string               `json:"switch"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil || len(body.Rules) != 1 || body.Switch != approvalRulesDryRun {
		t.Fatalf("list = %d %s", w.Code, w.Body)
	}
}
