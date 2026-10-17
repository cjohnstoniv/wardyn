// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerpool

import (
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// An organization pool's own-runner candidates are its claimed members the organization owns; a
// person's runner listed there (a forged row) and an unclaimed org runner are never candidates, and an
// org runner is never a person's own candidate.
func TestOrgRunnerCandidatesAreOnlyTheOrganizations(t *testing.T) {
	org, unclaimed, persons := uuid.New(), uuid.New(), uuid.New()
	runners := []types.Runner{
		{ID: org, OwnerKind: types.RunnerOwnerOrg, State: types.RunnerClaimed},
		{ID: unclaimed, OwnerKind: types.RunnerOwnerOrg, State: types.RunnerUnclaimed},
		{ID: persons, Owner: "alice", OwnerKind: types.RunnerOwnerPerson, State: types.RunnerClaimed},
	}
	var members []types.RunnerPoolMember
	for _, id := range []uuid.UUID{org, unclaimed, persons, org} {
		members = append(members, types.RunnerPoolMember{RunnerID: &id})
	}
	if got := OrgRunnerCandidates(members, runners); !slices.Equal(got, []uuid.UUID{org}) {
		t.Errorf("OrgRunnerCandidates = %v, want only %s", got, org)
	}
	if got := OwnRunnerCandidates("alice", members, runners); !slices.Equal(got, []uuid.UUID{persons}) {
		t.Errorf("OwnRunnerCandidates = %v, want only alice's runner", got)
	}
}
