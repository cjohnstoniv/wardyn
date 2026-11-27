// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_RestrictionUserTypes: the user types restrictions name as subjects, each once and sorted; group,
// user and everyone subjects are not types.
func TestPG_RestrictionUserTypes(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(runsPGPoolIsolated(t))
	if got, err := pg.RestrictionUserTypes(ctx); err != nil || len(got) != 0 {
		t.Fatalf("no restrictions: %v %v, want none", got, err)
	}
	userType := func(id string) types.RestrictionMember {
		return types.RestrictionMember{SubjectType: types.CapabilitySubjectUserType, Subject: id}
	}
	for _, r := range []types.Restriction{
		restrictionNamed("rut-a", userType("engineer"), userType("contractor")),
		restrictionNamed("rut-b", userType("contractor"), userMember("contractor-sub")),
		restrictionNamed("rut-c", types.RestrictionMember{SubjectType: types.CapabilitySubjectGroup, Subject: "analyst"}),
		restrictionNamed("rut-d", types.RestrictionMember{SubjectType: types.CapabilitySubjectAll}),
	} {
		if _, err := pg.PutRestriction(ctx, r, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := pg.RestrictionUserTypes(ctx)
	if err != nil || !slices.Equal(got, []string{"contractor", "engineer"}) {
		t.Errorf("RestrictionUserTypes = %v %v, want [contractor engineer]", got, err)
	}
}
