// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// workloadBody is an apply body whose intent runs task under policy pol; extra is spliced into the intent.
func workloadBody(pol, task, extra string) string {
	return `{"intent":{"agent":"claude-code","task":"` + task + `","policy_id":"` + pol + `"` + extra + `}}`
}

func decodeApply(t *testing.T, w *httptest.ResponseRecorder, want int) client.WorkloadApplyResult {
	t.Helper()
	if w.Code != want {
		t.Fatalf("apply: %d %s, want %d", w.Code, w.Body.String(), want)
	}
	var res client.WorkloadApplyResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	return res
}

func launchedRun(t *testing.T, res client.WorkloadApplyResult) uuid.UUID {
	t.Helper()
	if res.Launch.State != client.WorkloadLaunched || res.Launch.RunID == nil {
		t.Fatalf("launch = %+v, want launched with a run", res.Launch)
	}
	return *res.Launch.RunID
}

func countAudit(events []types.AuditEvent, action string) int {
	n := 0
	for _, ev := range events {
		if ev.Action == action {
			n++
		}
	}
	return n
}

func workloadAuditIndex(events []types.AuditEvent, runID uuid.UUID, action string) int {
	for i, ev := range events {
		if ev.Action == action && ev.RunID != nil && *ev.RunID == runID {
			return i
		}
	}
	return -1
}

func killReason(t *testing.T, e svcEnv, runID uuid.UUID) string {
	t.Helper()
	ev := waitForRecAudit(t, e.h.audit, runID, "run.kill", "success")
	var d struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(ev.Data, &d)
	return d.Reason
}

// TestPG_Workload_UnchangedApplyIsANoOp: the same spec twice is one run and one workload.apply row.
func TestPG_Workload_UnchangedApplyIsANoOp(t *testing.T) {
	e := newSvcEnv(t)
	pol := e.plainPolicy(t, "wl-pol")
	first := decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", e.mem, workloadBody(pol, "t", "")), http.StatusCreated)
	runID := launchedRun(t, first)
	again := decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", e.mem, workloadBody(pol, "t", "")), http.StatusOK)
	if again.Outcome != client.WorkloadUnchanged || again.Launch.State != client.WorkloadNoLaunch {
		t.Fatalf("second apply outcome=%q launch=%+v, want unchanged and none", again.Outcome, again.Launch)
	}
	if again.Workload.RunID == nil || *again.Workload.RunID != runID {
		t.Fatalf("second apply run = %v, want %s kept", again.Workload.RunID, runID)
	}
	events := e.h.audit.snapshot()
	if n, l := countAudit(events, "workload.apply"), countAudit(events, "workload.launch"); n != 1 || l != 1 {
		t.Fatalf("workload.apply rows = %d, workload.launch rows = %d, want 1 and 1", n, l)
	}
}

