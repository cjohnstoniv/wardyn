// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// svcEndStore is the SCIM fake with service identities: what a suspend, a retire and a sponsor's
// suspension ask of them.
type svcEndStore struct {
	*scimCovStore
	store.ServiceIdentityStore // only Get, List, Update and Retire are asked
	mu                         sync.Mutex
	sis                        map[string]*types.ServiceIdentity
	// listErr, when set, is what every ListServiceIdentities answers.
	listErr error
	// runsErr is what ListNonTerminalRunsBy answers for a creator, when set.
	runsErr map[string]error
}

func (s *svcEndStore) ListNonTerminalRunsBy(ctx context.Context, createdBy string) ([]types.AgentRun, error) {
	s.mu.Lock()
	err := s.runsErr[createdBy]
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s.scimCovStore.ListNonTerminalRunsBy(ctx, createdBy)
}

func newSvcEndStore(sis ...types.ServiceIdentity) *svcEndStore {
	s := &svcEndStore{scimCovStore: newSCIMCovStore(), sis: map[string]*types.ServiceIdentity{}}
	for _, si := range sis {
		s.sis[si.Name] = &si
	}
	return s
}

func (s *svcEndStore) GetServiceIdentity(_ context.Context, name string) (types.ServiceIdentity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	si, ok := s.sis[name]
	if !ok {
		return types.ServiceIdentity{}, store.ErrNotFound
	}
	return *si, nil
}

