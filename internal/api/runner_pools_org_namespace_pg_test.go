// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// An organization runner serves a pool in a runs namespace (design T5 §2.7), against a real Postgres.
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func (e *placementEnv) auditWith(action string, match func(map[string]any) bool) map[string]any {
	e.t.Helper()
	for _, ev := range e.h.audit.snapshot() {
		var data map[string]any
		if ev.Action == action && json.Unmarshal(ev.Data, &data) == nil && match(data) {
			return data
		}
	}
	return nil
}

// The add takes an optional body {runs_namespace}: a namespace name is stored, listed with the member and
// audited; anything else is a 400; a namespace the runner already serves for another pool is the
// member-mismatch refusal with its sentence; and a re-add names the namespace the runner already has.
func TestPG_OrgRunnerAddTakesARunsNamespace(t *testing.T) {
	e := newPlacementEnv(t)
	a, b, c := e.createPool("A", types.RunnerPoolOrganization), e.createPool("B", types.RunnerPoolOrganization), e.createPool("C", types.RunnerPoolOrganization)
	cluster := e.orgRunner()

	for _, bad := range []string{`{"runs_namespace":"Team_A"}`, `{"runs_namespace":"` + strings.Repeat("a", 64) + `"}`, `{"runs_namespace":"-a"}`, `{"namespace":"team-a"}`, `{`} {
		if w := e.admin(http.MethodPut, orgRunnerPath(a.ID, cluster), bad); w.Code != http.StatusBadRequest {
			t.Errorf("body %s = %d %s, want 400", bad, w.Code, w.Body)
		}
	}
	if n := e.count(`SELECT count(*) FROM runner_pool_members WHERE runner_id = $1`, cluster); n != 0 {
		t.Fatalf("a refused body left %d memberships", n)
	}

	w := e.admin(http.MethodPut, orgRunnerPath(a.ID, cluster), `{"runs_namespace":"team-a"}`)
	if w.Code != http.StatusCreated || decodeJSON[types.RunnerPoolMember](t, w).RunsNamespace != "team-a" {
		t.Fatalf("add in team-a = %d %s", w.Code, w.Body)
	}
	if w := e.admin(http.MethodPut, orgRunnerPath(b.ID, cluster), `{"runs_namespace":"team-b"}`); w.Code != http.StatusCreated {
		t.Errorf("the same runner in another namespace for another pool = %d %s, want 201", w.Code, w.Body)
	}
	w = e.admin(http.MethodPut, orgRunnerPath(c.ID, cluster), `{"runs_namespace":"team-a"}`)
	if w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != string(runnerpool.ReasonMemberMismatch) || wireError(t, w) != "This runner already serves team-a for another pool." {
		t.Errorf("team-a for a third pool = %d %s, want the namespace refusal", w.Code, w.Body)
	}
	if w := e.admin(http.MethodPut, orgRunnerPath(c.ID, cluster), ""); w.Code != http.StatusUnprocessableEntity || wireError(t, w) != orgRunnerInOtherPoolMsg {
		t.Errorf("the default namespace beside named ones = %d %s, want the one-pool refusal", w.Code, w.Body)
	}
	if w := e.admin(http.MethodPut, orgRunnerPath(a.ID, cluster), `{"runs_namespace":"team-a"}`); w.Code != http.StatusOK {
		t.Errorf("the same add again = %d %s, want 200", w.Code, w.Body)
	}
	if w := e.admin(http.MethodPut, orgRunnerPath(a.ID, cluster), ""); w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != string(runnerpool.ReasonMemberMismatch) ||
		!strings.Contains(wireError(t, w), "serving team-a") {
		t.Errorf("a re-add in the default namespace = %d %s, want the fixed-namespace refusal", w.Code, w.Body)
	}

	w = e.admin(http.MethodGet, "/api/v1/runner-pools/"+a.ID.String()+"/members", "")
	var members []client.RunnerPoolMemberView
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &members) != nil || len(members) != 1 || members[0].RunsNamespace != "team-a" {
		t.Errorf("members = %d %s, want one in team-a", w.Code, w.Body)
	}
	attach := e.auditWith(auditPoolRunnerAdd, func(d map[string]any) bool { return d["runs_namespace"] == "team-a" })
	if attach == nil || attach["runner_id"] != cluster.String() {
		t.Errorf("the attach row carries %v, want runs_namespace team-a for the runner", attach)
	}
	if plain := e.auditWith(auditPoolRunnerAdd, func(d map[string]any) bool { _, has := d["runs_namespace"]; return !has }); plain != nil {
		t.Errorf("an attach row without a namespace carries one: %v", plain)
	}
}