// TestPG_Workload_ChangeCreatesThenKills: a changed spec starts the new run first, then kills the old one
// with reason workload_replaced.
func TestPG_Workload_ChangeCreatesThenKills(t *testing.T) {
	e := newSvcEnv(t)
	pol := e.plainPolicy(t, "wl-pol")
	oldRun := launchedRun(t, decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", e.mem, workloadBody(pol, "t", "")), http.StatusCreated))
	res := decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", e.mem, workloadBody(pol, "t2", "")), http.StatusOK)
	newRun := launchedRun(t, res)
	if newRun == oldRun || res.Outcome != client.WorkloadUpdated {
		t.Fatalf("changed apply outcome=%q run=%s, want updated and a new run", res.Outcome, newRun)
	}
	if got := killReason(t, e, oldRun); got != workloadKillReplaced {
		t.Fatalf("old run's run.kill reason = %q, want %q", got, workloadKillReplaced)
	}
	run, err := e.st.GetRun(context.Background(), oldRun)
	if err != nil || run.State != types.RunKilled {
		t.Fatalf("old run state = %q (%v), want KILLED", run.State, err)
	}
	events := e.h.audit.snapshot()
	launch, kill := workloadAuditIndex(events, newRun, "workload.launch"), workloadAuditIndex(events, oldRun, "run.kill")
	if launch < 0 || kill < 0 || launch > kill {
		t.Fatalf("workload.launch of the new run at %d, run.kill of the old at %d: want the create first", launch, kill)
	}
	if workloadAuditIndex(events, newRun, "workload.replace") < 0 {
		t.Fatal("no workload.replace row on the new run")
	}
}

// TestPG_Workload_CeilingRefusalPassesThrough: a launch the create path refuses answers 200 with the same
// reason a direct POST /runs gets, records it, and keeps the previous run.
func TestPG_Workload_CeilingRefusalPassesThrough(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	unit, err := e.st.UpsertGovernanceUnit(ctx, types.GovernanceUnit{
		ID: uuid.New(), Name: "walled",
		Ceiling:   types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}, MinConfinementClass: types.CC2},
		Limits:    types.GovernanceLimits{DenyInteractive: true},
		CreatedBy: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.UpsertGovernanceMembership(ctx, types.GovernanceMembership{
		ID: uuid.New(), SubjectType: types.CapabilitySubjectGroup, Subject: "walled", UnitID: unit.ID, CreatedBy: "test",
	}); err != nil {
		t.Fatal(err)
	}
	walled := accessSession(t, "walled-sub", "walled@corp.example", oidc.RoleUser, []string{"walled"})
	pol := e.plainPolicy(t, "wl-pol")
	oldRun := launchedRun(t, decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", walled, workloadBody(pol, "t", "")), http.StatusCreated))

	direct := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/runs", walled,
		`{"agent":"claude-code","task":"t","policy_id":"`+pol+`","interactive":true}`)
	if direct.Code == http.StatusCreated || respReason(direct) == "" {
		t.Fatalf("direct interactive create: %d %s, want a refusal with a reason", direct.Code, direct.Body.String())
	}
	res := decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", walled, workloadBody(pol, "t", `,"interactive":true`)), http.StatusOK)
	if res.Launch.State != client.WorkloadRefused || res.Launch.Reason != respReason(direct) {
		t.Fatalf("launch = %+v, want refused with %q", res.Launch, respReason(direct))
	}
	if res.Workload.RunID == nil || *res.Workload.RunID != oldRun || res.Workload.LastRefusal == nil {
		t.Fatalf("workload run=%v last_refusal=%v, want %s kept and the refusal recorded", res.Workload.RunID, res.Workload.LastRefusal, oldRun)
	}
	if run, err := e.st.GetRun(ctx, oldRun); err != nil || run.State == types.RunKilled {
		t.Fatalf("old run state = %q (%v), want it still running", run.State, err)
	}
	if countAudit(e.h.audit.snapshot(), "workload.launch_refused") != 1 {
		t.Fatal("want one workload.launch_refused row")
	}
}

// TestPG_Workload_SponsorReadsAndDeletes: a service's workload is its sponsor's to read and delete, and
// nobody else's to see.
func TestPG_Workload_SponsorReadsAndDeletes(t *testing.T) {
	e := newSvcEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	tok := e.mustMint(t, e.sponsor, svcName)
	pol := e.plainPolicy(t, "wl-pol")
	w := e.as(t, tok.Token, http.MethodPut, "/api/v1/workloads/web", workloadBody(pol, "t", ""))
	runID := launchedRun(t, decodeApply(t, w, http.StatusCreated))

	path := "/api/v1/workloads/web?owner=" + svcPrin
	if w := doSSO(t, e.h.srv, http.MethodGet, path, e.sponsor, ""); w.Code != http.StatusOK {
		t.Fatalf("sponsor GET: %d %s, want 200", w.Code, w.Body.String())
	}
	list := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/workloads", e.sponsor, "")
	var page []client.Workload
	if err := json.Unmarshal(list.Body.Bytes(), &page); err != nil || len(page) != 1 || page[0].Owner != svcPrin {
		t.Fatalf("sponsor list: %d %s, want the service's workload", list.Code, list.Body.String())
	}
	if w := doSSO(t, e.h.srv, http.MethodGet, path, e.other, ""); w.Code != http.StatusNotFound || respReason(w) != reasonWorkloadNotFound {
		t.Fatalf("other GET: %d %s, want 404 %s", w.Code, w.Body.String(), reasonWorkloadNotFound)
	}
	if w := doSSO(t, e.h.srv, http.MethodDelete, path, e.other, ""); w.Code != http.StatusNotFound {
		t.Fatalf("other DELETE: %d %s, want 404", w.Code, w.Body.String())
	}
	if w := doSSO(t, e.h.srv, http.MethodDelete, path, e.sponsor, ""); w.Code != http.StatusNoContent {
		t.Fatalf("sponsor DELETE: %d %s, want 204", w.Code, w.Body.String())
	}
	if got := killReason(t, e, runID); got != workloadKillDeleted {
		t.Fatalf("run.kill reason = %q, want %q", got, workloadKillDeleted)
	}
}

// TestPG_WorkloadBinding_NeedsAPerson: no API token, the admin token included, makes a binding.
func TestPG_WorkloadBinding_NeedsAPerson(t *testing.T) {
	e := newSvcEnv(t)
	body := `{"pool":"p","namespace":"apps","service":"` + svcName + `"}`
	if w := do(t, e.h.srv, http.MethodPost, "/api/v1/workload-bindings", adminToken, body); w.Code != http.StatusForbidden ||
		respReason(w) != reasonWorkloadBindingNeedsPerson {
		t.Fatalf("admin token: %d %s, want 403 %s", w.Code, w.Body.String(), reasonWorkloadBindingNeedsPerson)
	}
	ctx := withAPITokenID(withHumanIdentity(context.Background(), "sec", "sec@corp.example", oidc.RoleSecurityAdmin, "", nil, false), uuid.New())
	r := httptest.NewRequest(http.MethodPost, "/api/v1/workload-bindings", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	e.h.srv.handleCreateWorkloadBinding(w, r)
	if w.Code != http.StatusForbidden || respReason(w) != reasonWorkloadBindingNeedsPerson {
		t.Fatalf("a person's API token: %d %s, want 403 %s", w.Code, w.Body.String(), reasonWorkloadBindingNeedsPerson)
	}
}

// TestPG_Workload_BindingLaunchMatchesTokenLaunch: a launch under the service's bound authority writes the
// run.create row a launch with the service's token writes, apart from `via` (which neither carries here).
func TestPG_Workload_BindingLaunchMatchesTokenLaunch(t *testing.T) {
	e := newSvcEnv(t)
	si := e.mustCreate(t, svcName, svcSponsor)
	tok := e.mustMint(t, e.sponsor, svcName)
	pol := e.plainPolicy(t, "wl-pol")
	tokenRun := launchedRun(t, decodeApply(t, e.as(t, tok.Token, http.MethodPut, "/api/v1/workloads/web-a", workloadBody(pol, "t", "")), http.StatusCreated))

	in, invalid := normalizeWorkload(client.WorkloadDocument{APIVersion: client.TemplateDocumentVersion, Kind: client.WorkloadDocumentKind,
		Name: "web-b", Intent: decodeIntent(t, `{"agent":"claude-code","task":"t","policy_id":"`+pol+`"}`)})
	if invalid != nil {
		t.Fatalf("normalize: %+v", invalid)
	}
	in.Owner, in.Source, in.By = svcPrin, store.WorkloadSourceAPI, svcPrin
	wl, _, err := e.st.PutWorkload(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	sr, cancel, err := serviceLaunchRequest(si, si.Sponsor)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	launch, err := e.h.srv.launchWorkload(sr, e.st, wl)
	if err != nil || launch.RunID == nil {
		t.Fatalf("bound launch = %+v (%v), want a run", launch, err)
	}
	a := waitForRecAudit(t, e.h.audit, tokenRun, "run.create", "success")
	b := waitForRecAudit(t, e.h.audit, *launch.RunID, "run.create", "success")
	if a.Actor != b.Actor || a.ActorType != b.ActorType {
		t.Fatalf("run.create actor %s/%s vs %s/%s, want the same", a.ActorType, a.Actor, b.ActorType, b.Actor)
	}
	var da, db map[string]json.RawMessage
	_ = json.Unmarshal(a.Data, &da)
	_ = json.Unmarshal(b.Data, &db)
	for _, k := range []string{"agent", "policy_id", "confinement_class", "confinement_source", "policy_source", "user_type", "sponsor", "inline_policy", "service"} {
		if string(da[k]) != string(db[k]) {
			t.Errorf("run.create %s: token %s, bound %s", k, da[k], db[k])
		}
	}
	if _, ok := da["via"]; ok {
		t.Errorf("token launch run.create carries via: %s", a.Data)
	}
	if _, ok := db["via"]; ok {
		t.Errorf("bound launch run.create carries via: %s", b.Data)
	}
}

func decodeIntent(t *testing.T, raw string) client.TemplateIntent {
	t.Helper()
	var i client.TemplateIntent
	if err := json.Unmarshal([]byte(raw), &i); err != nil {
		t.Fatal(err)
	}
	return i
}
