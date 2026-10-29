// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

// TestPG_Workload_HealthReadsAsTheRunRoutesServeIt: a workload's state carries its run's service health
// projected as the run routes project it: unknown while the run is paused or its last read is stale.
func TestPG_Workload_HealthReadsAsTheRunRoutesServeIt(t *testing.T) {
	e := newSvcEnv(t)
	pol := e.plainPolicy(t, "wl-pol")
	runID := launchedRun(t, decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", e.mem, workloadBody(pol, "t", "")), http.StatusCreated))
	set := func(sql string) {
		t.Helper()
		if _, err := e.pool.Exec(context.Background(), sql, runID); err != nil {
			t.Fatal(err)
		}
	}
	reads := func() (string, string) {
		t.Helper()
		var one client.Workload
		if w := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/workloads/web", e.mem, ""); w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &one) != nil {
			t.Fatalf("GET: %d %s", w.Code, w.Body.String())
		}
		var page []client.Workload
		if w := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/workloads", e.mem, ""); w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page) != 1 {
			t.Fatalf("list: %d %s", w.Code, w.Body.String())
		}
		return one.State, page[0].State
	}
	set(`UPDATE agent_runs SET state = 'RUNNING', paused_at = NULL,
		service_status = jsonb_build_object('health', 'healthy', 'restarts', 0, 'observed_at', now()) WHERE id = $1`)
	if got, listed := reads(); got != "RUNNING/healthy" || listed != got {
		t.Fatalf("fresh health: GET %q, list %q; want RUNNING/healthy", got, listed)
	}
	set(`UPDATE agent_runs SET paused_at = now() WHERE id = $1`)
	if got, listed := reads(); got != "RUNNING/unknown" || listed != got {
		t.Fatalf("paused run: GET %q, list %q; want RUNNING/unknown", got, listed)
	}
	set(`UPDATE agent_runs SET paused_at = NULL,
		service_status = jsonb_build_object('health', 'healthy', 'restarts', 0, 'observed_at', now() - interval '1 hour') WHERE id = $1`)
	if got, listed := reads(); got != "RUNNING/unknown" || listed != got {
		t.Fatalf("stale health: GET %q, list %q; want RUNNING/unknown", got, listed)
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
	otherList := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/workloads?owner="+svcPrin, e.other, "")
	var otherPage []client.Workload
	if err := json.Unmarshal(otherList.Body.Bytes(), &otherPage); err != nil || otherList.Code != http.StatusOK || len(otherPage) != 0 {
		t.Fatalf("other list of the service: %d %s, want 200 and no workload", otherList.Code, otherList.Body.String())
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

// TestPG_Workload_HistoryListsOnlyThisWorkloadsRows: the history of a workload that was applied twice is its
// own workload.* rows, newest first, with the replace naming both runs, and none of another workload's.
func TestPG_Workload_HistoryListsOnlyThisWorkloadsRows(t *testing.T) {
	e := newSvcEnv(t)
	e.h.srv.cfg.Audit = store.Recorder{Pool: e.pool} // the history reads the audit table itself
	pol := e.plainPolicy(t, "wl-pol")
	oldRun := launchedRun(t, decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", e.mem, workloadBody(pol, "t", "")), http.StatusCreated))
	newRun := launchedRun(t, decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", e.mem, workloadBody(pol, "t2", "")), http.StatusOK))
	decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/api", e.mem, workloadBody(pol, "other", "")), http.StatusCreated)

	w := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/workloads/web/history", e.mem, "")
	if w.Code != http.StatusOK {
		t.Fatalf("history: %d %s, want 200", w.Code, w.Body.String())
	}
	var got []client.WorkloadEvent
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, ev := range got {
		actions = append(actions, ev.Action)
	}
	if want := []string{"replace", "launch", "apply", "launch", "apply"}; !slices.Equal(actions, want) {
		t.Fatalf("history actions = %v, want %v", actions, want)
	}
	if r := got[0]; r.Generation != 2 || r.RunID == nil || *r.RunID != newRun || r.EndedRunID == nil || *r.EndedRunID != oldRun {
		t.Fatalf("replace row = %+v, want generation 2, run %s, ended %s", r, newRun, oldRun)
	}
	if l := got[3]; l.Generation != 1 || l.RunID == nil || *l.RunID != oldRun {
		t.Fatalf("first launch row = %+v, want generation 1 and run %s", l, oldRun)
	}
}

// TestPG_Workload_HistoryIgnoresARowAForwardingDeviceClaimed: a laptop can forward a workload.launch_refused row
// whose target names a real owner/name, and the history is the organisation's own record, so that row is not in it.
// A refusal the organisation wrote for the same workload still is.
func TestPG_Workload_HistoryIgnoresARowAForwardingDeviceClaimed(t *testing.T) {
	e := newSvcEnv(t)
	e.h.srv.cfg.Audit = store.Recorder{Pool: e.pool}
	pol := e.plainPolicy(t, "wl-pol")
	decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", e.mem, workloadBody(pol, "t", "")), http.StatusCreated)
	var wl client.Workload
	if w := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/workloads/web", e.mem, ""); w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &wl) != nil {
		t.Fatalf("get workload: %d %s", w.Code, w.Body.String())
	}
	ctx := context.Background()
	target := wl.Owner + "/web"
	refused := func(generation int) types.AuditEvent {
		d, _ := json.Marshal(map[string]any{"generation": generation, "reason": "pool_lifetime_exceeded"})
		return types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC().Truncate(time.Microsecond), ActorType: types.ActorSystem,
			Actor: "wardynd", Action: "workload.launch_refused", Target: target, Outcome: "failure", Data: d}
	}

	forged := refused(99)
	if err := e.pool.QueryRow(ctx, `SELECT audit_row_hash($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb)`,
		"", forged.ID, forged.Time, forged.RunID, string(forged.ActorType), forged.Actor,
		forged.Action, forged.Target, forged.Outcome, forged.SourceIP, []byte(forged.Data)).Scan(&forged.RowHash); err != nil {
		t.Fatal(err)
	}
	dev, err := e.st.CreateDevice(ctx, types.Device{ID: uuid.New(), Name: "laptop"}, "wdd_"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.IngestDeviceAudit(ctx, dev.ID, "10.9.9.9", []types.FederatedAuditEvent{{AuditEvent: forged, Seq: 1}}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	own := refused(7)
	if err := store.InsertAuditEvent(ctx, e.pool, &own); err != nil {
		t.Fatal(err)
	}

	w := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/workloads/web/history", e.mem, "")
	if w.Code != http.StatusOK {
		t.Fatalf("history: %d %s, want 200", w.Code, w.Body.String())
	}
	var got []client.WorkloadEvent
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	var refusedGens []int64
	for _, ev := range got {
		if ev.Action == "launch_refused" {
			refusedGens = append(refusedGens, ev.Generation)
		}
	}
	if want := []int64{7}; !slices.Equal(refusedGens, want) {
		t.Fatalf("launch_refused generations = %v, want %v: the organisation's row, not the forwarded one (99)", refusedGens, want)
	}
}

// TestPG_Workload_HistoryHasTheWorkloadReadGate: a service's workload history is its sponsor's to read, and
// nobody else's, as the workload itself is.
func TestPG_Workload_HistoryHasTheWorkloadReadGate(t *testing.T) {
	e := newSvcEnv(t)
	e.h.srv.cfg.Audit = store.Recorder{Pool: e.pool}
	e.mustCreate(t, svcName, svcSponsor)
	tok := e.mustMint(t, e.sponsor, svcName)
	pol := e.plainPolicy(t, "wl-pol")
	decodeApply(t, e.as(t, tok.Token, http.MethodPut, "/api/v1/workloads/web", workloadBody(pol, "t", "")), http.StatusCreated)

	path := "/api/v1/workloads/web/history?owner=" + svcPrin
	if w := doSSO(t, e.h.srv, http.MethodGet, path, e.sponsor, ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"action":"apply"`) {
		t.Fatalf("sponsor history: %d %s, want 200 with the apply row", w.Code, w.Body.String())
	}
	if w := doSSO(t, e.h.srv, http.MethodGet, path, e.other, ""); w.Code != http.StatusNotFound || respReason(w) != reasonWorkloadNotFound {
		t.Fatalf("other history: %d %s, want 404 %s", w.Code, w.Body.String(), reasonWorkloadNotFound)
	}
	if w := e.as(t, tok.Token, http.MethodGet, "/api/v1/workloads/web/history", ""); w.Code != http.StatusForbidden {
		t.Fatalf("service token history: %d %s, want 403 service_scope", w.Code, w.Body.String())
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

// TestPG_WorkloadBinding_NamesAnOrganizationPoolOnly: a person's own pool is refused 422, named by id or by
// name, so a personal runner never launches as a service; an organization pool binds.
func TestPG_WorkloadBinding_NamesAnOrganizationPoolOnly(t *testing.T) {
	e := newSvcEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	ctx := context.Background()
	mine, err := e.st.CreateRunnerPool(ctx, types.RunnerPool{ID: uuid.New(), Name: "My laptop", Scope: types.RunnerPoolUser, Owner: svcSponsor}, svcSponsor)
	if err != nil {
		t.Fatal(err)
	}
	org, err := e.st.CreateRunnerPool(ctx, types.RunnerPool{ID: uuid.New(), Name: "Platform", Scope: types.RunnerPoolOrganization}, "test")
	if err != nil {
		t.Fatal(err)
	}
	bind := func(pool, ns string) *httptest.ResponseRecorder {
		return doSSO(t, e.h.srv, http.MethodPost, "/api/v1/workload-bindings", e.sec, `{"pool":"`+pool+`","namespace":"`+ns+`","service":"`+svcName+`"}`)
	}
	for _, ref := range []string{mine.ID.String(), mine.Name} {
		if w := bind(ref, "apps"); w.Code != http.StatusUnprocessableEntity || respReason(w) != reasonWorkloadBindingPoolScope {
			t.Errorf("bind by %q: %d %s, want 422 %s", ref, w.Code, w.Body.String(), reasonWorkloadBindingPoolScope)
		}
	}
	if left, err := e.st.ListWorkloadBindings(ctx, store.Page{}); err != nil || len(left) != 0 {
		t.Fatalf("bindings after the refusals = %+v (%v), want none", left, err)
	}
	for _, ref := range []string{org.ID.String(), org.Name} {
		ns := map[string]string{org.ID.String(): "by-id", org.Name: "by-name"}[ref]
		if w := bind(ref, ns); w.Code != http.StatusCreated {
			t.Errorf("bind %q on the organization pool: %d %s, want 201", ref, w.Code, w.Body.String())
		}
	}
}

// TestPG_WorkloadBinding_RefusesAnInactiveSponsor: a binding for an identity whose sponsor's access was
// revoked is refused 409 at the door, as a token mint is, and no row is written.
func TestPG_WorkloadBinding_RefusesAnInactiveSponsor(t *testing.T) {
	e := newSvcEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	ctx := context.Background()
	org, err := e.st.CreateRunnerPool(ctx, types.RunnerPool{ID: uuid.New(), Name: "Platform", Scope: types.RunnerPoolOrganization}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.rev.RevokeSub(ctx, svcSponsor); err != nil {
		t.Fatal(err)
	}
	w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/workload-bindings", e.sec, `{"pool":"`+org.Name+`","namespace":"apps","service":"`+svcName+`"}`)
	if w.Code != http.StatusConflict || respReason(w) != reasonServiceIdentityInactive {
		t.Fatalf("bind with a revoked sponsor: %d %s, want 409 %s", w.Code, w.Body.String(), reasonServiceIdentityInactive)
	}
	if left, err := e.st.ListWorkloadBindings(ctx, store.Page{}); err != nil || len(left) != 0 {
		t.Fatalf("bindings after the refusal = %+v (%v), want none", left, err)
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

// cutShort is the store with the request cut short where a workload lets its run go: once the delete has
// committed, or once the launch's record is lost (lose: SetWorkloadRun answers that another launch was
// recorded first, recording nothing), cancel is called before the run is read. With failReads set, every
// run read fails from then on.
type cutShort struct {
	store.PG
	cancel    context.CancelFunc
	lose      bool
	failReads *atomic.Bool
}

func (s cutShort) cut() {
	s.cancel()
	if s.failReads != nil {
		s.failReads.Store(true)
	}
}

func (s cutShort) DeleteWorkload(ctx context.Context, owner, name string) (store.Workload, error) {
	w, err := s.PG.DeleteWorkload(ctx, owner, name)
	s.cut()
	return w, err
}

func (s cutShort) SetWorkloadRun(ctx context.Context, id, claim uuid.UUID, generation int64, runID uuid.UUID) (*uuid.UUID, error) {
	if !s.lose {
		return s.PG.SetWorkloadRun(ctx, id, claim, generation, runID)
	}
	s.cut()
	return nil, store.ErrWorkloadSuperseded
}

func (s cutShort) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	if s.failReads != nil && s.failReads.Load() {
		return types.AgentRun{}, errors.New("run unreadable")
	}
	return s.PG.GetRun(ctx, id)
}

// owedEnds counts runID's rows in workload_run_ends.
func owedEnds(t *testing.T, e svcEnv, runID uuid.UUID) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM workload_run_ends WHERE run_id = $1`, runID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// wantKilledFor waits for runID's run.kill row and checks the run is KILLED for why and owes nothing.
func wantKilledFor(t *testing.T, e svcEnv, runID uuid.UUID, why string) {
	t.Helper()
	if got := killReason(t, e, runID); got != why {
		t.Fatalf("run.kill reason = %q, want %q", got, why)
	}
	if run, err := e.st.GetRun(context.Background(), runID); err != nil || run.State != types.RunKilled {
		t.Fatalf("run state = %q (%v), want KILLED", run.State, err)
	}
	if n := owedEnds(t, e, runID); n != 0 {
		t.Fatalf("workload_run_ends rows for the killed run = %d, want 0", n)
	}
}

// TestPG_Workload_DeleteEndsTheRunWhenTheCallerGoesAway: a caller who goes away once the delete has
// committed, before the run is read, does not leave the run running.
func TestPG_Workload_DeleteEndsTheRunWhenTheCallerGoesAway(t *testing.T) {
	e := newSvcEnv(t)
	pol := e.plainPolicy(t, "wl-pol")
	runID := launchedRun(t, decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", e.mem, workloadBody(pol, "t", "")), http.StatusCreated))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.h.srv.cfg.Store = cutShort{PG: e.st, cancel: cancel}
	if w := doSSOCtx(t, e.h.srv, ctx, http.MethodDelete, "/api/v1/workloads/web", e.mem, ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s, want 204", w.Code, w.Body.String())
	}
	wantKilledFor(t, e, runID, workloadKillDeleted)
}

// TestPG_Workload_LostRecordEndsTheRunWhenTheCallerGoesAway: a launch whose record another launch won
// ends the run it started, though its caller went away before the run was read.
func TestPG_Workload_LostRecordEndsTheRunWhenTheCallerGoesAway(t *testing.T) {
	e := newSvcEnv(t)
	pol := e.plainPolicy(t, "wl-pol")
	oldRun := launchedRun(t, decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", e.mem, workloadBody(pol, "t", "")), http.StatusCreated))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.h.srv.cfg.Store = cutShort{PG: e.st, cancel: cancel, lose: true}
	res := decodeApply(t, doSSOCtx(t, e.h.srv, ctx, http.MethodPut, "/api/v1/workloads/web", e.mem, workloadBody(pol, "t2", "")), http.StatusOK)
	if res.Launch.State != client.WorkloadNoLaunch {
		t.Fatalf("launch = %+v, want none: the record was lost", res.Launch)
	}
	e.h.srv.cfg.Store = e.st
	var lost []types.AuditEvent
	for _, ev := range e.h.audit.snapshot() {
		if ev.Action == "run.create" && ev.RunID != nil && *ev.RunID != oldRun {
			lost = append(lost, ev)
		}
	}
	if len(lost) != 1 {
		t.Fatalf("run.create rows for a new run = %d, want 1", len(lost))
	}
	wantKilledFor(t, e, *lost[0].RunID, workloadKillReplaced)
	if run, err := e.st.GetRun(context.Background(), oldRun); err != nil || isTerminalRunState(run.State) {
		t.Fatalf("the workload's own run = %q (%v), want it still live", run.State, err)
	}
}

// TestPG_Workload_UnreadableRunStaysOwedUntilTheSweepEndsIt: a run that cannot be read once its workload
// is deleted is left owed its end, and the sweep ends it.
func TestPG_Workload_UnreadableRunStaysOwedUntilTheSweepEndsIt(t *testing.T) {
	e := newSvcEnv(t)
	pol := e.plainPolicy(t, "wl-pol")
	runID := launchedRun(t, decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", e.mem, workloadBody(pol, "t", "")), http.StatusCreated))
	e.h.srv.cfg.Store = cutShort{PG: e.st, cancel: func() {}, failReads: new(atomic.Bool)}
	if w := doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/workloads/web", e.mem, ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s, want 204", w.Code, w.Body.String())
	}
	e.h.srv.cfg.Store = e.st
	if run, err := e.st.GetRun(context.Background(), runID); err != nil || isTerminalRunState(run.State) {
		t.Fatalf("run state = %q (%v), want it not yet ended", run.State, err)
	}
	if n := owedEnds(t, e, runID); n != 1 {
		t.Fatalf("workload_run_ends rows = %d, want the run owed its end", n)
	}
	if err := e.h.srv.sweepWorkloadRunEnds(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := owedEnds(t, e, runID); n != 1 {
		t.Fatalf("workload_run_ends rows = %d: the sweep took an end younger than its grace", n)
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE workload_run_ends SET created_at = now() - interval '1 hour' WHERE run_id = $1`, runID); err != nil {
		t.Fatal(err)
	}
	if err := e.h.srv.sweepWorkloadRunEnds(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantKilledFor(t, e, runID, workloadKillDeleted)
	if ev := waitForRecAudit(t, e.h.audit, runID, "run.kill", "success"); ev.ActorType != types.ActorSystem {
		t.Fatalf("the sweep's run.kill actor = %s/%s, want system", ev.ActorType, ev.Actor)
	}
}

// hostAxisIntent carries each host-axis field a workload read withholds from the security tier, beside the
// egress axis it keeps.
const hostAxisIntent = `,"image":"registry.corp.internal/base:1","inline_policy":{"allowed_domains":["artifacts.corp.internal"],` +
	`"workspace_mounts":[{"source":"/srv/nfs-prod/payments","target":"/work/data"}],` +
	`"llm_inspection":{"mode":"alert","detect_secrets":true,"workspace_secret_names":["acme-prod-db-password"]}}`

// hostAxisView is the part of a read workload's intent hostAxisIntent wrote.
type hostAxisView struct {
	Image        string `json:"image"`
	InlinePolicy struct {
		AllowedDomains  []string `json:"allowed_domains"`
		WorkspaceMounts []struct {
			Source string `json:"source"`
			Target string `json:"target"`
		} `json:"workspace_mounts"`
		LLMInspection map[string]json.RawMessage `json:"llm_inspection"`
	} `json:"inline_policy"`
}

// TestPG_Workload_ForeignSecurityReadWithholdsTheHostAxis: the owner and an operator read a workload's
// intent whole; the security tier reading another owner's, by GET or in a list, reads it with the mount's
// host path and the image blanked and the secret names gone, and the egress and the rest kept.
func TestPG_Workload_ForeignSecurityReadWithholdsTheHostAxis(t *testing.T) {
	e := newSvcEnv(t)
	// No policy_id: an intent names it or an inline_policy, not both.
	body := `{"intent":{"agent":"claude-code","task":"t"` + hostAxisIntent + `}}`
	decodeApply(t, doSSO(t, e.h.srv, http.MethodPut, "/api/v1/workloads/web", e.mem, body), http.StatusCreated)

	read := func(c *http.Cookie, path string) hostAxisView {
		t.Helper()
		w := doSSO(t, e.h.srv, http.MethodGet, path, c, "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, w.Code, w.Body.String())
		}
		var one struct {
			Owner  string          `json:"owner"`
			Intent json.RawMessage `json:"intent"`
		}
		var page []struct {
			Owner  string          `json:"owner"`
			Intent json.RawMessage `json:"intent"`
		}
		raw := w.Body.Bytes()
		if json.Unmarshal(raw, &page) == nil {
			for _, p := range page {
				if p.Owner == "mem" {
					one = p
				}
			}
		} else if err := json.Unmarshal(raw, &one); err != nil {
			t.Fatal(err)
		}
		if one.Owner != "mem" {
			t.Fatalf("GET %s: no workload of mem in %s", path, raw)
		}
		var v hostAxisView
		if err := json.Unmarshal(one.Intent, &v); err != nil {
			t.Fatalf("GET %s intent: %v", path, err)
		}
		return v
	}
	check := func(what string, v hostAxisView, projected bool) {
		t.Helper()
		image, source, names := "registry.corp.internal/base:1", "/srv/nfs-prod/payments", true
		if projected {
			image, source, names = "", "", false
		}
		if v.Image != image {
			t.Errorf("%s: image = %q, want %q", what, v.Image, image)
		}
		if m := v.InlinePolicy.WorkspaceMounts; len(m) != 1 || m[0].Source != source || m[0].Target != "/work/data" {
			t.Errorf("%s: workspace_mounts = %+v, want source %q and the target kept", what, m, source)
		}
		if _, ok := v.InlinePolicy.LLMInspection["workspace_secret_names"]; ok != names {
			t.Errorf("%s: llm_inspection = %v, workspace_secret_names present = %v, want %v", what, v.InlinePolicy.LLMInspection, ok, names)
		}
		if string(v.InlinePolicy.LLMInspection["detect_secrets"]) != "true" {
			t.Errorf("%s: llm_inspection.detect_secrets = %s, want it kept", what, v.InlinePolicy.LLMInspection["detect_secrets"])
		}
		if d := v.InlinePolicy.AllowedDomains; len(d) != 1 || d[0] != "artifacts.corp.internal" {
			t.Errorf("%s: allowed_domains = %v, want the egress kept", what, d)
		}
	}
	check("owner GET", read(e.mem, "/api/v1/workloads/web"), false)
	check("owner list", read(e.mem, "/api/v1/workloads"), false)
	check("operator GET", read(e.admin, "/api/v1/workloads/web?owner=mem"), false)
	check("security GET", read(e.sec, "/api/v1/workloads/web?owner=mem"), true)
	check("security list", read(e.sec, "/api/v1/workloads"), true)
	check("security list of the owner", read(e.sec, "/api/v1/workloads?owner=mem"), true)
}

// holdingWorkload makes the person's workload "web", a Background task in pool, hold r, a running run, at
// generation 1. It returns the workload's intent for a task.
func (e *queueEnv) holdingWorkload(pool types.RunnerPool, r types.AgentRun) func(task string) string {
	e.t.Helper()
	ctx := context.Background()
	intent := func(task string) string {
		return `{"experience":"background","workload":{"kind":"agent_task","agent":"claude-code","task":"` + task + `"},` +
			`"tools":[{"id":"claude-code","kind":"harness"}],"no_repositories_or_drives":true,"runner_pool_id":"` + pool.ID.String() + `"}`
	}
	in, invalid := normalizeWorkload(client.WorkloadDocument{APIVersion: client.TemplateDocumentVersion, Kind: client.WorkloadDocumentKind,
		Name: "web", Intent: decodeIntent(e.t, intent("t"))})
	if invalid != nil {
		e.t.Fatalf("normalize: %+v", invalid)
	}
	in.Owner, in.Source, in.By = e.personP, store.WorkloadSourceAPI, e.personP
	wl, _, err := e.pg.PutWorkload(ctx, in)
	if err != nil {
		e.t.Fatal(err)
	}
	claimed, ok, err := e.pg.ClaimWorkloadLaunch(ctx, wl.ID)
	if err != nil || !ok {
		e.t.Fatalf("claim = %v, %v", ok, err)
	}
	if _, err := e.pg.SetWorkloadRun(ctx, wl.ID, claimed.ClaimToken, claimed.Generation, r.ID); err != nil {
		e.t.Fatal(err)
	}
	return intent
}

// queuedWorkloadReplacement makes the person's workload "web" hold r, a running run in another pool than
// pool, then applies a changed spec into pool while pool is full: the new run waits in the queue. It
// returns the new run.
func (e *queueEnv) queuedWorkloadReplacement(pool types.RunnerPool, r types.AgentRun) uuid.UUID {
	e.t.Helper()
	intent := e.holdingWorkload(pool, r)
	res := decodeApply(e.t, e.call(e.person, http.MethodPut, "/api/v1/workloads/web", `{"intent":`+intent("t2")+`}`), http.StatusOK)
	queued := launchedRun(e.t, res)
	if got := e.run(queued).State; got != types.RunQueued {
		e.t.Fatalf("replacement state = %s, want QUEUED in the full pool", got)
	}
	if got := e.run(r.ID).State; got != types.RunRunning {
		e.t.Fatalf("running run state = %s once its replacement queued, want RUNNING", got)
	}
	if len(e.audits(r.ID, "run.kill")) != 0 || len(e.audits(queued, "workload.replace")) != 0 {
		e.t.Fatal("a queued replacement ended the running run")
	}
	return queued
}

// settleWorkloadRunEnds runs the workload sweep over every owed end, however young.
func (e *queueEnv) settleWorkloadRunEnds() {
	e.t.Helper()
	e.exec(`UPDATE workload_run_ends SET created_at = now() - interval '1 hour'`)
	if err := e.srv.sweepWorkloadRunEnds(context.Background()); err != nil {
		e.t.Fatal(err)
	}
}

// TestPG_Workload_QueuedReplacementTakesOverOnceRunning: a replacement that waits in another pool's queue
// leaves the running run alone while it waits; once it runs, the sweep ends the old run and records the replacement.
func TestPG_Workload_QueuedReplacementTakesOverOnceRunning(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	occupant, running := e.occupy(pool), e.occupy(e.cappedPool("Old", 1))
	queued := e.queuedWorkloadReplacement(pool, running)

	e.settleWorkloadRunEnds()
	if got := e.run(running.ID).State; got != types.RunRunning {
		t.Fatalf("running run state = %s while its replacement waits, want RUNNING", got)
	}
	e.free(occupant)
	e.waitStarted(queued)
	e.exec(`UPDATE agent_runs SET state = 'RUNNING' WHERE id = $1`, queued) // the runner reports it up
	e.settleWorkloadRunEnds()
	if got := e.run(running.ID).State; got != types.RunKilled {
		t.Fatalf("old run state = %s once its replacement runs, want KILLED", got)
	}
	kills := e.waitAudit(running.ID, "run.kill")
	if d := queueAuditData(t, kills[0]); d["reason"] != workloadKillReplaced {
		t.Fatalf("run.kill data = %v, want reason %s", d, workloadKillReplaced)
	}
	if rows := e.audits(queued, "workload.replace"); len(rows) != 1 {
		t.Fatalf("workload.replace rows on the replacement = %d, want 1", len(rows))
	}
	if n := e.count(`SELECT count(*) FROM workload_run_ends`); n != 0 {
		t.Fatalf("owed ends left = %d, want 0", n)
	}
}

// TestPG_Workload_RefusedQueuedReplacementKeepsTheRunningRun: a replacement whose queued start is refused
// leaves the running run running; the workload takes it back and records the refusal, owed a launch again.
func TestPG_Workload_RefusedQueuedReplacementKeepsTheRunningRun(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	occupant, running := e.occupy(pool), e.occupy(e.cappedPool("Old", 1))
	queued := e.queuedWorkloadReplacement(pool, running)

	if _, err := e.pg.UpsertCapabilityGrant(context.Background(), types.CapabilityGrant{SubjectType: types.CapabilitySubjectUser,
		Subject: e.personP, Capability: capAgent, Value: "claude-code", Effect: types.CapabilityDeny, CreatedBy: "seed"}); err != nil {
		t.Fatal(err)
	}
	e.free(occupant)
	if got := e.waitFailed(queued); got.StatusDetail != "queue: capability_agent" {
		t.Fatalf("replacement status_detail = %q, want its start refused", got.StatusDetail)
	}
	e.settleWorkloadRunEnds()
	if got := e.run(running.ID).State; got != types.RunRunning {
		t.Fatalf("old run state = %s after its replacement was refused, want RUNNING", got)
	}
	v, err := e.pg.GetWorkload(context.Background(), e.personP, "web")
	if err != nil {
		t.Fatal(err)
	}
	if v.CurrentRunID == nil || *v.CurrentRunID != running.ID || v.CurrentGeneration != 0 {
		t.Fatalf("workload run=%v generation=%d, want %s back and a launch owed", v.CurrentRunID, v.CurrentGeneration, running.ID)
	}
	if f := v.LastRefusal; f == nil || f.Generation != 2 || f.Reason != "capability_agent" {
		t.Fatalf("last_refusal = %+v, want generation 2 refused for capability_agent", f)
	}
	if rows := e.audits(queued, "workload.launch_refused"); len(rows) != 1 || rows[0].Outcome != "denied" {
		t.Fatalf("workload.launch_refused rows = %+v, want one denied", rows)
	}
	if len(e.audits(running.ID, "run.kill")) != 0 {
		t.Fatal("the running run was killed")
	}
	if n := e.count(`SELECT count(*) FROM workload_run_ends`); n != 0 {
		t.Fatalf("owed ends left = %d, want 0", n)
	}
}

// TestPG_Workload_ReplacementThatRanThenEndedStillReplaces: a queued replacement that started and then
// ended before the sweep saw it running ended normally: the old run is still ended, with no restore and no
// refusal.
func TestPG_Workload_ReplacementThatRanThenEndedStillReplaces(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	occupant, running := e.occupy(pool), e.occupy(e.cappedPool("Old", 1))
	queued := e.queuedWorkloadReplacement(pool, running)

	e.free(occupant)
	e.waitStarted(queued)
	e.exec(`UPDATE agent_runs SET state = 'FAILED', status_detail = 'agent exited' WHERE id = $1`, queued) // ran, then crashed
	e.settleWorkloadRunEnds()
	if got := e.run(running.ID).State; got != types.RunKilled {
		t.Fatalf("old run state = %s after its replacement ran and ended, want KILLED", got)
	}
	v, err := e.pg.GetWorkload(context.Background(), e.personP, "web")
	if err != nil {
		t.Fatal(err)
	}
	if v.CurrentRunID == nil || *v.CurrentRunID != queued || v.CurrentGeneration != 2 || v.LastRefusal != nil {
		t.Fatalf("workload run=%v generation=%d refusal=%+v, want %s kept at generation 2 and no refusal", v.CurrentRunID, v.CurrentGeneration, v.LastRefusal, queued)
	}
	if len(e.audits(queued, "workload.launch_refused")) != 0 {
		t.Fatal("a replacement that ran was recorded as refused")
	}
	if rows := e.audits(queued, "workload.replace"); len(rows) != 1 {
		t.Fatalf("workload.replace rows on the replacement = %d, want 1", len(rows))
	}
	if n := e.count(`SELECT count(*) FROM workload_run_ends`); n != 0 {
		t.Fatalf("owed ends left = %d, want 0", n)
	}
}

// TestPG_Workload_ReplacementQueuedBehindItsOwnRunReplacesInPlace: in a pool of one slot, held by the
// workload's own run, a changed spec could never start beside it: the old run ends at once, recorded as a
// replacement, and the new one starts in the slot it freed.
func TestPG_Workload_ReplacementQueuedBehindItsOwnRunReplacesInPlace(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Solo", 1)
	running := e.occupy(pool)
	intent := e.holdingWorkload(pool, running)

	res := decodeApply(t, e.call(e.person, http.MethodPut, "/api/v1/workloads/web", `{"intent":`+intent("t2")+`}`), http.StatusOK)
	queued := launchedRun(t, res)
	if got := e.run(running.ID).State; got != types.RunKilled {
		t.Fatalf("old run state = %s once its replacement queued behind it, want KILLED", got)
	}
	kills := e.waitAudit(running.ID, "run.kill")
	if d := queueAuditData(t, kills[0]); d["reason"] != workloadKillReplaced {
		t.Fatalf("run.kill data = %v, want reason %s", d, workloadKillReplaced)
	}
	if rows := e.audits(queued, "workload.replace"); len(rows) != 1 {
		t.Fatalf("workload.replace rows on the replacement = %d, want 1", len(rows))
	}
	e.waitStarted(queued)
	if n := e.count(`SELECT count(*) FROM workload_run_ends`); n != 0 {
		t.Fatalf("owed ends left = %d, want 0", n)
	}
}

// TestPG_Workload_AdminRestartChecksTheSponsorSinceTheSpec: a super admin's restart of a service's workload
// launches as the service only while its sponsor's sessions have not been cut since the spec was written. A
// security_admin's restart is refused outright, with the 404 a foreign workload gets, and launches nothing.
func TestPG_Workload_AdminRestartChecksTheSponsorSinceTheSpec(t *testing.T) {
	e := newSvcEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	tok := e.mustMint(t, e.sponsor, svcName)
	pol := e.plainPolicy(t, "wl-pol")
	launchedRun(t, decodeApply(t, e.as(t, tok.Token, http.MethodPut, "/api/v1/workloads/web", workloadBody(pol, "t", "")), http.StatusCreated))
	path := "/api/v1/workloads/web/restart?owner=" + svcPrin
	if w := doSSO(t, e.h.srv, http.MethodPost, path, e.sec, ""); w.Code != http.StatusNotFound || respReason(w) != reasonWorkloadNotFound {
		t.Fatalf("security_admin restart: %d %s, want 404 %s", w.Code, w.Body.String(), reasonWorkloadNotFound)
	}
	if n := countAudit(e.h.audit.snapshot(), "workload.restart"); n != 0 {
		t.Fatalf("workload.restart rows after the refused restart = %d, want 0", n)
	}
	launchedRun(t, decodeApply(t, doSSO(t, e.h.srv, http.MethodPost, path, e.super, ""), http.StatusOK))

	if err := e.rev.RevokeSub(context.Background(), svcSponsor); err != nil {
		t.Fatal(err)
	}
	res := decodeApply(t, doSSO(t, e.h.srv, http.MethodPost, path, e.super, ""), http.StatusOK)
	if res.Launch.State != client.WorkloadRefused || res.Launch.Reason != reasonServiceIdentityInactive {
		t.Fatalf("restart after the sponsor's access was revoked = %+v, want refused %s", res.Launch, reasonServiceIdentityInactive)
	}
}

// TestPG_Workload_SecurityAdminDeletesButCannotLaunch: a security_admin and a super admin both delete a
// service's workload (the containment power), and the delete ends its run.
func TestPG_Workload_SecurityAdminDeletesButCannotLaunch(t *testing.T) {
	e := newSvcEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	tok := e.mustMint(t, e.sponsor, svcName)
	pol := e.plainPolicy(t, "wl-pol")
	for _, name := range []string{"web-a", "web-b"} {
		launchedRun(t, decodeApply(t, e.as(t, tok.Token, http.MethodPut, "/api/v1/workloads/"+name, workloadBody(pol, name, "")), http.StatusCreated))
	}
	for name, as := range map[string]*http.Cookie{"web-a": e.sec, "web-b": e.super} {
		if w := doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/workloads/"+name+"?owner="+svcPrin, as, ""); w.Code != http.StatusNoContent {
			t.Errorf("DELETE %s: %d %s, want 204", name, w.Code, w.Body.String())
		}
	}
}

// TestPG_Workload_RestartRowsNameThePerson: a sponsor's restart of a service's workload launches the run as
// the service, while the workload rows and the replaced run's run.kill row name the sponsor.
func TestPG_Workload_RestartRowsNameThePerson(t *testing.T) {
	e := newSvcEnv(t)
	e.mustCreate(t, svcName, svcSponsor)
	tok := e.mustMint(t, e.sponsor, svcName)
	pol := e.plainPolicy(t, "wl-pol")
	oldRun := launchedRun(t, decodeApply(t, e.as(t, tok.Token, http.MethodPut, "/api/v1/workloads/web", workloadBody(pol, "t", "")), http.StatusCreated))
	newRun := launchedRun(t, decodeApply(t, doSSO(t, e.h.srv, http.MethodPost, "/api/v1/workloads/web/restart?owner="+svcPrin, e.sponsor, ""), http.StatusOK))

	for _, row := range []struct {
		run    uuid.UUID
		action string
		actor  string
	}{
		{newRun, "run.create", svcPrin},
		{newRun, "workload.launch", svcSponsor},
		{newRun, "workload.replace", svcSponsor},
		{oldRun, "run.kill", svcSponsor},
	} {
		ev := waitForRecAudit(t, e.h.audit, row.run, row.action, "success")
		if ev.Actor != row.actor {
			t.Errorf("%s on %s: actor %s/%s, want %s", row.action, row.run, ev.ActorType, ev.Actor, row.actor)
		}
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
