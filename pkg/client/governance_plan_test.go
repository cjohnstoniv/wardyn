// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// governancePlanFixture is a current state of two profiles ("keep" and "edit")
// and a document that keeps "keep", changes "edit" and adds "fresh".
func governancePlanFixture() (current, doc client.GovernanceDocument) {
	keepID, editID := uuid.New(), uuid.New()
	keep := client.GovernanceProfile{ID: keepID, Name: "keep",
		Ceiling: client.RunPolicySpec{MinConfinementClass: client.CC1}}
	edit := client.GovernanceProfile{ID: editID, Name: "edit",
		Ceiling: client.RunPolicySpec{MinConfinementClass: client.CC1},
		Limits:  client.GovernanceLimits{MaxConcurrentRuns: 1}}
	current = client.GovernanceDocument{Profiles: []client.GovernanceProfile{keep, edit}}
	wanted := edit
	wanted.Limits = client.GovernanceLimits{MaxConcurrentRuns: 4}
	doc = client.GovernanceDocument{Profiles: []client.GovernanceProfile{
		keep,
		wanted,
		{Name: "fresh"},
	}}
	return current, doc
}

func TestPlanGovernance_ListsCreatesUpdatesAndUnchangedWithoutWriting(t *testing.T) {
	current, doc := governancePlanFixture()
	g, c := newGovServer(t, current)
	plan, err := c.PlanGovernance(context.Background(), doc, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 2 {
		t.Fatalf("steps = %+v, want the edit update then the fresh create (apply's order)", plan.Steps)
	}
	update, create := plan.Steps[0], plan.Steps[1]
	if update.Kind != "profile" || update.Op != "update" || update.Name != "edit" ||
		!slices.Equal(update.Fields, []string{"limits"}) || update.ID == nil || *update.ID != current.Profiles[1].ID {
		t.Fatalf("update step = %+v, want profile update of edit changing limits", update)
	}
	if create.Kind != "profile" || create.Op != "create" || create.Name != "fresh" {
		t.Fatalf("create step = %+v, want profile create of fresh", create)
	}
	if plan.Unchanged != 1 || plan.Destructive {
		t.Fatalf("unchanged = %d, destructive = %v; want 1 and false", plan.Unchanged, plan.Destructive)
	}
	if len(g.writes) != 0 {
		t.Fatalf("a plan issued writes: %v", g.writes)
	}
}

func TestPlanGovernance_PruneNamesEveryDeleteAndMarksDestructive(t *testing.T) {
	keepID, dropID, asgKeep, asgDrop := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	current := client.GovernanceDocument{
		Profiles: []client.GovernanceProfile{{ID: keepID, Name: "kept"}, {ID: dropID, Name: "dropped"}},
		Assignments: []client.GovernanceAssignment{
			{ID: asgKeep, SubjectType: "user", Subject: "a", ProfileID: keepID},
			{ID: asgDrop, SubjectType: "user", Subject: "b", ProfileID: dropID},
		},
	}
	doc := client.GovernanceDocument{
		Profiles:    []client.GovernanceProfile{{ID: keepID, Name: "kept"}},
		Assignments: []client.GovernanceAssignment{{SubjectType: "user", Subject: "a", ProfileID: keepID}},
	}
	g, c := newGovServer(t, current)

	plan, err := c.PlanGovernance(context.Background(), doc, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 2 {
		t.Fatalf("steps = %+v, want exactly the assignment delete then the profile delete", plan.Steps)
	}
	asg, prof := plan.Steps[0], plan.Steps[1]
	if asg.Kind != "assignment" || asg.Op != "delete" || asg.Subject != "b" || asg.ID == nil || *asg.ID != asgDrop ||
		asg.Profile != "dropped" {
		t.Fatalf("assignment delete = %+v, want user b on dropped", asg)
	}
	if prof.Kind != "profile" || prof.Op != "delete" || prof.Name != "dropped" || prof.ID == nil || *prof.ID != dropID {
		t.Fatalf("profile delete = %+v, want dropped", prof)
	}
	if !plan.Destructive {
		t.Fatal("a plan with deletes is not marked destructive")
	}
	if len(g.writes) != 0 {
		t.Fatalf("a prune plan issued writes: %v", g.writes)
	}

	plan, err = c.PlanGovernance(context.Background(), doc, false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Destructive {
		t.Fatalf("without prune the plan is destructive: %+v", plan.Steps)
	}
	for _, s := range plan.Steps {
		if s.Op == "delete" {
			t.Fatalf("without prune the plan has a delete: %+v", s)
		}
	}
}

// TestPlanGovernance_AgreesWithApply holds the plan to the writes apply makes:
// against the same state, each create and update step is one write of the
// same method and kind, in the same order.
func TestPlanGovernance_AgreesWithApply(t *testing.T) {
	current, doc := governancePlanFixture()
	doc.Assignments = []client.GovernanceAssignment{
		{SubjectType: "user", Subject: "alice", ProfileID: doc.Profiles[1].ID},
	}
	planServer, planClient := newGovServer(t, current)
	plan, err := planClient.PlanGovernance(context.Background(), doc, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(planServer.writes) != 0 {
		t.Fatalf("a plan issued writes: %v", planServer.writes)
	}

	applyServer, applyClient := newGovServer(t, current)
	if _, err := applyClient.ApplyGovernanceResult(context.Background(), doc, false); err != nil {
		t.Fatal(err)
	}

	var planned []string
	for _, s := range plan.Steps {
		switch {
		case s.Kind == "profile" && s.Op == "create":
			planned = append(planned, "POST profile")
		case s.Kind == "profile" && s.Op == "update":
			planned = append(planned, "PUT profile")
		case s.Kind == "assignment":
			planned = append(planned, "POST assignment")
		}
	}
	var applied []string
	for _, w := range applyServer.writes {
		switch {
		case strings.HasPrefix(w, "POST /api/v1/governance/profiles"):
			applied = append(applied, "POST profile")
		case strings.HasPrefix(w, "PUT /api/v1/governance/profiles/"):
			applied = append(applied, "PUT profile")
		case w == "POST /api/v1/governance/assignments":
			applied = append(applied, "POST assignment")
		default:
			t.Fatalf("apply issued an unexpected write %q", w)
		}
	}
	if !slices.Equal(planned, applied) {
		t.Fatalf("plan steps %v, apply writes %v; want them to agree", planned, applied)
	}
}

// A composed profile whose base was renamed on the server: apply creates the base
// afresh and re-points the profile at it with a write, so the plan lists that
// write too, and the two agree.
func TestPlanGovernance_RenamedBaseRepointsTheComposedProfile(t *testing.T) {
	baseID, composedID := uuid.New(), uuid.New()
	renamed := client.GovernanceProfile{ID: baseID, Name: "platform-base", Ceiling: client.RunPolicySpec{MinConfinementClass: client.CC1}}
	composed := client.GovernanceProfile{ID: composedID, Name: "team", BaseProfileID: &baseID}
	current := client.GovernanceDocument{Profiles: []client.GovernanceProfile{renamed, composed}}
	// The exported file still calls the base by its old name.
	oldName := renamed
	oldName.Name = "base"
	doc := client.GovernanceDocument{Profiles: []client.GovernanceProfile{oldName, composed}}

	planServer, planClient := newGovServer(t, current)
	plan, err := planClient.PlanGovernance(context.Background(), doc, false)
	if err != nil {
		t.Fatal(err)
	}
	var team *client.ApplyStep
	for i, s := range plan.Steps {
		if s.Kind == "profile" && s.Name == "team" {
			team = &plan.Steps[i]
		}
	}
	if team == nil || team.Op != "update" || !slices.Equal(team.Fields, []string{"base_profile_id"}) {
		t.Fatalf("plan = %+v, want an update of team re-pointing its base", plan)
	}
	if plan.Unchanged != 0 || len(planServer.writes) != 0 {
		t.Fatalf("unchanged = %d writes = %v", plan.Unchanged, planServer.writes)
	}

	applyServer, applyClient := newGovServer(t, current)
	if _, err := applyClient.ApplyGovernanceResult(context.Background(), doc, false); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(applyServer.writes, "PUT /api/v1/governance/profiles/"+composedID.String()) {
		t.Fatalf("apply writes = %v, want the PUT of team the plan listed", applyServer.writes)
	}
}
