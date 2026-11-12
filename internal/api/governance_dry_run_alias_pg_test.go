// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestDryRunMembershipMoveListsAliasOwnedRun: a user membership that names its person by a sign-in
// email alias, not their sub, still puts that person's live run in the move's live_runs, with no wall
// edit. A run of an unrelated owner and an ended run of the alias owner stay out.
func TestDryRunMembershipMoveListsAliasOwnedRun(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	ctx := context.Background()
	from := e.seedProfile("al-from", "pypi.org")
	to := e.seedProfile("al-to", "github.com")
	if _, err := e.pg.UpsertLoginIdentity(ctx, store.LoginIdentity{
		Principal: "sub-al-owner", Issuer: "https://dex.example", Email: "Al.Owner@corp.example"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	byAlias := treeMember(t, e, from, types.CapabilitySubjectUser, "al.owner@corp.example", 1, false)
	treeMember(t, e, from, types.CapabilitySubjectUser, "sub-al-direct", 1, false)
	seed := func(owner string, state types.RunState) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := e.pg.CreateRun(ctx, types.AgentRun{ID: id, CreatedBy: owner, Agent: "claude-code", ConfinementClass: types.CC2,
			State: state, RunnerTarget: "docker", Task: "t", SPIFFEID: "spiffe://wardyn.local/agent-run/" + id.String(),
			SandboxRef: "wardyn-agent-" + id.String(), UnitID: &from.ID}); err != nil {
			t.Fatalf("seed run of %s: %v", owner, err)
		}
		return id
	}
	alias := seed("sub-al-owner", types.RunRunning)
	direct := seed("sub-al-direct", types.RunRunning)
	seed("sub-al-stranger", types.RunRunning)
	seed("sub-al-owner", types.RunCompleted)

	w := e.admin(http.MethodPost, "/api/v1/governance/dry-run",
		`{"target":"membership_move","id":"`+byAlias.ID.String()+`","unit_id":"`+to.ID.String()+`"}`)
	var got treeDryRunReport
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil {
		t.Fatalf("dry run = %d %s", w.Code, w.Body)
	}
	if !slices.Contains(got.LiveRuns, alias) {
		t.Errorf("live_runs = %v, want the alias owner's run %s", got.LiveRuns, alias)
	}
	// Only the moved membership counts: the other member's run is not its owner's.
	if slices.Contains(got.LiveRuns, direct) || len(got.LiveRuns) != 1 {
		t.Errorf("live_runs = %v, want only %s (not the other member's, a stranger's or an ended run)", got.LiveRuns, alias)
	}
}

// TestUnitWritePassThroughNamesItsParent: a pass-through unit with no parent is refused
// governance_unit_request_invalid whether or not the body carries an overlay, and the Default unit as
// a pass-through unit is the read-only refusal, not a cycle on itself.
func TestUnitWritePassThroughNamesItsParent(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	for name, body := range map[string]string{
		"overlay absent":       `{"name":"pt-orphan-a","mode":"pass_through"}`,
		"overlay present":      `{"name":"pt-orphan-b","mode":"pass_through","overlay":{}}`,
		"overlay, null parent": `{"name":"pt-orphan-c","mode":"pass_through","parent_id":null,"overlay":{}}`,
	} {
		w := e.admin(http.MethodPost, "/api/v1/governance/units", body)
		if w.Code != http.StatusBadRequest || wireReason(t, w) != reasonGovernanceUnitRequestInvalid || !strings.Contains(w.Body.String(), passThroughTopLevelMsg) {
			t.Errorf("%s: POST = %d %s, want 400 %s", name, w.Code, w.Body, reasonGovernanceUnitRequestInvalid)
		}
	}
	// The Default unit is read-only at the door, before any shape or cycle check can name it.
	w := e.admin(http.MethodPut, "/api/v1/governance/units/"+store.DefaultUnitID.String(),
		`{"name":"Default","mode":"pass_through","parent_id":null,"overlay":{}}`)
	if w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceUnitDefaultReadonly {
		t.Errorf("Default as pass_through = %d %s, want 409 %s (not %s)", w.Code, w.Body, reasonGovernanceUnitDefaultReadonly, reasonGovernanceUnitCycle)
	}
}
