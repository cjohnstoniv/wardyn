// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// reviewServer answers each "METHOD path" with the status and body registered for it and records the
// requests it saw, with their bodies.
type reviewServer struct {
	t      *testing.T
	routes map[string]reviewAnswer
	calls  []string
	bodies []string
}

type reviewAnswer struct {
	status int
	body   string
}

func newReviewServer(t *testing.T, routes map[string]reviewAnswer) (*reviewServer, string) {
	t.Helper()
	f := &reviewServer{t: t, routes: routes}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		f.bodies = append(f.bodies, string(raw))
		ans, ok := f.routes[r.Method+" "+r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if ans.status != 0 {
			w.WriteHeader(ans.status)
		}
		_, _ = io.WriteString(w, ans.body)
	}))
	t.Cleanup(srv.Close)
	return f, srv.URL
}

// changeBody is a pending change's JSON with extra spliced in after its diff.
func changeBody(id uuid.UUID, state, diffExtra, extra string) string {
	return fmt.Sprintf(`{"id":%q,"target_kind":"governance_unit","op":"update","target_key":"payments","state":%q,"proposed_by":"alice",`+
		`"proposed_at":"2026-11-20T09:00:00Z","expires_at":"2026-11-23T09:00:00Z","diff":{"changed":["allowed_domains"]%s}%s}`,
		id, state, diffExtra, extra)
}

const widensReview = `,"review":{"verdict":"widens","widens":[{"field":"allowed_domains","entries":["a.example","b.example","c.example"]}],` +
	`"risk":{"before":{"overall":"low","items":[]},"after":{"overall":"medium","items":[]}}}`

func approvalsBody(reach, reason string, eligible string) string {
	return `,"approvals":{"required":2,"require_security":true,"have":1,"entries":[` +
		`{"approver":"pat","tier":"security_admin","reason":"checked the diff","approved_at":"2026-11-20T10:00:00Z","counts":true},` +
		`{"approver":"lee","tier":"unit_admin","unit_name":"Payments","approved_at":"2026-11-20T09:30:00Z","counts":false,"dropped":"session_revoked"}],` +
		`"eligible":[` + eligible + `],"reach":"` + reach + `","cant_reach_reason":"` + reason + `","still_needed":1,"remaining_eligible":0}`
}

func TestChangesShow_PrintsVerdictRiskApprovalsAndReach(t *testing.T) {
	id := uuid.New()
	eligible := `{"principal":"kai-sub","email":"kai@example.com","tier":"security_admin"},{"principal":"sam-sub","tier":"unit_admin"}`
	f, url := newReviewServer(t, map[string]reviewAnswer{
		"GET /api/v1/governance/changes/" + id.String(): {body: changeBody(id, "pending", widensReview, approvalsBody("cant_reach", "count", eligible))},
	})
	out, _, err := runGovernance(t, url, "changes", "show", id.String())
	if err != nil {
		t.Fatalf("changes show: %v", err)
	}
	for _, want := range []string{
		"Kind:     governance_unit", "Op:       update", "Target:   payments", "State:    pending",
		"Widens: allowed_domains (+3: a.example, b.example, c.example)",
		"Risk:     low → medium",
		"Approvals: 1 of 2 approved (one must be a security admin)",
		"  pat (security admin) approved 2026-11-20T10:00:00Z: checked the diff",
		"  lee (unit admin of Payments) no longer counts: their sessions were revoked",
		"Can approve: kai@example.com, sam-sub",
		"Reach:    not enough people can still approve (approvals still needed: 1, people who can still give one: 0)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if len(f.calls) != 1 {
		t.Errorf("requests = %v, want only the read of the change", f.calls)
	}
}

func TestChangesShow_SecurityReachNamesNoNumber(t *testing.T) {
	id := uuid.New()
	_, url := newReviewServer(t, map[string]reviewAnswer{
		"GET /api/v1/governance/changes/" + id.String(): {body: changeBody(id, "pending", "", approvalsBody("cant_reach", "security", `{"principal":"sam-sub","tier":"unit_admin"}`))},
	})
	out, _, err := runGovernance(t, url, "changes", "show", id.String())
	if err != nil {
		t.Fatal(err)
	}
	var reach string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "Reach:") {
			reach = line
		}
	}
	if !strings.Contains(reach, "no one who can still approve is a security admin") || regexp.MustCompile(`\d`).MatchString(reach) {
		t.Errorf("reach line = %q, want the security sentence with no number", reach)
	}
}

