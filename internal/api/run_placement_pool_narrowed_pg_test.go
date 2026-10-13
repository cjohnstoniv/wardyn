// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.

import (
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// RP-2 r2 NB: a user pool holding only a remote-hosted runner whose people policy does not admit the person
// reads unavailable (no_eligible_member) in the pool list, and the launch into it is refused. The list does not
// judge the pool by the membership row alone, so it agrees with placement once the policy narrows.
func TestPG_PoolReadsUnavailableWhenItsOnlyRemoteMemberDoesNotAdmitThePerson(t *testing.T) {
	e := newPlacementEnv(t)
	pool := e.createOwnPool(e.person, "Borrowed")
	e.setPeople("alpha", everyonePeople)
	if w := e.call(e.person, http.MethodPut, myRemotePath(pool.ID, "alpha"), ""); w.Code != http.StatusCreated {
		t.Fatalf("add alpha = %d %s", w.Code, w.Body)
	}
	if got := e.listed(e.person)[pool.ID]; got.Availability != client.RunnerPoolAvailable {
		t.Fatalf("while admitted the list reads %s %s, want available", got.Availability, got.Reason)
	}
	e.setPeople("alpha", `{"people_pools":"listed","subjects":[{"subject_type":"user","subject":"sub-someone-else"}]}`)
	if got := e.listed(e.person)[pool.ID]; got.Availability != client.RunnerPoolUnavailable || got.Reason != runnerpool.ReasonNoEligibleMember {
		t.Errorf("not admitted: the list reads %s %s, want unavailable %s", got.Availability, got.Reason, runnerpool.ReasonNoEligibleMember)
	}
	sameEverywhere(t, "not admitted", e.answers(e.person, runBody(`"runner_pool_id":"`+pool.ID.String()+`"`)), http.StatusUnprocessableEntity, string(runnerpool.ReasonNoEligibleMember))
}
