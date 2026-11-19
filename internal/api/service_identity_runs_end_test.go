// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

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
