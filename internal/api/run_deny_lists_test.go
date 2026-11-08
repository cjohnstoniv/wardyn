// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// agentBody is a Background run into pool whose one harness names provider (empty: the default).
func (e *queueEnv) agentBody(pool types.RunnerPool, provider string, extra string) string {
	tool := `{"id":"claude-code","kind":"harness"`
	if provider != "" {
		tool += `,"model_provider":"` + provider + `"`
	}
	return `{"title":"t","experience":"background","workload":{"kind":"agent_task","agent":"claude-code","task":"t"},` +
		`"tools":[` + tool + `}],"no_repositories_or_drives":true,"runner_pool_id":"` + pool.ID.String() + `"` + extra + `}`
}

// orgComponent stores an organisation component whose one host the test profile already allows.
func (e *queueEnv) orgComponent(name string) uuid.UUID {
	e.t.Helper()
	cs, ok := any(e.pg).(store.ComponentStore)
	if !ok {
		e.t.Fatal("the Postgres store has no component seam")
	}
	id := uuid.New()
	if _, err := cs.CreateComponent(context.Background(), types.Component{ID: id, Name: name,
		Definition: types.ComponentDefinition{Hosts: []string{"github.com"}}}); err != nil {
		e.t.Fatalf("seed component: %v", err)
	}
	return id
}

