// Copyright 2026 The Wardyn Authors
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

	"github.com/cjohnstoniv/wardyn/internal/store"
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

// TestApprovalRuleDryRunSelectsStoredRuleOnlyByName: a body holding the name key and nothing else previews the saved
// rule; every other body is a draft and is validated as one, so an edited draft with kinds omitted or emptied is
// refused rather than answered with the saved rule's report.
func TestApprovalRuleDryRunSelectsStoredRuleOnlyByName(t *testing.T) {
	t.Setenv(envApprovalRules, approvalRulesOn)
	f := newRuleFix(t)
	const path = "/api/v1/governance/approval-rules/dry-run"
	w := do(t, f.srv, http.MethodPost, path, adminToken, `{"name":"ci-review"}`)
	var rep approvalRuleReport
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &rep) != nil || rep.Rule.Name != "ci-review" ||
		rep.Rule.Mode != types.ApprovalRuleEnforce || len(rep.Rule.Kinds) != 2 {
		t.Fatalf("a name-only body = %d %s; want the saved rule", w.Code, w.Body)
	}
	if w := do(t, f.srv, http.MethodPost, path, adminToken, `{"name":"missing"}`); w.Code != http.StatusNotFound {
		t.Fatalf("a name-only body for an absent rule = %d %s; want 404", w.Code, w.Body)
	}
	for name, body := range map[string]string{
		"edited draft, kinds omitted": `{"name":"ci-review","mode":"dry_run","match":{"repos":["narrow/repo"]}}`,
		"edited draft, kinds empty":   `{"name":"ci-review","kinds":[],"match":{"repos":["narrow/repo"]}}`,
		"name and empty kinds only":   `{"name":"ci-review","kinds":[]}`,
		"name and mode only":          `{"name":"ci-review","mode":"enforce"}`,
	} {
		w := do(t, f.srv, http.MethodPost, path, adminToken, body)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "kinds is required") {
			t.Errorf("%s = %d %s; want 400 kinds is required", name, w.Code, w.Body)
		}
	}
}

// TestApprovalRuleDryRunHonoursTheWaitBudget: a pending row at or past its expiry, which the sweep has not yet
// moved, is skipped as already decided, as Decide's conditional update would refuse it; one still ahead of it, or
// with no expiry, would be approved. The clock is the server's, which a store with no database clock stands in for.
func TestApprovalRuleDryRunHonoursTheWaitBudget(t *testing.T) {
	t.Setenv(envApprovalRules, approvalRulesOn)
	now := time.Unix(1_800_000_000, 0).UTC()
	at := func(d time.Duration) *time.Time { return new(now.Add(d)) }
	for _, c := range []struct {
		name    string
		expires *time.Time
		want    string
	}{
		{"expired a minute ago", at(-time.Minute), "skip/" + reasonApprovalAlreadyDecided},
		{"expires at this instant", at(0), "skip/" + reasonApprovalAlreadyDecided},
		{"expires a minute ahead", at(time.Minute), "would_approve/"},
		{"no expiry", nil, "would_approve/"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newRuleFix(t)
			f.srv.cfg.Now = func() time.Time { return now }
			ap, err := f.aps.Request(context.Background(), types.ApprovalRequest{ID: uuid.New(), RunID: f.run.ID, Kind: types.ApprovalToolCall,
				RequestedScope: json.RawMessage(ruleToolScope), ExpiresAt: c.expires})
			if err != nil {
				t.Fatal(err)
			}
			w := do(t, f.srv, http.MethodPost, "/api/v1/governance/approval-rules/dry-run", adminToken, approvalRuleBody)
			var rep approvalRuleReport
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &rep) != nil || len(rep.Pending) != 1 {
				t.Fatalf("dry run = %d %s", w.Code, w.Body)
			}
			if got := rep.Pending[0].Outcome + "/" + rep.Pending[0].Reason; got != c.want || rep.Pending[0].ApprovalID != ap.ID {
				t.Fatalf("outcome = %s; want %s", got, c.want)
			}
		})
	}
}

// pagedRuleStore serves the approval feed newest first from rows, and after each read a new request lands at its
// head, as one raised during the dry run would. Every other Pager read panics (nil).
type pagedRuleStore struct {
	*ruleStore
	store.Pager
	rows  []types.ApprovalRequest
	reads []store.Page
}

func (s *pagedRuleStore) ListApprovalsPage(_ context.Context, _ types.ApprovalState, p store.Page) ([]types.ApprovalRequest, error) {
	s.reads = append(s.reads, p)
	rows := s.rows
	head := rows[0]
	head.ID, head.RequestedAt = uuid.New(), head.RequestedAt.Add(time.Second)
	s.rows = append([]types.ApprovalRequest{head}, rows...)
	end := len(rows)
	if p.Limit > 0 {
		end = min(p.Offset+p.Limit, len(rows))
	}
	if p.Offset >= end {
		return []types.ApprovalRequest{}, nil
	}
	return rows[p.Offset:end], nil
}

// TestApprovalRuleDryRunReadsTheRecentFeedOnce: the 7-day look back reads the feed in one bounded read, so a request
// raised during the scan neither repeats a row nor hides one. Read by OFFSET pages, the row that landed at the head
// pushed the last row of a page onto the next page too.
func TestApprovalRuleDryRunReadsTheRecentFeedOnce(t *testing.T) {
	t.Setenv(envApprovalRules, approvalRulesOn)
	f := newRuleFix(t)
	ps := &pagedRuleStore{ruleStore: f.st}
	now := time.Now()
	for i := range 300 {
		ps.rows = append(ps.rows, types.ApprovalRequest{ID: uuid.New(), RunID: f.run.ID, Kind: types.ApprovalToolCall, State: types.ApprovalApproved,
			RequestedScope: json.RawMessage(ruleToolScope), RequestedAt: now.Add(-time.Duration(i+1) * time.Minute)})
	}
	f.srv.cfg.Store = ps
	w := do(t, f.srv, http.MethodPost, "/api/v1/governance/approval-rules/dry-run", adminToken, approvalRuleBody)
	var rep approvalRuleReport
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &rep) != nil {
		t.Fatalf("dry run = %d %s", w.Code, w.Body)
	}
	seen := map[uuid.UUID]bool{}
	for _, r := range rep.Recent {
		if seen[r.ApprovalID] {
			t.Fatalf("approval %s reported twice", r.ApprovalID)
		}
		seen[r.ApprovalID] = true
	}
	if len(rep.Recent) != 300 || rep.RecentTruncated || len(ps.reads) != 1 || ps.reads[0] != (store.Page{Limit: approvalRuleHistoryScan + 1}) {
		t.Fatalf("recent = %d rows (truncated %v) over reads %+v; want 300 in one read of %d", len(rep.Recent), rep.RecentTruncated, ps.reads, approvalRuleHistoryScan+1)
	}
}
