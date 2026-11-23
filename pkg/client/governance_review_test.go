// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// seen is the request a fake answered: its method, path and body.
type seen struct {
	method, path, body string
}

// reviewFake answers every request with status and body, and records the request.
func reviewFake(t *testing.T, status int, body string, got *seen) *client.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		*got = seen{r.Method, r.URL.Path, string(raw)}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return newTestClient(srv)
}

// approvalsSample is a pending change's approvals block as the server writes it for a change that needs
// two approvals, one of them a security admin's.
const approvalsSample = `{
  "required": 2, "require_security": true, "have": 1,
  "entries": [
    {"approver": "pat", "tier": "security_admin", "reason": "checked the diff", "approved_at": "2026-11-20T10:00:00Z", "counts": true},
    {"approver": "lee", "tier": "unit_admin", "unit_name": "Payments", "approved_at": "2026-11-20T09:00:00Z", "counts": false,
     "dropped": "session_revoked", "dropped_at": "2026-11-20T09:30:00Z"}
  ],
  "eligible": [{"principal": "kai", "email": "kai@example.com", "tier": "security_admin"}, {"principal": "sam", "tier": "unit_admin", "unit_name": "Payments"}],
  "uncountable": [{"why": "unconfirmed_people", "count": 4}, {"why": "too_many_people"}],
  "reach": "cant_reach", "cant_reach_reason": "security", "still_needed": 1, "remaining_eligible": 3,
  "proposer_active": false, "reject_reason_required": true,
  "viewer": {"state": "cannot_be_last", "last": true, "approved_at": "2026-11-20T11:00:00Z"}
}`

// countsSample is the approval counts as the server writes them, one kind with a row of its own.
const countsSample = `{
  "minimum": 2, "highest": 3,
  "default": {"required": 2, "require_security": false},
  "kinds": [
    {"kind": "governance_unit", "own": true, "required": 3, "require_security": true, "approvers": "security_or_unit_admin",
     "pool": {"status": "cant_confirm", "count": 3, "unit_name": "Data Science", "why": "unconfirmed_people"}},
    {"kind": "role_mapping", "own": false, "required": 2, "require_security": false, "approvers": "super_admin",
     "pool": {"status": "ok", "count": 5}}
  ]
}`

// sameJSON reports whether a and b are the same JSON value.
func sameJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("decode %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return reflect.DeepEqual(x, y)
}

func TestGovernanceApprovalsKeepEveryKey(t *testing.T) {
	var a client.GovernanceChangeApprovals
	if err := json.Unmarshal([]byte(approvalsSample), &a); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if a.Have != 1 || a.Required != 2 || !a.RequireSecurity || a.Entries[1].Dropped != "session_revoked" || a.Entries[1].Counts {
		t.Errorf("decoded %+v", a)
	}
	if a.ProposerActive == nil || *a.ProposerActive {
		t.Errorf("proposer_active false was not kept: %v", a.ProposerActive)
	}
	out, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if !sameJSON(t, out, []byte(approvalsSample)) {
		t.Errorf("approvals changed through decode and encode:\n got %s\nwant %s", out, approvalsSample)
	}
}

// A zero the server sent on the read of one change is an answer, not an absence: it must come back out.
func TestGovernanceApprovalsKeepZeroes(t *testing.T) {
	const zeroes = `{"required":2,"require_security":false,"have":2,"entries":[],"eligible":[],"uncountable":[],` +
		`"reach":"ok","still_needed":0,"remaining_eligible":0,"proposer_active":false,"reject_reason_required":false}`
	var a client.GovernanceChangeApprovals
	if err := json.Unmarshal([]byte(zeroes), &a); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if a.StillNeeded == nil || a.RemainingEligible == nil {
		t.Fatalf("zero counts were read as absent: %+v", a)
	}
	out, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"still_needed", "remaining_eligible", "proposer_active", "reject_reason_required"} {
		if !strings.Contains(string(out), `"`+key+`":`) {
			t.Errorf("%s was dropped from %s", key, out)
		}
	}
	var row client.GovernanceChangeApprovals
	if err := json.Unmarshal([]byte(`{"required":2,"require_security":false,"have":1}`), &row); err != nil {
		t.Fatal(err)
	}
	out, _ = json.Marshal(row)
	if strings.Contains(string(out), "still_needed") || strings.Contains(string(out), "remaining_eligible") {
		t.Errorf("a list row must stay without the one-change fields: %s", out)
	}
}

