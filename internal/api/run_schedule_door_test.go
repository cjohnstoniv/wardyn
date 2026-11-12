// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// seedScheduleUnit stores a profile with the given limits and returns it. Called again with the
// returned unit's ID, it rewrites that unit's limits.
func (e *queueEnv) seedScheduleUnit(id uuid.UUID, limits types.GovernanceLimits) types.GovernanceUnit {
	e.t.Helper()
	u, err := e.pg.UpsertGovernanceUnit(context.Background(), types.GovernanceUnit{
		ID: id, Name: "Schedulers", CreatedBy: "seed", Limits: limits,
		Ceiling: types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com", "github.com"}, MinConfinementClass: types.CC2},
	})
	if err != nil {
		e.t.Fatalf("seed profile: %v", err)
	}
	return u
}

// TestScheduledDoorRefusesAtCreate: a member whose profile denies scheduled runs is refused a
// start time with 403 scheduled_runs_denied and an authz.denied row, with nothing written; the
// same member still starts a run now, and a profile that does not deny schedules as before.
func TestScheduledDoorRefusesAtCreate(t *testing.T) {
	e := newQueueEnv(t)
	t.Setenv(envScheduledRuns, "1")
	pool := e.cappedPool("Night", 5)
	unit := e.seedScheduleUnit(uuid.Nil, types.GovernanceLimits{DenyScheduledRuns: true})
	e.seedAssignment(unit, e.personP)

	_, w := e.launch(e.person, pool, scheduleAt(time.Hour))
	if w.Code != http.StatusForbidden || errorReason(w) != "scheduled_runs_denied" {
		t.Fatalf("scheduled launch under a denying profile = %d %s; want 403 scheduled_runs_denied", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "Schedulers") {
		t.Errorf("the refusal %s does not name the profile", w.Body)
	}
	if n := e.count(`SELECT count(*) FROM agent_runs WHERE runner_pool_id = $1`, pool.ID); n != 0 {
		t.Errorf("a refused scheduled launch wrote %d runs", n)
	}
	var denied int
	for _, ev := range e.h.audit.snapshot() {
		if ev.Action == "authz.denied" && ev.Target == "runs.schedule" { // the target is the row's own column, not its data
			denied++
		}
	}
	if denied != 1 {
		t.Errorf("authz.denied rows for runs.schedule = %d, want 1", denied)
	}
	if _, w := e.launch(e.person, pool, ""); w.Code >= 300 {
		t.Errorf("an unscheduled launch under the same profile = %d %s; the door closes only start times", w.Code, w.Body)
	}

	e.seedScheduleUnit(unit.ID, types.GovernanceLimits{})
	if _, w := e.launch(e.person, pool, scheduleAt(time.Hour)); w.Code != http.StatusCreated {
		t.Errorf("scheduled launch under a profile that does not deny = %d %s; want 201", w.Code, w.Body)
	}
}

// TestScheduledDoorEndsBookedRun: a run booked before the admin closed the door ends FAILED
// scheduled_runs_denied at its start, with the door's own sentence, and mints nothing.
func TestScheduledDoorEndsBookedRun(t *testing.T) {
	e := newQueueEnv(t)
	t.Setenv(envScheduledRuns, "1")
	pool := e.cappedPool("Night", 5)
	unit := e.seedScheduleUnit(uuid.Nil, types.GovernanceLimits{})
	e.seedAssignment(unit, e.personP)
	out := e.queuedLaunch(e.person, pool, scheduleAt(time.Hour))

	e.seedScheduleUnit(unit.ID, types.GovernanceLimits{DenyScheduledRuns: true})
	e.due(out.ID)
	e.sweep()
	reason := string(types.RunEndScheduledRunsDenied)
	e.ended(out.ID, queueStartRecheck{reason: reason, sent: types.RunEndScheduledRunsDenied.Sentence()})
}

// TestScheduledDoorOperatorExempt: an operator's start time passes the door, and a member's does not.
func TestScheduledDoorOperatorExempt(t *testing.T) {
	s := New(Config{Store: &capStore{}, Audit: &recRecorder{}})
	denying := types.GovernanceLimits{DenyScheduledRuns: true}
	ceiling := governanceCeiling{Limits: denying, Unit: &ResolvedUnit{ID: uuid.New(), Name: "walled", Limits: denying}}
	if rf := s.scheduleDoorRefusal(operatorCtx("op", "op@corp.example", oidc.RoleAdmin), ceiling); rf != nil {
		t.Errorf("an operator's start time was refused: %+v", rf)
	}
	rf := s.scheduleDoorRefusal(memberCtxFor("alice"), ceiling)
	if rf == nil || rf.decision == nil || rf.status != http.StatusForbidden || rf.body.Reason != "scheduled_runs_denied" {
		t.Fatalf("a member's start time = %+v; want 403 scheduled_runs_denied with a decision", rf)
	}
	if rf := s.scheduleDoorRefusal(memberCtxFor("alice"), governanceCeiling{}); rf != nil {
		t.Errorf("a member under no denying profile was refused: %+v", rf)
	}
}

// TestScheduledDoorQueuedStartOperatorExempt: the queued start asks the door with the owner's bound
// authority, so a booked run an operator owns starts under a ceiling that denies scheduled runs,
// and one a member owns ends scheduled_runs_denied. A run with no start time is never held to it.
func TestScheduledDoorQueuedStartOperatorExempt(t *testing.T) {
	s := New(Config{Store: &capStore{}, Audit: &recRecorder{}})
	denying := types.GovernanceLimits{DenyScheduledRuns: true}
	ceiling := governanceCeiling{Limits: denying, Unit: &ResolvedUnit{ID: uuid.New(), Name: "walled", Limits: denying}}
	booked := time.Now().Add(time.Hour)
	start := func(a queueAuthority, startAt *time.Time) *queuedStart {
		st := &queuedStart{auth: a}
		st.entry.StartAt = startAt
		st.ctx = bindAuthority(context.Background(), a, contextChoice{})
		return st
	}
	operator := queueAuthority{Sub: "op", Email: "op@corp.example", Role: oidc.RoleAdmin}
	member := queueAuthority{Sub: "alice", Email: "alice@corp.example", Role: oidc.RoleUser}

	if end := s.queueScheduleDoor(start(operator, &booked), ceiling); end != nil {
		t.Errorf("an operator's booked run ended %+v; operators are exempt", end)
	}
	end := s.queueScheduleDoor(start(member, &booked), ceiling)
	if end == nil || end.token != string(types.RunEndScheduledRunsDenied) || end.hint != types.RunEndScheduledRunsDenied.Sentence() {
		t.Errorf("a member's booked run = %+v; want the scheduled_runs_denied end", end)
	}
	if end := s.queueScheduleDoor(start(member, nil), ceiling); end != nil {
		t.Errorf("a member's run with no start time ended %+v", end)
	}
}
