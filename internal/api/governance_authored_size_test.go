// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// A resolved ceiling inherits the deployment's size where its profile sets none; the AUTHORED size the
// member cap reads (#1949) keeps that apart: a standalone row's own size, an overlay's narrowing of it,
// and nothing for a chain no row of which sets one.
func TestResolvedUnitCarriesTheAuthoredSize(t *testing.T) {
	srv := &Server{cfg: Config{DefaultPolicy: types.RunPolicySpec{Resources: &types.ResourceLimits{CPUMillis: 1000, MemoryMiB: 2048}}}}
	n := func(v int) *int { return &v }
	sized := types.GovernanceUnit{ID: uuid.New(), Name: "sized", Ceiling: types.RunPolicySpec{Resources: &types.ResourceLimits{CPUMillis: 3000}}}
	bare := types.GovernanceUnit{ID: uuid.New(), Name: "bare"}
	narrowed := types.GovernanceUnit{ID: uuid.New(), Name: "narrowed", ParentID: &sized.ID,
		Overlay: &types.CeilingOverlay{Resources: &types.ResourcesOverlay{CPUMillis: n(2500)}}}
	composedBare := types.GovernanceUnit{ID: uuid.New(), Name: "composed", ParentID: &bare.ID,
		Overlay: &types.CeilingOverlay{}}
	for _, tc := range []struct {
		name      string
		chain     []types.GovernanceUnit
		authored  types.ResourceLimits
		inherited int
	}{
		{"a standalone row's own size; memory inherited", []types.GovernanceUnit{sized}, types.ResourceLimits{CPUMillis: 3000}, 3000},
		{"a row with no size authors none", []types.GovernanceUnit{bare}, types.ResourceLimits{}, 1000},
		{"an overlay's size is the meet's", []types.GovernanceUnit{narrowed, sized}, types.ResourceLimits{CPUMillis: 2500}, 2500},
		{"a composed chain that sets none authors none", []types.GovernanceUnit{composedBare, bare}, types.ResourceLimits{}, 1000},
	} {
		p, err := srv.composeChain(tc.chain)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if p.AuthoredResources != tc.authored || p.Ceiling.Resources == nil || p.Ceiling.Resources.CPUMillis != tc.inherited || p.Ceiling.Resources.MemoryMiB != 2048 {
			t.Errorf("%s: authored %+v ceiling %+v, want authored %+v and ceiling cpu %d / 2048 MiB", tc.name, p.AuthoredResources, p.Ceiling.Resources, tc.authored, tc.inherited)
		}
	}
}

// A composed profile on the deployment's root that narrows the PIDs limit authored that limit, so a cut at it
// is the profile's, not the deployment's: its overlay is what stands between the ask and the deployment.
func TestComposedProfileAuthorsItsPidsLimit(t *testing.T) {
	srv := &Server{cfg: Config{DefaultPolicy: types.RunPolicySpec{Resources: &types.ResourceLimits{CPUMillis: 1000, MemoryMiB: 2048}}}}
	pids := 256
	overlaid := types.GovernanceUnit{ID: uuid.New(), Name: "overlaid",
		Overlay: &types.CeilingOverlay{Resources: &types.ResourcesOverlay{PidsLimit: &pids}}}
	res, err := srv.composeChain([]types.GovernanceUnit{overlaid})
	if err != nil {
		t.Fatal(err)
	}
	source := policySourceRecord{Kind: policyKindStored, Spec: types.RunPolicySpec{Resources: &types.ResourceLimits{PidsLimit: 4096}}}
	got := appliedPids(source, governanceCeiling{Unit: res}, pids)
	want := client.AppliedAmount{Requested: 4096, Applied: pids, Capped: true, Source: types.LimitSourceGovernance, SourceName: "overlaid"}
	if *got != want {
		t.Errorf("pids_limit = %+v, want %+v", *got, want)
	}
}
