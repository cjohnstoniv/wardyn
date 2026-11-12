// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestUnitChangeScopeStrictness: a unit's own update is strict over every unit it touches, the unit
// itself, the units a rename activates or vacates and the new parent of a move; a create is not, and a
// delete, a grant and a deciders change are. A membership change is not.
func TestUnitChangeScopeStrictness(t *testing.T) {
	x, p1, p2, y, z := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	diff := func(beforeParent, afterParent *uuid.UUID, activates, vacates []uuid.UUID) json.RawMessage {
		act := func(ids []uuid.UUID) []unitActivation {
			var out []unitActivation
			for _, id := range ids {
				out = append(out, unitActivation{UnitID: id, UnitName: id.String()})
			}
			return out
		}
		b, err := json.Marshal(governanceDiffBody{
			Before:    mustJSON(map[string]any{"parent_id": beforeParent}),
			After:     mustJSON(map[string]any{"parent_id": afterParent}),
			Changed:   []string{"overlay"},
			Activates: act(activates),
			Vacates:   act(vacates),
		})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, tc := range []struct {
		what       string
		ch         types.GovernanceChange
		wantUnits  []uuid.UUID
		wantStrict bool
		wantOK     bool
	}{
		{"unit update, same parent",
			types.GovernanceChange{TargetKind: govKindUnit, Op: "update", TargetKey: x.String(), Diff: diff(&p1, &p1, nil, nil)},
			[]uuid.UUID{x}, true, true},
		{"unit update, re-parent: the new parent is touched",
			types.GovernanceChange{TargetKind: govKindUnit, Op: "update", TargetKey: x.String(), Diff: diff(&p1, &p2, nil, nil)},
			[]uuid.UUID{x, p2}, true, true},
		{"unit update, a rename that activates and vacates",
			types.GovernanceChange{TargetKind: govKindUnit, Op: "update", TargetKey: x.String(), Diff: diff(&p1, &p1, []uuid.UUID{y}, []uuid.UUID{z})},
			[]uuid.UUID{x, y, z}, true, true},
		{"unit create",
			types.GovernanceChange{TargetKind: govKindUnit, Op: "create", TargetKey: x.String(), Diff: diff(nil, &p1, nil, nil)},
			[]uuid.UUID{p1}, false, true},
		{"unit delete",
			types.GovernanceChange{TargetKind: govKindUnit, Op: "delete", TargetKey: x.String(), Diff: diff(&p1, nil, nil, nil)},
			[]uuid.UUID{x}, true, true},
		{"membership move",
			types.GovernanceChange{TargetKind: govKindMembership, Op: "move", TargetKey: membershipKey(x, types.CapabilitySubjectUser, "sub-a"),
				Payload: mustJSON(map[string]any{"id": uuid.New(), "unit_id": p2}), Diff: diff(nil, nil, nil, nil)},
			[]uuid.UUID{x, p2}, false, true},
		{"unit admin grant",
			types.GovernanceChange{TargetKind: govKindUnitAdmin, Op: "create", TargetKey: membershipKey(x, types.CapabilitySubjectUser, "sub-a"), Diff: diff(nil, nil, nil, nil)},
			[]uuid.UUID{x}, true, true},
		{"unit deciders",
			types.GovernanceChange{TargetKind: govKindUnitDeciders, Op: "update", TargetKey: x.String(), Diff: diff(nil, nil, nil, nil)},
			[]uuid.UUID{x}, true, true},
	} {
		t.Run(tc.what, func(t *testing.T) {
			units, strict, ok := unitChangeScope(tc.ch)
			if ok != tc.wantOK || strict != tc.wantStrict || !slices.Equal(units, tc.wantUnits) {
				t.Errorf("unitChangeScope = %v, strict %v, ok %v; want %v, strict %v, ok %v", units, strict, ok, tc.wantUnits, tc.wantStrict, tc.wantOK)
			}
		})
	}
}
