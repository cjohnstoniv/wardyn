// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

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
	Changed *types.WallPresetChange `json:"changed,omitempty"`
}

// wallPresetJSON is the wall_preset column's value: NULL for a run with none.
func wallPresetJSON(p *types.WallPresetOnRun) ([]byte, error) {
	if p == nil {
		return nil, nil
	}
	raw, err := json.Marshal(wallPresetColumn{ID: p.ID, Name: p.Name, Scope: p.Scope, GroupID: p.GroupID, Version: p.Version, Spec: p.Spec, Changed: p.Changed})
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
	return &types.WallPresetOnRun{ID: c.ID, Name: c.Name, Scope: c.Scope, GroupID: c.GroupID, Version: c.Version, Spec: c.Spec, Changed: c.Changed}, nil
}

// SetRunWallPresetChange records on a run's wall preset what the preset had become when it ended the
// run queued under it (types.WallPresetOnRun.Changed). A run with no wall preset is left alone.
func (s PG) SetRunWallPresetChange(ctx context.Context, id uuid.UUID, c types.WallPresetChange) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("store: marshal run wall preset change: %w", err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE agent_runs SET wall_preset = jsonb_set(wall_preset, '{changed}', $2::jsonb)
		WHERE id = $1 AND wall_preset IS NOT NULL`, id, raw); err != nil {
		return fmt.Errorf("store: set run wall preset change: %w", err)
	}
	return nil
}
