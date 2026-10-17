// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// A document whose profiles are each other's base cannot be ordered: the apply is refused with the
// cycle named, before anything is written.
func TestApplyGovernanceGapCovRefusesABaseCycleBeforeWriting(t *testing.T) {
	aID := uuid.MustParse("00000000-0000-0000-0000-00000000c0a1")
	bID := uuid.MustParse("00000000-0000-0000-0000-00000000c0b2")
	for name, doc := range map[string]client.GovernanceDocument{
		"two profiles on each other": {Profiles: []client.GovernanceProfile{
			{ID: aID, Name: "alpha", BaseProfileID: &bID},
			{ID: bID, Name: "beta", BaseProfileID: &aID},
		}},
		"a profile on itself": {Profiles: []client.GovernanceProfile{
			{ID: aID, Name: "alpha", BaseProfileID: &aID},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			g := &graphFake{}
			res, err := g.client(t).ApplyGovernanceResult(context.Background(), doc, false)
			if err == nil || !strings.Contains(err.Error(), "is part of a base_profile_id cycle") {
				t.Fatalf("ApplyGovernanceResult err = %v, want the cycle refusal", err)
			}
			if len(g.writes) != 0 || len(res.Pending) != 0 {
				t.Errorf("writes %v, pending %v after a refused document; want none", g.writes, res.Pending)
			}
		})
	}
}

// Prune deletes a stored profile whose base is not among the stored profiles, rather than stalling on
// the missing base.
func TestApplyGovernanceGapCovPruneDeletesAProfileWhoseBaseIsGone(t *testing.T) {
	orphan := client.GovernanceProfile{
		ID: uuid.MustParse("00000000-0000-0000-0000-00000000c0c3"), Name: "orphan",
		BaseProfileID: ptr(uuid.MustParse("00000000-0000-0000-0000-00000000c0d4")),
	}
	g := &graphFake{profiles: []client.GovernanceProfile{orphan}}

	if _, err := g.client(t).ApplyGovernance(context.Background(), client.GovernanceDocument{}, true); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if want := []string{"orphan"}; !reflect.DeepEqual(g.deleted, want) {
		t.Fatalf("deleted = %v, want %v", g.deleted, want)
	}
}