func (s *svcEndStore) ListServiceIdentities(_ context.Context, sponsor string, _ store.Page) ([]types.ServiceIdentity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []types.ServiceIdentity
	for _, si := range s.sis {
		if sponsor == "" || si.Sponsor == sponsor {
			out = append(out, *si)
		}
	}
	slices.SortFunc(out, func(a, b types.ServiceIdentity) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (s *svcEndStore) UpdateServiceIdentity(_ context.Context, name string, ch store.ServiceIdentityChange) (store.ServiceIdentityChanged, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	si, ok := s.sis[name]
	switch {
	case !ok:
		return store.ServiceIdentityChanged{}, store.ErrNotFound
	case si.RetiredAt != nil:
		return store.ServiceIdentityChanged{}, store.ErrServiceIdentityInactive
	}
	before := *si
	if ch.Suspended != nil && *ch.Suspended && si.SuspendedAt == nil {
		at := scimCovNow
		si.SuspendedAt, si.SuspendedBy = &at, ch.By
	}
	return store.ServiceIdentityChanged{Before: before, After: *si}, nil
}

func (s *svcEndStore) RetireServiceIdentity(_ context.Context, name, by string) (store.ServiceIdentityChanged, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	si, ok := s.sis[name]
	switch {
	case !ok:
		return store.ServiceIdentityChanged{}, store.ErrNotFound
	case si.RetiredAt != nil:
		return store.ServiceIdentityChanged{Before: *si, After: *si}, store.ErrServiceIdentityInactive
	}
	before := *si
	at := scimCovNow
	si.RetiredAt, si.RetiredBy = &at, by
	return store.ServiceIdentityChanged{Before: before, After: *si}, nil
}

// svcEndCall calls a service identity handler for name as the security admin, its route already matched.
func svcEndCall(e *scimCovEnv, h http.HandlerFunc, method, name, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequest(method, "/api/v1/service-identities/"+name, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", name)
	r = r.WithContext(context.WithValue(withOIDCHuman(r.Context(), "sub-secadmin"), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

// svcEndRuns seeds a live and an ended run of ops-bot and a live run of another identity.
func svcEndRuns(st *svcEndStore) (live, ended, bystander uuid.UUID) {
	return st.addRun("service:ops-bot", types.RunRunning, "sbx-ops"),
		st.addRun("service:ops-bot", types.RunCompleted, "sbx-ops-ended"),
		st.addRun("service:other-bot", types.RunRunning, "sbx-other")
}

// svcEndWant asserts ops-bot's live run is KILLED with one run.kill row naming reason, and nothing else moved.
func svcEndWant(t *testing.T, e *scimCovEnv, live, ended, bystander uuid.UUID, reason, killer string) {
	t.Helper()
	if got := e.st.runState(live); got != types.RunKilled {
		t.Errorf("the identity's live run = %s, want KILLED", got)
	}
	if e.st.runState(ended) != types.RunCompleted || e.st.runState(bystander) != types.RunRunning {
		t.Errorf("an ended run or another identity's run was touched: %s %s", e.st.runState(ended), e.st.runState(bystander))
	}
	var rows []types.AuditEvent
	for _, ev := range e.auditRows("run.kill") {
		if ev.RunID != nil && *ev.RunID == live {
			rows = append(rows, ev)
		}
	}
	if len(rows) != 1 || e.auditData(rows[0])["reason"] != reason || rows[0].Actor != killer || rows[0].Outcome != "success" {
		t.Errorf("run.kill rows for the live run = %+v, want one success by %s naming %s", rows, killer, reason)
	}
}

func svcEndOpsBot() types.ServiceIdentity {
	return types.ServiceIdentity{Name: "ops-bot", Sponsor: "sub-pat"}
}

func svcEndEnv(t *testing.T, st *svcEndStore) *scimCovEnv {
	t.Helper()
	return newSCIMCovEnv(t, st, func(c *Config) { c.Secrets = nil })
}

// Suspending a service identity ends its live runs, as a SCIM suspension ends a person's, and a second
// suspend finds none left.
func TestServiceIdentitySuspendEndsItsRuns(t *testing.T) {
	st := newSvcEndStore(svcEndOpsBot())
	live, ended, bystander := svcEndRuns(st)
	e := svcEndEnv(t, st)

	w := svcEndCall(e, e.srv.handleUpdateServiceIdentity, http.MethodPatch, "ops-bot", `{"suspended":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH suspended = %d %s", w.Code, w.Body.String())
	}
	svcEndWant(t, e, live, ended, bystander, killReasonServiceSuspended, "sub-secadmin")

	kills := e.runner.killCount()
	if w := svcEndCall(e, e.srv.handleUpdateServiceIdentity, http.MethodPatch, "ops-bot", `{"suspended":true}`); w.Code != http.StatusOK {
		t.Fatalf("second PATCH suspended = %d %s", w.Code, w.Body.String())
	}
	if e.runner.killCount() != kills {
		t.Error("a repeated suspend killed a sandbox again")
	}
}

// A PATCH that does not suspend leaves the identity's runs alone.
func TestServiceIdentityDescriptionChangeLeavesItsRuns(t *testing.T) {
	st := newSvcEndStore(svcEndOpsBot())
	live, _, _ := svcEndRuns(st)
	e := svcEndEnv(t, st)

	if w := svcEndCall(e, e.srv.handleUpdateServiceIdentity, http.MethodPatch, "ops-bot", `{"description":"nightly"}`); w.Code != http.StatusOK {
		t.Fatalf("PATCH description = %d %s", w.Code, w.Body.String())
	}
	if got := e.st.runState(live); got != types.RunRunning {
		t.Errorf("the identity's run = %s after a description change, want RUNNING", got)
	}
}

// Retiring a service identity ends its live runs.
func TestServiceIdentityRetireEndsItsRuns(t *testing.T) {
	st := newSvcEndStore(svcEndOpsBot())
	live, ended, bystander := svcEndRuns(st)
	e := svcEndEnv(t, st)

	if w := svcEndCall(e, e.srv.handleRetireServiceIdentity, http.MethodDelete, "ops-bot", ""); w.Code != http.StatusOK {
		t.Fatalf("DELETE = %d %s", w.Code, w.Body.String())
	}
	svcEndWant(t, e, live, ended, bystander, queueCancelServiceRetired, "sub-secadmin")
}

// A kill that fails answers 5xx, and the same DELETE run again ends the run.
func TestServiceIdentityRetireRetriesAFailedKill(t *testing.T) {
	st := newSvcEndStore(svcEndOpsBot())
	live, _, _ := svcEndRuns(st)
	e := svcEndEnv(t, st)

	st.failNext("ListNonTerminalRunsBy", errSCIMCovBoom, 1)
	if w := svcEndCall(e, e.srv.handleRetireServiceIdentity, http.MethodDelete, "ops-bot", ""); w.Code != http.StatusInternalServerError {
		t.Fatalf("DELETE with the run list failing = %d, want 500", w.Code)
	}
	if got := e.st.runState(live); got != types.RunRunning {
		t.Fatalf("run = %s before the retry, want RUNNING", got)
	}
	if w := svcEndCall(e, e.srv.handleRetireServiceIdentity, http.MethodDelete, "ops-bot", ""); w.Code != http.StatusOK {
		t.Fatalf("retried DELETE = %d %s", w.Code, w.Body.String())
	}
	if got := e.st.runState(live); got != types.RunKilled {
		t.Errorf("run = %s after the retry, want KILLED", got)
	}
}

// A SCIM suspension of the sponsor ends the runs of the identities they sponsor in the same kill step, on
// the same ledger, and person.deprovision counts them.
func TestSCIMSuspensionOfTheSponsorEndsTheServiceRuns(t *testing.T) {
	st := newSvcEndStore(svcEndOpsBot())
	l := scimCovSeedLeaver(st.scimCovStore)
	live, ended, bystander := svcEndRuns(st)
	e := svcEndEnv(t, st)

	w := e.scim(http.MethodPatch, "/scim/v2/Users/"+l.id.String(), scimCovPatch(scimCovReplace("active", "false")))
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH active=false = %d %s", w.Code, w.Body.String())
	}
	svcEndWant(t, e, live, ended, bystander, killReasonServiceSponsorInactive, scimActor)
	if got := e.st.runState(l.runSub); got != types.RunKilled {
		t.Errorf("the sponsor's own run = %s, want KILLED", got)
	}
	if !slices.ContainsFunc(st.ledger(l.id, store.JobKindSuspend), func(j store.DeprovisionJob) bool {
		return j.Step == jobStepKillRun && j.Target == live.String() && j.Done
	}) {
		t.Error("the service run is not a done kill_run row on the sponsor's ledger")
	}
	deprov := e.auditRows("person.deprovision")
	if len(deprov) != 1 || e.auditData(deprov[0])["runs_killed"] != float64(3) {
		t.Errorf("person.deprovision = %+v, want runs_killed 3 (two of the person's, one of the service's)", deprov)
	}
}

// A suspend whose kill failed leaves the identity suspended with its run live; a later PATCH with suspended
// true on the already-suspended identity ends the run the first kill left.
func TestServiceIdentityRepeatSuspendEndsARunTheFirstLeft(t *testing.T) {
	st := newSvcEndStore(svcEndOpsBot())
	live, ended, bystander := svcEndRuns(st)
	e := svcEndEnv(t, st)

	st.failNext("ListNonTerminalRunsBy", errSCIMCovBoom, 1)
	if w := svcEndCall(e, e.srv.handleUpdateServiceIdentity, http.MethodPatch, "ops-bot", `{"suspended":true}`); w.Code != http.StatusInternalServerError {
		t.Fatalf("PATCH suspended with the run list failing = %d, want 500", w.Code)
	}
	if si, _ := st.GetServiceIdentity(context.Background(), "ops-bot"); si.SuspendedAt == nil || e.st.runState(live) != types.RunRunning {
		t.Fatalf("after the failed kill: suspended_at %v, run %s; want suspended with the run still RUNNING", si.SuspendedAt, e.st.runState(live))
	}
	if w := svcEndCall(e, e.srv.handleUpdateServiceIdentity, http.MethodPatch, "ops-bot", `{"suspended":true}`); w.Code != http.StatusOK {
		t.Fatalf("repeat PATCH suspended = %d %s", w.Code, w.Body.String())
	}
	svcEndWant(t, e, live, ended, bystander, killReasonServiceSuspended, "sub-secadmin")
}

// Retiring an identity already suspended, whose suspend left a run live, ends that run.
func TestServiceIdentityRetireOfASuspendedIdentityEndsItsRuns(t *testing.T) {
	st := newSvcEndStore(svcEndOpsBot())
	live, ended, bystander := svcEndRuns(st)
	e := svcEndEnv(t, st)

	st.failNext("ListNonTerminalRunsBy", errSCIMCovBoom, 1)
	if w := svcEndCall(e, e.srv.handleUpdateServiceIdentity, http.MethodPatch, "ops-bot", `{"suspended":true}`); w.Code != http.StatusInternalServerError {
		t.Fatalf("PATCH suspended with the run list failing = %d, want 500", w.Code)
	}
	if w := svcEndCall(e, e.srv.handleRetireServiceIdentity, http.MethodDelete, "ops-bot", ""); w.Code != http.StatusOK {
		t.Fatalf("DELETE of the suspended identity = %d %s", w.Code, w.Body.String())
	}
	svcEndWant(t, e, live, ended, bystander, queueCancelServiceRetired, "sub-secadmin")
}

// A sponsor's suspension that cannot read the identities they sponsor still ends the person's own runs; the
// step stays pending, the request answers a retry, and the retry ends the service's run.
func TestSCIMSuspensionEndsThePersonsRunsWhenTheSponsoredListFails(t *testing.T) {
	st := newSvcEndStore(svcEndOpsBot())
	l := scimCovSeedLeaver(st.scimCovStore)
	live, _, _ := svcEndRuns(st)
	e := svcEndEnv(t, st)
	path, body := "/scim/v2/Users/"+l.id.String(), scimCovPatch(scimCovReplace("active", "false"))

	st.mu.Lock()
	st.listErr = errSCIMCovBoom
	st.mu.Unlock()
	scimCovWantRetry(t, e.scim(http.MethodPatch, path, body), "secret-dsn")
	for name, id := range map[string]uuid.UUID{"sub": l.runSub, "email": l.runEmail} {
		if got := e.st.runState(id); got != types.RunKilled {
			t.Errorf("the person's run under the %s = %s with the sponsored list failing, want KILLED", name, got)
		}
	}
	if got := e.st.runState(live); got != types.RunRunning {
		t.Fatalf("the service's run = %s before the retry, want RUNNING", got)
	}
	if n := len(e.auditRows("person.deprovision")); n != 0 {
		t.Errorf("%d person.deprovision rows before the step finished, want none", n)
	}

	st.mu.Lock()
	st.listErr = nil
	st.mu.Unlock()
	if w := e.scim(http.MethodPatch, path, body); w.Code != http.StatusOK {
		t.Fatalf("retried PATCH active=false = %d %s", w.Code, w.Body.String())
	}
	if got := e.st.runState(live); got != types.RunKilled {
		t.Errorf("the service's run = %s after the retry, want KILLED", got)
	}
}

// An approved suspension whose kill fails returns data naming the failure and the runs still live, which
// the approval's service_identity.update row and answer carry; one that ends every run adds nothing.
func TestApprovedSuspendRunsDataNamesTheRunsLeft(t *testing.T) {
	change := uuid.New()
	req := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/governance/changes/"+change.String()+"/approve", nil)
		return r.WithContext(withOIDCHuman(r.Context(), "sub-approver"))
	}

	st := newSvcEndStore(svcEndOpsBot())
	live, _, _ := svcEndRuns(st)
	e := svcEndEnv(t, st)
	st.scimCovStore.mu.Lock()
	st.casLost = 10 // every kill loses its compare-and-swap
	st.scimCovStore.mu.Unlock()
	data := e.srv.approvedSuspendRunsData(req(), "service:ops-bot", change)
	if data["runs_end_failed"] != true || !slices.Equal(data["runs_left"].([]string), []string{live.String()}) {
		t.Errorf("data = %v, want runs_end_failed and the live run", data)
	}

	st.scimCovStore.mu.Lock()
	st.casLost = 0
	st.scimCovStore.mu.Unlock()
	if data := e.srv.approvedSuspendRunsData(req(), "service:ops-bot", change); data != nil || e.st.runState(live) != types.RunKilled {
		t.Errorf("data = %v, run %s; want nothing added and the run KILLED", data, e.st.runState(live))
	}

	st.failNext("ListNonTerminalRunsBy", errSCIMCovBoom, 1)
	if data := e.srv.approvedSuspendRunsData(req(), "service:ops-bot", change); data["runs_end_failed"] != true || len(data["runs_left"].([]string)) != 0 {
		t.Errorf("data with the run list failing = %v, want runs_end_failed and no run named", data)
	}
}

// auditDecision reports an applied change's runs_end_failed, and the target's row carries it beside the change id.
func TestAuditDecisionReportsRunsNotEnded(t *testing.T) {
	st := newSvcEndStore(svcEndOpsBot())
	e := svcEndEnv(t, st)
	out := types.GovernanceChange{ID: uuid.New(), TargetKind: govKindServiceIdentity, TargetKey: "ops-bot", ProposedBy: "sub-proposer"}
	left := uuid.New().String()
	applied := govApplied{action: "service_identity.update", target: "service:ops-bot", data: map[string]any{"tokens_revoked": 0},
		afterCommit: func() map[string]any {
			return map[string]any{"state": "suspended", "runs_end_failed": true, "runs_left": []string{left}}
		}}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/governance/changes/"+out.ID.String()+"/approve", nil)
	r = r.WithContext(withOIDCHuman(r.Context(), "sub-approver"))

	tokens, runs := e.srv.auditDecision(r, out, applied, true, false, types.ActorHuman, "sub-approver", "", "")
	if tokens || !runs {
		t.Errorf("auditDecision = tokens %v, runs %v; want runs not ended only", tokens, runs)
	}
	rows := e.auditRows("service_identity.update")
	if len(rows) != 1 {
		t.Fatalf("service_identity.update rows = %+v, want one", rows)
	}
	d := e.auditData(rows[0])
	if d["runs_end_failed"] != true || d["change_id"] != out.ID.String() || !slices.Equal(anyStrings(d["runs_left"]), []string{left}) {
		t.Errorf("row data = %v, want runs_end_failed, the change id and the run left", d)
	}
}

// A sponsor's suspension that cannot read one sponsored identity's runs still ends the person's own runs; the
// step stays pending, and the retry ends the service's run.
func TestSCIMSuspensionEndsThePersonsRunsWhenAServiceRunListFails(t *testing.T) {
	st := newSvcEndStore(svcEndOpsBot())
	l := scimCovSeedLeaver(st.scimCovStore)
	live, _, _ := svcEndRuns(st)
	e := svcEndEnv(t, st)
	path, body := "/scim/v2/Users/"+l.id.String(), scimCovPatch(scimCovReplace("active", "false"))

	st.mu.Lock()
	st.runsErr = map[string]error{"service:ops-bot": errSCIMCovBoom}
	st.mu.Unlock()
	scimCovWantRetry(t, e.scim(http.MethodPatch, path, body), "secret-dsn")
	for name, id := range map[string]uuid.UUID{"sub": l.runSub, "email": l.runEmail} {
		if got := e.st.runState(id); got != types.RunKilled {
			t.Errorf("the person's run under the %s = %s with the service's run list failing, want KILLED", name, got)
		}
	}
	if got := e.st.runState(live); got != types.RunRunning {
		t.Fatalf("the service's run = %s before the retry, want RUNNING", got)
	}
	if n := len(e.auditRows("person.deprovision")); n != 0 {
		t.Errorf("%d person.deprovision rows before the step finished, want none", n)
	}

	st.mu.Lock()
	st.runsErr = nil
	st.mu.Unlock()
	if w := e.scim(http.MethodPatch, path, body); w.Code != http.StatusOK {
		t.Fatalf("retried PATCH active=false = %d %s", w.Code, w.Body.String())
	}
	if got := e.st.runState(live); got != types.RunKilled {
		t.Errorf("the service's run = %s after the retry, want KILLED", got)
	}
}

// svcEndTx is the approval transaction applyServiceIdentityChange runs on: every read of the identity
// answers si, and the suspend's UPDATE marks it suspended.
type svcEndTx struct{ si types.ServiceIdentity }

func (q *svcEndTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "suspended_at = clock_timestamp()") {
		at := scimCovNow
		q.si.SuspendedAt, q.si.SuspendedBy = &at, "sub-proposer"
	}
	return pgconn.CommandTag{}, nil
}

func (q *svcEndTx) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errSCIMCovBoom
}

func (q *svcEndTx) QueryRow(context.Context, string, ...any) pgx.Row { return svcEndRow{q.si} }

type svcEndRow struct{ si types.ServiceIdentity }

// Scan fills the eleven columns scanServiceIdentity reads, in its order.
func (r svcEndRow) Scan(dest ...any) error {
	si := r.si
	vals := []any{si.Name, si.Description, si.Sponsor, si.SponsorSince, si.UserType, si.CreatedBy, si.CreatedAt,
		si.SuspendedAt, si.SuspendedBy, si.RetiredAt, si.RetiredBy}
	if len(dest) != len(vals) {
		return errSCIMCovBoom
	}
	for i, d := range dest {
		reflect.ValueOf(d).Elem().Set(reflect.ValueOf(vals[i]))
	}
	return nil
}

// An approved suspension's after-commit work ends the identity's runs, and when it cannot, the data it adds to
// the approval's row (and so its answer) says runs_end_failed and names the run left.
func TestApprovedServiceSuspensionCarriesRunsEndFailed(t *testing.T) {
	st := newSvcEndStore(svcEndOpsBot())
	live, _, _ := svcEndRuns(st)
	e := svcEndEnv(t, st)
	cur := types.ServiceIdentity{Name: "ops-bot", Sponsor: "sub-pat", UserType: "member", CreatedAt: scimCovNow.Add(-time.Hour)}
	ch := types.GovernanceChange{ID: uuid.New(), TargetKind: govKindServiceIdentity, TargetKey: "ops-bot", Op: "update",
		ProposedBy: "sub-proposer", Payload: json.RawMessage(`{"suspended":true}`), BaseHash: computeETag(serviceIdentityBaseOf(cur))}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/governance/changes/"+ch.ID.String()+"/approve", nil)
	r = r.WithContext(withOIDCHuman(r.Context(), "sub-approver"))

	applied, err := applyServiceIdentityChange(e.srv, r, &svcEndTx{si: cur}, ch)
	if err != nil || applied.afterCommit == nil {
		t.Fatalf("apply = %+v, %v; want an applied change with after-commit work", applied, err)
	}
	st.scimCovStore.mu.Lock()
	st.casLost = 10 // every kill loses its compare-and-swap
	st.scimCovStore.mu.Unlock()
	data := applied.afterCommit()
	if data["state"] != types.ServiceIdentitySuspended || data["runs_end_failed"] != true ||
		!slices.Equal(data["runs_left"].([]string), []string{live.String()}) {
		t.Errorf("after-commit data = %v, want suspended with runs_end_failed and the live run", data)
	}
}