func TestGetGovernanceChangeImpact_ZeroLiveRunsStay(t *testing.T) {
	var got seen
	c := reviewFake(t, http.StatusOK, `{"computed":"tree","as_of":"2026-11-20T10:00:00Z","people":[],"live_runs_total":0}`, &got)
	out, err := c.GetGovernanceChangeImpact(context.Background(), uuid.New())
	if err != nil || out.LiveRunsTotal == nil {
		t.Fatalf("live_runs_total 0 was read as absent: %+v, %v", out, err)
	}
	if b, _ := json.Marshal(out); !strings.Contains(string(b), `"live_runs_total":0`) {
		t.Errorf("live_runs_total was dropped from %s", b)
	}
}

func TestApprovalCountsKeepEveryKey(t *testing.T) {
	var c client.ApprovalCounts
	if err := json.Unmarshal([]byte(countsSample), &c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(c.Kinds) != 2 || c.Kinds[0].Pool.Why != "unconfirmed_people" || c.Highest != 3 {
		t.Errorf("decoded %+v", c)
	}
	out, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if !sameJSON(t, out, []byte(countsSample)) {
		t.Errorf("counts changed through decode and encode:\n got %s\nwant %s", out, countsSample)
	}
}

func TestChangeCarriesApprovalsAndReview(t *testing.T) {
	id := uuid.New()
	body := `{"id":"` + id.String() + `","state":"pending","diff":{"changed":["name"],` +
		`"review":{"verdict":"widens","widens":[{"field":"allowed_domains","entries":["a.example"]}],"held_because":["contact"],` +
		`"risk":{"before":null,"after":{"overall":"medium","items":[{"field":"egress","value":"a.example","risk_level":"medium","rationale":"new host","invariant_ref":"I-1"}]}}}},` +
		`"approvals":` + approvalsSample + `}`
	var got seen
	c := reviewFake(t, http.StatusOK, body, &got)
	ch, err := c.GetGovernanceChange(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	r := ch.Diff.Review
	if r == nil || r.Verdict != "widens" || r.Widens[0].Entries[0] != "a.example" || r.Risk == nil || r.Risk.Before != nil ||
		r.Risk.After.Items[0].InvariantRef != "I-1" {
		t.Errorf("review = %+v", r)
	}
	if ch.Approvals == nil || ch.Approvals.Have != 1 {
		t.Errorf("approvals = %+v", ch.Approvals)
	}
	var plain client.GovernanceChange
	if err := json.Unmarshal([]byte(`{"id":"`+id.String()+`","state":"pending","diff":{"changed":[]}}`), &plain); err != nil || plain.Approvals != nil || plain.Diff.Review != nil {
		t.Errorf("a change with neither key must read as one person deciding: %+v, %v", plain, err)
	}
}

func TestGetGovernanceChangeImpact(t *testing.T) {
	id, run, mem := uuid.New(), uuid.New(), uuid.New()
	body := `{"computed":"tree","as_of":"2026-11-20T10:00:00Z","people":[{"membership_id":"` + mem.String() + `","subject_type":"user","subject":"bob",` +
		`"from_unit_id":"` + uuid.Nil.String() + `","from_unit_name":"Default","unit_id":"` + uuid.Nil.String() + `","unit_name":"Data","changes":[{"knob":"cpu","before":1,"after":2}]}],` +
		`"live_runs":[{"id":"` + run.String() + `","owner":"bob","started_at":"2026-11-20T09:00:00Z"}],"live_runs_total":3,` +
		`"activates":[{"unit_id":"` + uuid.Nil.String() + `","unit_name":"Data"}]}`
	var got seen
	c := reviewFake(t, http.StatusOK, body, &got)
	out, err := c.GetGovernanceChangeImpact(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.method != http.MethodGet || got.path != "/api/v1/governance/changes/"+id.String()+"/impact" || got.body != "" {
		t.Errorf("request = %+v", got)
	}
	if out.Computed != "tree" || len(out.People) != 1 || out.People[0].Subject != "bob" || out.People[0].Changes[0].Knob != "cpu" ||
		out.LiveRuns[0].ID != run || out.LiveRunsTotal == nil || *out.LiveRunsTotal != 3 || out.Activates[0].UnitName != "Data" || out.AsOf.IsZero() {
		t.Errorf("impact = %+v", out)
	}
}

func TestGetGovernanceChangeImpact_Boundary(t *testing.T) {
	body := `{"computed":"boundary","as_of":"2026-11-20T10:00:00Z","boundary":{"target":"boundary","deployment":[{"knob":"k","before":1,"after":0}],` +
		`"units":[],"stored_policies":[],"memberships":[],"provider_rows":[{"id":"gh","ceiling_removed":["push"]}]}}`
	var got seen
	c := reviewFake(t, http.StatusOK, body, &got)
	out, err := c.GetGovernanceChangeImpact(context.Background(), uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if out.Boundary == nil || len(out.Boundary.Deployment) != 1 || out.Boundary.ProviderRows[0].CeilingRemoved[0] != "push" {
		t.Errorf("boundary = %+v", out.Boundary)
	}
}

func TestApproveGovernanceChangeWithReason(t *testing.T) {
	id := uuid.New()
	var got seen
	c := reviewFake(t, http.StatusOK, `{"id":"`+id.String()+`","state":"pending","approvals":{"required":2,"require_security":false,"have":1}}`, &got)
	ch, err := c.ApproveGovernanceChangeWithReason(context.Background(), id, "looks right")
	if err != nil {
		t.Fatal(err)
	}
	if got.method != http.MethodPost || got.path != "/api/v1/governance/changes/"+id.String()+"/approve" || got.body != `{"reason":"looks right"}` {
		t.Errorf("request = %+v", got)
	}
	if ch.State != "pending" || ch.Approvals == nil || ch.Approvals.Have != 1 {
		t.Errorf("change = %+v", ch)
	}
	if _, err := c.ApproveGovernanceChangeWithReason(context.Background(), id, ""); err != nil || got.body != "{}" {
		t.Errorf("an empty reason sends no reason: body %q, %v", got.body, err)
	}
}

func TestTakeBackGovernanceApproval(t *testing.T) {
	id := uuid.New()
	var got seen
	c := reviewFake(t, http.StatusOK, `{"id":"`+id.String()+`","state":"pending"}`, &got)
	ch, err := c.TakeBackGovernanceApproval(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.method != http.MethodDelete || got.path != "/api/v1/governance/changes/"+id.String()+"/approval" || got.body != "" || ch.ID != id {
		t.Errorf("request = %+v, change = %+v", got, ch)
	}
	c = reviewFake(t, http.StatusNotFound, `{"error":"No approval of yours to take back.","reason":"governance_change_approval_not_found"}`, &got)
	var apiErr *client.APIError
	if _, err := c.TakeBackGovernanceApproval(context.Background(), id); !errors.As(err, &apiErr) || apiErr.Reason != "governance_change_approval_not_found" {
		t.Errorf("err = %v, want the 404 reason", err)
	}
}

func TestGetApprovalCounts(t *testing.T) {
	var got seen
	c := reviewFake(t, http.StatusOK, countsSample, &got)
	out, err := c.GetApprovalCounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.method != http.MethodGet || got.path != "/api/v1/governance/approval-counts" || got.body != "" || out.Minimum != 2 {
		t.Errorf("request = %+v, counts = %+v", got, out)
	}
}

func TestPutApprovalCounts(t *testing.T) {
	w := client.ApprovalCountsWrite{
		Default: client.ApprovalCountsRule{Required: 2},
		Kinds:   []client.ApprovalCountsWriteKind{{Kind: "governance_unit", Required: 3, RequireSecurity: true}},
	}
	var got seen
	c := reviewFake(t, http.StatusOK, countsSample, &got)
	out, err := c.PutApprovalCounts(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"default":{"required":2,"require_security":false},"kinds":[{"kind":"governance_unit","required":3,"require_security":true}]}`
	if got.method != http.MethodPut || got.path != "/api/v1/governance/approval-counts" || got.body != want || out.Highest != 3 {
		t.Errorf("request = %+v, counts = %+v", got, out)
	}
}

// The write replaces every row, so a caller with no kinds sends an empty list and never null.
func TestPutApprovalCounts_NoKindsIsAnEmptyList(t *testing.T) {
	var got seen
	c := reviewFake(t, http.StatusOK, countsSample, &got)
	if _, err := c.PutApprovalCounts(context.Background(), client.ApprovalCountsWrite{Default: client.ApprovalCountsRule{Required: 2}}); err != nil {
		t.Fatal(err)
	}
	if want := `{"default":{"required":2,"require_security":false},"kinds":[]}`; got.body != want {
		t.Errorf("body = %s, want %s", got.body, want)
	}
}

func TestPutApprovalCounts_HeldIsPendingApprovalError(t *testing.T) {
	id := uuid.New()
	var got seen
	c := reviewFake(t, http.StatusAccepted, `{"pending_change":{"id":"`+id.String()+`","target_kind":"governance_approval_counts","op":"set","state":"pending"}}`, &got)
	out, err := c.PutApprovalCounts(context.Background(), client.ApprovalCountsWrite{})
	var pending *client.PendingApprovalError
	if !errors.As(err, &pending) || len(pending.Changes) != 1 || pending.Changes[0].ID != id {
		t.Fatalf("err = %v, want *PendingApprovalError naming %s", err, id)
	}
	if out.Minimum != 0 || len(out.Kinds) != 0 {
		t.Errorf("a held edit must not decode as counts: %+v", out)
	}
}
