// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// wallPresetColumn is agent_runs.wall_preset's own shape: the frozen copy with every field, the spec
// included, under ordinary tags (types.WallPresetOnRun has none, and the run read never sends it).
type wallPresetColumn struct {
	ID      string                  `json:"id"`
	Name    string                  `json:"name"`
	Scope   types.WallPresetScope   `json:"scope"`
	GroupID string                  `json:"group_id,omitempty"`
	Version int                     `json:"version"`
	Spec    types.PersonalWallsSpec `json:"spec"`
}

// wallPresetJSON is the wall_preset column's value: NULL for a run with none.
func wallPresetJSON(p *types.WallPresetOnRun) ([]byte, error) {
	if p == nil {
		return nil, nil
	}
	raw, err := json.Marshal(wallPresetColumn{ID: p.ID, Name: p.Name, Scope: p.Scope, GroupID: p.GroupID, Version: p.Version, Spec: p.Spec})
	if err != nil {
		return nil, fmt.Errorf("store: marshal run wall preset: %w", err)
	}
	return raw, nil
}

// scanRunWallPreset reads the wall_preset column back: nil for NULL. A copy that does not decode is an
// error, never no preset, so a run is never read without the walls it was launched under.
func scanRunWallPreset(raw []byte) (*types.WallPresetOnRun, error) {
	if raw == nil {
		return nil, nil
	}
	var c wallPresetColumn
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("store: unmarshal run wall preset: %w", err)
	}
	return &types.WallPresetOnRun{ID: c.ID, Name: c.Name, Scope: c.Scope, GroupID: c.GroupID, Version: c.Version, Spec: c.Spec}, nil
}
