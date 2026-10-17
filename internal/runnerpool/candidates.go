// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerpool

import (
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// OwnRunnerCandidates is the only way a self-hosted pool yields execution
// candidates: the claimed runners of members that principal owns, in a stable
// order. A pool lists many people's runners, and none of the others is ever a
// candidate for this person's run, whatever role the person holds. Liveness,
// capacity and every other admission fact are checked afresh on each candidate
// by the caller; membership is only the starting set.
func OwnRunnerCandidates(principal string, members []types.RunnerPoolMember, runners []types.Runner) []uuid.UUID {
	if principal == "" {
		return nil
	}
	owned := map[uuid.UUID]bool{}
	for _, r := range runners {
		if r.Owner == principal && r.OwnerKind != types.RunnerOwnerOrg && r.State == types.RunnerClaimed {
			owned[r.ID] = true
		}
	}
	var out []uuid.UUID
	for _, m := range members {
		if m.RunnerID != nil && owned[*m.RunnerID] && !slices.Contains(out, *m.RunnerID) {
			out = append(out, *m.RunnerID)
		}
	}
	slices.SortFunc(out, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	return out
}

// OrgRunnerCandidates is how an organization pool yields its own registered runners: the claimed
// members the organization owns (owner_kind org, no owner), in a stable order. A person's runner is
// never one, whatever pool lists it. Admission facts are checked afresh by the caller, as for
// OwnRunnerCandidates.
func OrgRunnerCandidates(members []types.RunnerPoolMember, runners []types.Runner) []uuid.UUID {
	org := map[uuid.UUID]bool{}
	for _, r := range runners {
		if r.OwnerKind == types.RunnerOwnerOrg && r.Owner == "" && r.State == types.RunnerClaimed {
			org[r.ID] = true
		}
	}
	var out []uuid.UUID
	for _, m := range members {
		if m.RunnerID != nil && org[*m.RunnerID] && !slices.Contains(out, *m.RunnerID) {
			out = append(out, *m.RunnerID)
		}
	}
	slices.SortFunc(out, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	return out
}