func TestChangesShow_NoReviewNoApprovals(t *testing.T) {
	id := uuid.New()
	_, url := newReviewServer(t, map[string]reviewAnswer{
		"GET /api/v1/governance/changes/" + id.String(): {body: changeBody(id, "pending", "", "")},
	})
	out, _, err := runGovernance(t, url, "changes", "show", id.String())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Not checked\n") || !strings.Contains(out, "One person decides.") || strings.Contains(out, "Risk:") {
		t.Errorf("output:\n%s", out)
	}
}

func TestChangesShow_NarrowsHeld(t *testing.T) {
	id := uuid.New()
	review := `,"review":{"verdict":"narrows_held","held_because":["contact","rename"]}`
	_, url := newReviewServer(t, map[string]reviewAnswer{
		"GET /api/v1/governance/changes/" + id.String(): {body: changeBody(id, "pending", review, "")},
	})
	out, _, err := runGovernance(t, url, "changes", "show", id.String())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Narrows, but held because: contact, rename\n") {
		t.Errorf("output:\n%s", out)
	}
}

func TestChangesShow_ImpactListsTenPeopleAndTheRunsWithOwners(t *testing.T) {
	id := uuid.New()
	var people []string
	for i := 0; i < 12; i++ {
		people = append(people, fmt.Sprintf(`{"membership_id":%q,"subject_type":"user","subject":"person-%02d","from_unit_id":%q,"from_unit_name":"Old","unit_id":%q,"unit_name":"New","changes":[]}`,
			uuid.New(), i, uuid.Nil, uuid.Nil))
	}
	run := uuid.New()
	impact := `{"computed":"tree","as_of":"2026-11-20T10:00:00Z","people":[` + strings.Join(people, ",") + `],` +
		`"live_runs":[{"id":"` + run.String() + `","owner":"person-00","started_at":"2026-11-20T09:00:00Z"}],"live_runs_total":3}`
	f, url := newReviewServer(t, map[string]reviewAnswer{
		"GET /api/v1/governance/changes/" + id.String():             {body: changeBody(id, "pending", "", "")},
		"GET /api/v1/governance/changes/" + id.String() + "/impact": {body: impact},
	})
	out, _, err := runGovernance(t, url, "changes", "show", id.String(), "--impact")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"12 people, 3 live runs", "user person-09: Old -> New", "and 2 more people", "run " + run.String() + " owned by person-00", "and 2 more live runs"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "person-10") {
		t.Errorf("the eleventh person must be counted, not listed:\n%s", out)
	}
	if len(f.calls) != 2 {
		t.Errorf("requests = %v", f.calls)
	}
}

func TestChangesShow_ImpactBoundaryAndNotComputed(t *testing.T) {
	id := uuid.New()
	path := "GET /api/v1/governance/changes/" + id.String()
	boundary := `{"computed":"boundary","as_of":"2026-11-20T10:00:00Z","boundary":{"target":"boundary","deployment":[{"knob":"k","before":1,"after":0}],` +
		`"units":[],"stored_policies":[],"memberships":[],"provider_rows":[]}}`
	f, url := newReviewServer(t, map[string]reviewAnswer{
		path:             {body: changeBody(id, "pending", "", "")},
		path + "/impact": {body: boundary},
	})
	out, _, err := runGovernance(t, url, "changes", "show", id.String(), "--impact")
	if err != nil || !strings.Contains(out, "the boundary narrows 1 deployment knobs, 0 units") {
		t.Errorf("boundary: %v\n%s", err, out)
	}
	f.routes[path+"/impact"] = reviewAnswer{body: `{"computed":"none","not_computed":"stale","as_of":"2026-11-20T10:00:00Z"}`}
	out, _, err = runGovernance(t, url, "changes", "show", id.String(), "--impact")
	if err != nil || !strings.Contains(out, "Impact: not computed: the target changed since the change was proposed") {
		t.Errorf("not computed: %v\n%s", err, out)
	}
}