// TestDenyComponentRefusedAtCreate: a member whose profile lists a component id is refused a run that
// attaches it, 403 component_denied with the profile named and an authz.denied row, and nothing is
// written; the same member attaches a component the list leaves out, and starts a run with none.
func TestDenyComponentRefusedAtCreate(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Wide", 5)
	walled, open := e.orgComponent("Walled Tool"), e.orgComponent("Open Tool")
	unit := e.seedScheduleUnit(uuid.Nil, types.GovernanceLimits{DenyComponents: []string{walled.String()}})
	e.seedAssignment(unit, e.personP)

	attach := func(id uuid.UUID) *httptest.ResponseRecorder {
		return e.call(e.person, http.MethodPost, "/api/v1/runs", e.agentBody(pool, "", `,"components":[{"id":"`+id.String()+`"}]`))
	}
	w := attach(walled)
	if w.Code != http.StatusForbidden || errorReason(w) != "component_denied" {
		t.Fatalf("attaching a denied component = %d %s; want 403 component_denied", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "Schedulers") {
		t.Errorf("the refusal %s does not name the profile", w.Body)
	}
	if n := e.count(`SELECT count(*) FROM agent_runs WHERE runner_pool_id = $1`, pool.ID); n != 0 {
		t.Errorf("a refused launch wrote %d runs", n)
	}
	var denied int
	for _, ev := range e.h.audit.snapshot() {
		if ev.Action == "authz.denied" && ev.Target == "runs.component" { // the target is the row's own column, not its data
			denied++
		}
	}
	if denied != 1 {
		t.Errorf("authz.denied rows for runs.component = %d, want 1", denied)
	}
	if w := attach(open); w.Code != http.StatusCreated {
		t.Errorf("attaching a component the list leaves out = %d %s; want 201", w.Code, w.Body)
	}
	if _, w := e.launch(e.person, pool, ""); w.Code != http.StatusCreated {
		t.Errorf("a launch with no component under the same profile = %d %s; want 201", w.Code, w.Body)
	}
}

// TestDenyComponentEndsQueuedRun: a run queued with a component the profile did not deny ends FAILED
// component_denied at its start once an admin lists that component, with the door's own sentence.
func TestDenyComponentEndsQueuedRun(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	comp := e.orgComponent("Later Walled")
	unit := e.seedScheduleUnit(uuid.Nil, types.GovernanceLimits{})
	e.seedAssignment(unit, e.personP)
	occupant := e.occupy(pool)
	w := e.call(e.person, http.MethodPost, "/api/v1/runs", e.agentBody(pool, "", `,"components":[{"id":"`+comp.String()+`"}]`))
	var queued queuedLaunchAnswer
	if err := json.Unmarshal(w.Body.Bytes(), &queued); w.Code != http.StatusCreated || err != nil || queued.State != types.RunQueued {
		t.Fatalf("launch into a full pool = %d %s; want 201 QUEUED", w.Code, w.Body)
	}

	e.seedScheduleUnit(unit.ID, types.GovernanceLimits{DenyComponents: []string{comp.String()}})
	e.free(occupant)
	e.sweep()
	e.ended(queued.ID, queueStartRecheck{reason: "component_denied"})
}

// TestDenyModelProviderRefusedAtCreate: a profile that lists a provider's uid refuses a run that
// would use it, 403 model_provider_denied; the launch that names the other provider starts.
func TestDenyModelProviderRefusedAtCreate(t *testing.T) {
	e := newQueueEnv(t)
	anthropic, _ := e.twoProviderLanes(e.personP)
	pool := e.cappedPool("Wide", 5)
	unit := e.seedScheduleUnit(uuid.Nil, types.GovernanceLimits{DenyModelProviders: []string{anthropic.UID}})
	e.seedAssignment(unit, e.personP)

	w := e.call(e.person, http.MethodPost, "/api/v1/runs", e.agentBody(pool, anthropic.ID, ""))
	if w.Code != http.StatusForbidden || errorReason(w) != "model_provider_denied" {
		t.Fatalf("a denied provider = %d %s; want 403 model_provider_denied", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "Schedulers") {
		t.Errorf("the refusal %s does not name the profile", w.Body)
	}
	// claude-code's default provider is the denied one too: not naming it changes nothing.
	if w := e.call(e.person, http.MethodPost, "/api/v1/runs", e.agentBody(pool, "", "")); w.Code != http.StatusForbidden || errorReason(w) != "model_provider_denied" {
		t.Errorf("the default provider, denied = %d %s; want 403 model_provider_denied", w.Code, w.Body)
	}
	e.seedScheduleUnit(unit.ID, types.GovernanceLimits{})
	if w := e.call(e.person, http.MethodPost, "/api/v1/runs", e.agentBody(pool, anthropic.ID, "")); w.Code != http.StatusCreated {
		t.Errorf("the same launch with the list emptied = %d %s; want 201", w.Code, w.Body)
	}
}

// TestDenyListOperatorNotExempt: the deny lists are a hard wall. An operator's run is refused as a
// member's is, where the scheduled-runs door lets an operator through.
func TestDenyListOperatorNotExempt(t *testing.T) {
	s := New(Config{Store: &capStore{}, Audit: &recRecorder{}})
	comp := uuid.New()
	limits := types.GovernanceLimits{DenyComponents: []string{comp.String()}, DenyModelProviders: []string{denyProviderUID}}
	ceiling := governanceCeiling{Limits: limits, Unit: &ResolvedUnit{ID: uuid.New(), Name: "walled", Limits: limits}}
	hit := denyCandidates{components: []denyComponent{{id: comp, name: "Tool"}}}
	for _, who := range []struct {
		name string
		rf   *runRefusal
	}{
		{"operator", s.denyListRefusal(operatorCtx("op", "op@corp.example", oidc.RoleAdmin), ceiling, hit)},
		{"member", s.denyListRefusal(memberCtxFor("alice"), ceiling, hit)},
	} {
		if who.rf == nil || who.rf.decision == nil || who.rf.status != http.StatusForbidden || who.rf.body.Reason != "component_denied" {
			t.Errorf("%s = %+v; want 403 component_denied with a decision", who.name, who.rf)
		}
	}
	if rf := s.denyListRefusal(memberCtxFor("alice"), governanceCeiling{}, hit); rf != nil {
		t.Errorf("a member under no list was refused: %+v", rf)
	}
}

const denyProviderUID = "44444444-4444-4444-8444-444444444444"

// TestDenyListsRefuseNonIDsAtWrite: a profile naming something that is not an id is a 400 naming the
// field, so a typo cannot leave a list that denies nothing.
func TestDenyListsRefuseNonIDsAtWrite(t *testing.T) {
	for field, l := range map[string]types.GovernanceLimits{
		"limits.deny_components":      {DenyComponents: []string{"jira-api"}},
		"limits.deny_model_providers": {DenyModelProviders: []string{"corp-gateway"}},
	} {
		if got := governanceLimitsRefusal(l); !strings.HasPrefix(got, field) {
			t.Errorf("governanceLimitsRefusal(%s) = %q; want a refusal naming the field", field, got)
		}
	}
	if got := governanceLimitsRefusal(types.GovernanceLimits{DenyComponents: []string{denyProviderUID}}); got != "" {
		t.Errorf("a well-formed list was refused: %q", got)
	}
}
