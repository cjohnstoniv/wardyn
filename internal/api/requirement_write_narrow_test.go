// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestApplyWorkspaceRequirements_OnlyAttachedOrEnabledRowsNarrow folds the run
// the way the fold step does, one workspace per call: O's required row makes
// /srv/app writable, then M's optional row, not enabled, is folded. When M is
// only in the run because it stores the directory, it leaves the write alone;
// when the run attaches M, its row leaves it alone too, since a row that grants
// nothing carries no narrowing. An enabled row from a workspace that is not
// attached still decides the mount.
func TestApplyWorkspaceRequirements_OnlyAttachedOrEnabledRowsNarrow(t *testing.T) {
	const path = "/srv/app"
	row := func(level string) map[string]types.WorkspaceRequirement {
		return map[string]types.WorkspaceRequirement{"write:" + path: {Level: level, Provenance: "scan_seeded"}}
	}
	o := types.Workspace{ID: uuid.New(), Requirements: row("required")}
	m := types.Workspace{ID: uuid.New(), Requirements: row("optional")}
	mRequired := types.Workspace{ID: m.ID, Requirements: row("required")}
	srv := New(Config{})
	for _, tc := range []struct {
		name      string
		authored  bool // the mount's read_only as seeded, before any fold
		folded    []types.Workspace
		attached  []types.Workspace
		wantWrite bool
	}{
		{"storing workspace's row not enabled", true, []types.Workspace{o, m}, []types.Workspace{o}, true},
		// An attached row that grants nothing leaves a write another attached workspace granted; only a
		// read_only the run asked for takes it away (TestApplyWorkspaceRequirements_AWriteRowThatGrantsNothing...).
		{"attached workspace's row not enabled", true, []types.Workspace{o, m}, []types.Workspace{o, m}, true},
		{"storing workspace's row required", true, []types.Workspace{mRequired}, nil, true},
		// A mount a policy authored writable, which no attached workspace holds: a
		// storing workspace's row that grants nothing leaves it as the policy wrote it.
		{"policy-authored writable mount, storing row not enabled", false, []types.Workspace{m}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ro := tc.authored
			spec := &types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{{Source: path, Target: "/home/agent/work", ReadOnly: &ro}}}
			for _, ws := range tc.folded {
				srv.applyWorkspaceRequirementsFor(context.Background(), nil, spec, "claude-code", []types.Workspace{ws}, tc.attached, nil)
			}
			if got := !spec.WorkspaceMounts[0].ReadOnlyOrDefault(); got != tc.wantWrite {
				t.Errorf("writable = %v, want %v", got, tc.wantWrite)
			}
		})
	}
}