func TestChangesShow_JSONCarriesTheImpactUnderImpact(t *testing.T) {
	id := uuid.New()
	_, url := newReviewServer(t, map[string]reviewAnswer{
		"GET /api/v1/governance/changes/" + id.String():             {body: changeBody(id, "pending", "", "")},
		"GET /api/v1/governance/changes/" + id.String() + "/impact": {body: `{"computed":"none","not_computed":"kind","as_of":"2026-11-20T10:00:00Z"}`},
	})
	out, _, err := runGovernance(t, url, "changes", "show", id.String(), "--json")
	if err != nil {
		t.Fatal(err)
	}
	var plain map[string]any
	if err := json.Unmarshal([]byte(out), &plain); err != nil || plain["id"] != id.String() {
		t.Fatalf("--json = %q, %v", out, err)
	}
	if _, has := plain["impact"]; has {
		t.Errorf("--json without --impact must not carry an impact: %v", plain)
	}
	out, _, err = runGovernance(t, url, "changes", "show", id.String(), "--json", "--impact")
	if err != nil {
		t.Fatal(err)
	}
	var withImpact struct {
		ID     string `json:"id"`
		Impact struct {
			Computed    string `json:"computed"`
			NotComputed string `json:"not_computed"`
		} `json:"impact"`
	}
	if err := json.Unmarshal([]byte(out), &withImpact); err != nil || withImpact.ID != id.String() || withImpact.Impact.NotComputed != "kind" {
		t.Errorf("--json --impact = %q, %v", out, err)
	}
}

func TestChangesApprove_ReasonAndRecorded(t *testing.T) {
	id := uuid.New()
	path := "POST /api/v1/governance/changes/" + id.String() + "/approve"
	f, url := newReviewServer(t, map[string]reviewAnswer{
		path: {body: changeBody(id, "pending", "", `,"approvals":{"required":2,"require_security":false,"have":1}`)},
	})
	out, _, err := runGovernance(t, url, "changes", "approve", id.String(), "--reason", "checked the diff")
	if err != nil || out != "Recorded: 1 of 2 approved\n" {
		t.Errorf("approve --reason = %q, %v", out, err)
	}
	if got := f.bodies[0]; got != `{"reason":"checked the diff"}`+"\n" && got != `{"reason":"checked the diff"}` {
		t.Errorf("body = %q", got)
	}
	// Without a reason there is no body, and an applied change prints today's line.
	f.routes[path] = reviewAnswer{body: changeBody(id, "applied", "", "")}
	out, _, err = runGovernance(t, url, "changes", "approve", id.String())
	if err != nil || !strings.Contains(out, id.String()+" -> applied") || f.bodies[1] != "" {
		t.Errorf("approve = %q (body %q), %v", out, f.bodies[1], err)
	}
	if _, _, err := runGovernance(t, url, "changes", "approve", id.String(), "--reason", strings.Repeat("x", 513)); err == nil {
		t.Errorf("a reason past the limit must be refused before any request")
	}
}

func TestChangesTakeBack(t *testing.T) {
	id := uuid.New()
	f, url := newReviewServer(t, map[string]reviewAnswer{
		"DELETE /api/v1/governance/changes/" + id.String() + "/approval": {body: changeBody(id, "pending", "", `,"approvals":{"required":2,"require_security":false,"have":0}`)},
	})
	out, _, err := runGovernance(t, url, "changes", "take-back", id.String())
	if err != nil || !strings.Contains(out, "Took back your approval of governance change "+id.String()+": 0 of 2 approved") {
		t.Errorf("take-back = %q, %v", out, err)
	}
	if len(f.calls) != 1 {
		t.Errorf("requests = %v", f.calls)
	}
}

