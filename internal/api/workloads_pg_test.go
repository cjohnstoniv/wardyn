// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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

func decodeIntent(t *testing.T, raw string) client.TemplateIntent {
	t.Helper()
	var i client.TemplateIntent
	if err := json.Unmarshal([]byte(raw), &i); err != nil {
		t.Fatal(err)
	}
	return i
}
