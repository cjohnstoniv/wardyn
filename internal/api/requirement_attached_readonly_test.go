// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestRequirementFold_AttachedReadOnlySelectionBeatsAStoringWorkspacesRequiredWrite: an attached
// workspace O selected read_only, plus a referenced, non-attached older workspace M on the same directory
// whose write row is required. The mount ends read-only whichever of the two is folded first and whether
// or not the mount was authored read-only.
func TestRequirementFold_AttachedReadOnlySelectionBeatsAStoringWorkspacesRequiredWrite(t *testing.T) {
	const path = "/srv/app"
	row := func(level string) map[string]types.WorkspaceRequirement {
		return map[string]types.WorkspaceRequirement{"write:" + path: {Level: level, Provenance: "scan_seeded"}}
	}
	yes := true
	for _, oLevel := range []string{"required", "optional"} {
		dir := []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: path}}
		o := types.Workspace{ID: uuid.New(), Requirements: row(oLevel), Sources: dir}
		m := types.Workspace{ID: uuid.New(), Requirements: row("required"), Sources: dir}
		selections := map[string]client.WorkspaceSelection{o.ID.String(): {ReadOnly: &yes}}
		for _, order := range [][]types.Workspace{{o, m}, {m, o}} {
			for _, authoredRO := range []bool{true, false} {
				ro := authoredRO
				spec := &types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{{Source: path, Target: "/home/agent/work", ReadOnly: &ro}}}
				srv := New(Config{})
				for _, ws := range order {
					if _, refusal := srv.applyWorkspaceRequirementsFor(context.Background(), nil, spec, "claude-code", []types.Workspace{ws}, []types.Workspace{o}, selections, nil); refusal != nil {
						t.Fatalf("O row %s, O first %v, authored read_only %v: the fold refused: %v", oLevel, ws.ID == o.ID, authoredRO, refusal)
					}
				}
				if !spec.WorkspaceMounts[0].ReadOnlyOrDefault() {
					t.Errorf("O row %s, folded O-first=%v, authored read_only=%v: the mount is writable", oLevel, order[0].ID == o.ID, authoredRO)
				}
			}
		}
	}
}