const countsTable = `{"minimum":2,"highest":3,"default":{"required":2,"require_security":false},"kinds":[` +
	`{"kind":"governance_unit","own":true,"required":3,"require_security":true,"approvers":"security_or_unit_admin","pool":{"status":"short","count":2,"unit_name":"Data Science"}},` +
	`{"kind":"role_mapping","own":false,"required":2,"require_security":false,"approvers":"super_admin","pool":{"status":"cant_confirm","count":1,"why":"too_many_people"}}]}`

func TestApprovalsNeededGet_MinimumUnsetSaysTheSentencePair(t *testing.T) {
	// A pending change keeps the snapshot it was proposed with, so the fake lists one that needs three
	// beside a minimum of zero. The output must not claim that every change needs one approval.
	id := uuid.New()
	f, url := newReviewServer(t, map[string]reviewAnswer{
		"GET /api/v1/governance/approval-counts": {body: `{"minimum":0,"highest":0,"default":{"required":0,"require_security":false},"kinds":[]}`},
		"GET /api/v1/governance/changes":         {body: "[" + changeBody(id, "pending", "", `,"approvals":{"required":3,"require_security":false,"have":1}`) + "]"},
	})
	out, _, err := runGovernance(t, url, "approvals-needed", "get")
	if err != nil {
		t.Fatal(err)
	}
	want := "Deploy-time minimum not set. New changes need one approval. Pending changes keep their existing approval requirements.\n"
	if out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
	if strings.Contains(strings.ToLower(out), "every change") {
		t.Errorf("output claims every change needs one approval: %q", out)
	}
	if strings.Join(f.calls, ",") != "GET /api/v1/governance/approval-counts" {
		t.Errorf("requests = %v, want the counts read only", f.calls)
	}
}

func TestApprovalsNeededGet_TableAndJSON(t *testing.T) {
	_, url := newReviewServer(t, map[string]reviewAnswer{"GET /api/v1/governance/approval-counts": {body: countsTable}})
	out, _, err := runGovernance(t, url, "approvals-needed", "get")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"KIND", "APPROVALS", "SECURITY ADMIN", "WHO CAN APPROVE", "CAN APPROVE NOW",
		"governance_unit", "security admins or unit admins", "2 (too few) in Data Science", "super admins", "cannot confirm (too many people to count)"} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}
	jsonOut, _, err := runGovernance(t, url, "approvals-needed", "get", "--json")
	var got struct {
		Highest int `json:"highest"`
	}
	if err != nil || json.Unmarshal([]byte(jsonOut), &got) != nil || got.Highest != 3 {
		t.Errorf("--json = %q, %v", jsonOut, err)
	}
}

func TestApprovalsNeededSet_SendsTheFileAndReportsAHeldEdit(t *testing.T) {
	body := `{"default":{"required":2,"require_security":false},"kinds":[{"kind":"governance_unit","required":3,"require_security":true}]}`
	file := filepath.Join(t.TempDir(), "counts.json")
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	path := "PUT /api/v1/governance/approval-counts"
	f, url := newReviewServer(t, map[string]reviewAnswer{path: {body: countsTable}})
	out, _, err := runGovernance(t, url, "approvals-needed", "set", "--file", file)
	if err != nil || !strings.Contains(out, "governance_unit") {
		t.Errorf("set = %q, %v", out, err)
	}
	var sent, want map[string]any
	if json.Unmarshal([]byte(f.bodies[0]), &sent) != nil || json.Unmarshal([]byte(body), &want) != nil || fmt.Sprint(sent) != fmt.Sprint(want) {
		t.Errorf("sent %q, want %s", f.bodies[0], body)
	}
	// Every person's edit is held: the id is printed on stderr and the exit code is 0.
	f.routes[path] = reviewAnswer{status: http.StatusAccepted, body: `{"pending_change":` + changeBody(id, "pending", "", "") + `}`}
	out, errOut, err := runGovernance(t, url, "approvals-needed", "set", "--file", file)
	if err != nil || out != "" || !strings.Contains(errOut, "pending approval: 1 change(s)") || !strings.Contains(errOut, id.String()) {
		t.Errorf("held set = %q / %q, %v", out, errOut, err)
	}
	if err := os.WriteFile(file, []byte(`{"default":{"required":2,"require_security":false},"kinds":[],"extra":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runGovernance(t, url, "approvals-needed", "set", "--file", file); err == nil {
		t.Errorf("a body with an unknown key must be refused before any request")
	}
}

func TestChangesShow_JSONKeepsZeroApprovalCounts(t *testing.T) {
	id := uuid.New()
	_, url := newReviewServer(t, map[string]reviewAnswer{
		"GET /api/v1/governance/changes/" + id.String(): {body: changeBody(id, "pending", "", approvalsBody("cant_reach", "count", ""))},
	})
	out, _, err := runGovernance(t, url, "changes", "show", id.String(), "--json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"remaining_eligible": 0`) || !strings.Contains(out, `"still_needed": 1`) {
		t.Errorf("--json dropped a zero count:\n%s", out)
	}
}

func TestChangesShow_DecidedChangeWithoutApprovalsSaysNothingAboutThem(t *testing.T) {
	id := uuid.New()
	_, url := newReviewServer(t, map[string]reviewAnswer{
		"GET /api/v1/governance/changes/" + id.String(): {body: changeBody(id, "applied", "", "")},
	})
	out, _, err := runGovernance(t, url, "changes", "show", id.String())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "One person decides") || strings.Contains(out, "Approvals") {
		t.Errorf("a decided change must not claim how it was approved:\n%s", out)
	}
	if !strings.Contains(out, "State:    applied") {
		t.Errorf("output:\n%s", out)
	}
}