// A namespaced membership is eligible only on a runner that advertises the namespace (an older runner
// advertises none: fail closed, the pool has no eligible member), a run it takes carries the namespace in
// its placement audit and in the sandbox spec dispatch creates, and a membership removed before the create
// refuses it.
func TestPG_OrgRunnerNamespaceGatesPlacementAndDispatch(t *testing.T) {
	e := newPlacementEnv(t)
	org := e.createPool("Cluster", types.RunnerPoolOrganization)
	cluster := e.orgRunner()
	if w := e.admin(http.MethodPut, orgRunnerPath(org.ID, cluster), `{"runs_namespace":"team-a"}`); w.Code != http.StatusCreated {
		t.Fatalf("add = %d %s", w.Code, w.Body)
	}
	body := runBody(`"placement":"local","runner_pool_id":"` + org.ID.String() + `"`)

	if w := e.call(e.person, http.MethodPost, "/api/v1/runs", body); w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != string(runnerpool.ReasonNoEligibleMember) {
		t.Fatalf("a runner advertising no namespace = %d %s, want runner_pool_no_eligible_member", w.Code, w.Body)
	}
	facts := e.runner.runnerFacts[cluster.String()]
	facts.RunsNamespaces = []string{"wardyn-runs", "team-b"}
	e.runner.runnerFacts[cluster.String()] = facts
	if w := e.call(e.person, http.MethodPost, "/api/v1/runs", body); w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != string(runnerpool.ReasonNoEligibleMember) {
		t.Fatalf("a runner advertising other namespaces = %d %s, want runner_pool_no_eligible_member", w.Code, w.Body)
	}
	facts.RunsNamespaces = []string{"wardyn-runs", "team-a"}
	e.runner.runnerFacts[cluster.String()] = facts
	e.stream(cluster, types.CC1, types.CC2, types.CC3)

	run, code, refused := e.launchStored(body)
	if code != http.StatusCreated || run.RunnerID == nil || *run.RunnerID != cluster {
		t.Fatalf("a runner advertising team-a = %d %s, run on %v", code, refused, run.RunnerID)
	}
	if got := e.dispatchedSpec(run.ID).Namespace; got != "team-a" {
		t.Errorf("dispatch created in namespace %q, want team-a", got)
	}
	created := e.auditWith("run.create", func(d map[string]any) bool { return d["runner_id"] == cluster.String() })
	if created == nil || created["runs_namespace"] != "team-a" {
		t.Errorf("the run.create row carries %v, want runs_namespace team-a", created)
	}

	if w := e.admin(http.MethodDelete, orgRunnerPath(org.ID, cluster), ""); w.Code != http.StatusNoContent {
		t.Fatalf("remove = %d %s", w.Code, w.Body)
	}
	stored, err := e.pg.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.srv.createSandboxAtTarget(context.Background(), stored, runner.SandboxSpec{RunID: run.ID}); !errors.Is(err, runner.ErrExecutorUnavailable) {
		t.Errorf("a create after the membership was removed = %v, want ErrExecutorUnavailable", err)
	}

	// Added again under a namespace the runner does not advertise: the create refuses it, never sends it.
	if w := e.admin(http.MethodPut, orgRunnerPath(org.ID, cluster), `{"runs_namespace":"team-c"}`); w.Code != http.StatusCreated {
		t.Fatalf("re-add = %d %s", w.Code, w.Body)
	}
	if _, err := e.srv.createSandboxAtTarget(context.Background(), stored, runner.SandboxSpec{RunID: run.ID}); !errors.Is(err, runner.ErrExecutorUnavailable) {
		t.Errorf("a create in a namespace the runner does not serve = %v, want ErrExecutorUnavailable", err)
	}
}

// The default namespace needs nothing advertised and sends none: today's behaviour.
func TestPG_OrgRunnerDefaultNamespaceNeedsNoAdvertisement(t *testing.T) {
	e := newPlacementEnv(t)
	org := e.createPool("Cluster", types.RunnerPoolOrganization)
	cluster := e.orgRunner()
	if w := e.admin(http.MethodPut, orgRunnerPath(org.ID, cluster), ""); w.Code != http.StatusCreated {
		t.Fatalf("add = %d %s", w.Code, w.Body)
	}
	e.stream(cluster, types.CC1, types.CC2, types.CC3)
	run, code, refused := e.launchStored(runBody(`"placement":"local","runner_pool_id":"` + org.ID.String() + `"`))
	if code != http.StatusCreated || run.RunnerID == nil {
		t.Fatalf("launch = %d %s", code, refused)
	}
	if got := e.dispatchedSpec(run.ID).Namespace; got != "" {
		t.Errorf("dispatch created in namespace %q, want the runner's default", got)
	}
}
