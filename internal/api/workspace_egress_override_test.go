// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// profileWithEgress builds a workspace whose scanned profile trusts hosts and
// whose one attachment overrides the given requirement keys.
func profileWithEgress(t *testing.T, egress []string, overrides map[string]string, effective map[string]types.WorkspaceRequirement) types.Workspace {
	t.Helper()
	blob, err := json.Marshal(workspacescan.WorkspaceProfile{EgressDomains: egress})
	if err != nil {
		t.Fatal(err)
	}
	sid := uuid.New()
	return types.Workspace{
		Profile:               blob,
		Attachments:           []types.WorkspaceAttachment{{SourceID: &sid, Overrides: overrides}},
		EffectiveRequirements: effective,
	}
}

// TestUnionWorkspaceEgress_OverrideOffDropsProfileHost pins GAP-EGRESS-5: an
// "off" egress override must close the PROFILE-union path too, not just the
// folded contract row. A host re-added required by the effective contract
// survives (the override lost to a stronger contributor / overlay).
func TestUnionWorkspaceEgress_OverrideOffDropsProfileHost(t *testing.T) {
	// pypi.org overridden off and NOT present in the effective contract -> dropped.
	ws := profileWithEgress(t,
		[]string{"pypi.org", "files.pythonhosted.org"},
		map[string]string{"egress:pypi.org": types.OverrideOff},
		nil)
	spec := &types.RunPolicySpec{}
	unionWorkspaceEgress(spec, []types.Workspace{ws})
	if slices.Contains(spec.AllowedDomains, "pypi.org") {
		t.Fatalf("egress:pypi.org off must be subtracted from the profile union; got %v", spec.AllowedDomains)
	}
	if !slices.Contains(spec.AllowedDomains, "files.pythonhosted.org") {
		t.Fatalf("a non-off profile host must still be unioned; got %v", spec.AllowedDomains)
	}

	// Same override, but the effective contract re-adds pypi.org required -> it
	// survives (some other contributor / the overlay won).
	ws2 := profileWithEgress(t,
		[]string{"pypi.org"},
		map[string]string{"egress:pypi.org": types.OverrideOff},
		map[string]types.WorkspaceRequirement{"egress:pypi.org": {Level: "required"}})
	spec2 := &types.RunPolicySpec{}
	unionWorkspaceEgress(spec2, []types.Workspace{ws2})
	if !slices.Contains(spec2.AllowedDomains, "pypi.org") {
		t.Fatalf("a host re-added required by the folded contract is NOT off and must survive; got %v", spec2.AllowedDomains)
	}
}