func TestChangesShow_InactiveProposerRefusesTheLastApproval(t *testing.T) {
	id := uuid.New()
	body := strings.Replace(approvalsBody("ok", "", `{"principal":"kai-sub","tier":"security_admin"}`), `"still_needed":1`, `"still_needed":1,"proposer_active":false`, 1)
	_, url := newReviewServer(t, map[string]reviewAnswer{
		"GET /api/v1/governance/changes/" + id.String(): {body: changeBody(id, "pending", "", body)},
	})
	out, _, err := runGovernance(t, url, "changes", "show", id.String())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Proposer: no longer active, so the last approval will be refused") || strings.Contains(out, "enough people can still approve") {
		t.Errorf("output:\n%s", out)
	}
}

func TestApprovalsNeeded_AServerWithoutTheRouteSaysSo(t *testing.T) {
	file := filepath.Join(t.TempDir(), "counts.json")
	if err := os.WriteFile(file, []byte(`{"default":{"required":2,"require_security":false},"kinds":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, url := newReviewServer(t, map[string]reviewAnswer{})
	for _, args := range [][]string{{"approvals-needed", "get"}, {"approvals-needed", "set", "--file", file}} {
		_, _, err := runGovernance(t, url, args...)
		if err == nil || err.Error() != "This server does not offer approval counts yet." || exitCodeFor(err) != 1 {
			t.Errorf("%v: err = %v (exit %d), want the plain message and exit 1", args, err, exitCodeFor(err))
		}
	}
}

func TestChangesShow_ImpactOnADecidedChangeShowsTheChangeAndSaysWhy(t *testing.T) {
	id := uuid.New()
	f, url := newReviewServer(t, map[string]reviewAnswer{
		"GET /api/v1/governance/changes/" + id.String(): {body: changeBody(id, "applied", "", "")},
	})
	out, _, err := runGovernance(t, url, "changes", "show", id.String(), "--impact")
	if err != nil {
		t.Fatalf("changes show --impact on a decided change: %v", err)
	}
	if !strings.Contains(out, "State:    applied") || !strings.HasSuffix(out, "Impact is shown for pending changes only.\n") {
		t.Errorf("output:\n%s", out)
	}
	if len(f.calls) != 1 {
		t.Errorf("requests = %v, want the read of the change only", f.calls)
	}
	jsonOut, errOut, err := runGovernance(t, url, "changes", "show", id.String(), "--impact", "--json")
	var got map[string]any
	if err != nil || json.Unmarshal([]byte(jsonOut), &got) != nil || got["id"] != id.String() || !strings.Contains(errOut, "Impact is shown for pending changes only.") {
		t.Errorf("--json: %q / %q, %v", jsonOut, errOut, err)
	}
}
