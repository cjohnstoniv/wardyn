// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_GovernanceChanges_HeldUnitDiffShowsMode: the approver of a held unit write reads the mode it
// asks for in the diff: a held pass-through create shows it after, and a held switch from own_walls
// shows it before and after.
func TestPG_GovernanceChanges_HeldUnitDiffShowsMode(t *testing.T) {
	def := store.DefaultUnitID.String()
	modes := func(t *testing.T, ch types.GovernanceChange) (before, after string) {
		t.Helper()
		var diff struct {
			Before *struct {
				Mode string `json:"mode"`
			} `json:"before"`
			After struct {
				Mode string `json:"mode"`
			} `json:"after"`
		}
		if err := json.Unmarshal(ch.Diff, &diff); err != nil {
			t.Fatalf("diff %s: %v", ch.Diff, err)
		}
		if diff.Before != nil {
			before = diff.Before.Mode
		}
		return before, diff.After.Mode
	}

	t.Run("create", func(t *testing.T) {
		e := newGovEnv(t)
		ch := e.pending(e.call(e.alice, http.MethodPost, "/api/v1/governance/units",
			`{"name":"held-pt","mode":"pass_through","parent_id":"`+def+`","overlay":{}}`))
		if before, after := modes(t, ch); before != "" || after != types.UnitModePassThrough {
			t.Errorf("held create diff mode = before %q after %q, want none and pass_through: %s", before, after, ch.Diff)
		}
	})

	t.Run("switch", func(t *testing.T) {
		e := newGovEnv(t)
		// The unit narrows its base, so dropping the narrowing is a loosening and is held.
		def0, denied := store.DefaultUnitID, []string{"evil.example"}
		own, err := e.pg.UpsertGovernanceUnit(context.Background(), types.GovernanceUnit{
			Name: "held-switch", CreatedBy: "seed", ParentID: &def0, Overlay: &types.CeilingOverlay{DeniedDomains: &denied},
		})
		if err != nil || own.Mode != types.UnitModeOwnWalls {
			t.Fatalf("seeded unit = mode %q (%v), want own_walls", own.Mode, err)
		}
		ch := e.pending(e.call(e.alice, http.MethodPut, "/api/v1/governance/units/"+own.ID.String(),
			`{"name":"held-switch","mode":"pass_through","parent_id":"`+def+`","overlay":{}}`))
		if before, after := modes(t, ch); before != types.UnitModeOwnWalls || after != types.UnitModePassThrough {
			t.Errorf("held switch diff mode = before %q after %q, want own_walls and pass_through: %s", before, after, ch.Diff)
		}
	})
}
