// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A mount's host path is the super admin's to read, and the risk assessment and the meet's messages
// reach readers below that tier (POST /runs/preflight, GET /governance warnings). Both name the mount
// by its target only, for every reader.

const pinHostPath = "/srv/pin-mount-src"

// TestGrade_MountValueNamesTargetOnly is R12.
func TestGrade_MountValueNamesTargetOnly(t *testing.T) {
	rw := false
	spec := types.RunPolicySpec{MinConfinementClass: types.CC2, WorkspaceMounts: []types.WorkspaceMount{
		{Source: pinHostPath, Target: "/home/agent/ro"}, {Source: pinHostPath, Target: "/home/agent/rw", ReadOnly: &rw}}}
	items := Grade(RunInput{}, spec)
	for field, want := range map[string]string{"workspace_mounts[0]": "/home/agent/ro (ro)", "workspace_mounts[1]": "/home/agent/rw (rw)"} {
		it, ok := find(items, field)
		if !ok || it.Value != want {
			t.Errorf("%s value = %q, want %q", field, it.Value, want)
		}
	}
	for _, it := range items {
		if strings.Contains(it.Value+it.Rationale, pinHostPath) {
			t.Errorf("the risk assessment names a host path: %+v", it)
		}
	}
}

// TestMeetMounts_MessagesNameTargetOnly is R11: the lenient meet's warning and the strict meet's
// refusal both name the target, never the host path.
func TestMeetMounts_MessagesNameTargetOnly(t *testing.T) {
	rw := false
	base := Authority{Ceiling: types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{{Source: "/srv/base", Target: "/home/agent/b"}}}}
	for name, ov := range map[string][]types.WorkspaceMount{
		"not a mount of the base": {{Source: pinHostPath, Target: "/home/agent/a"}},
		"read-only in the base":   {{Source: "/srv/base", Target: "/home/agent/b", ReadOnly: &rw}},
	} {
		_, warns, err := ApplyOverlay(base, overlayOf(types.CeilingOverlay{WorkspaceMounts: &ov}))
		if err != nil {
			t.Fatalf("%s: lenient meet = %v", name, err)
		}
		verr := ValidateOverlay(base, overlayOf(types.CeilingOverlay{WorkspaceMounts: &ov}))
		said := strings.Join(warns, "\n")
		if verr != nil {
			said += "\n" + verr.Error()
		}
		if !strings.Contains(said, name) || !strings.Contains(said, ov[0].Target) {
			t.Errorf("%s: messages %q do not name the target", name, said)
		}
		if strings.Contains(said, ov[0].Source) {
			t.Errorf("%s: messages %q name the host path %s", name, said, ov[0].Source)
		}
	}
}
