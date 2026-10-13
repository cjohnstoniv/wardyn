// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A resolved ceiling inherits the deployment's size where its profile sets none; the AUTHORED size the
// member cap reads (#1949) keeps that apart: a standalone row's own size, an overlay's narrowing of it,
// and nothing for a chain no row of which sets one.
func TestResolvedProfileCarriesTheAuthoredSize(t *testing.T) {
	srv := &Server{cfg: Config{DefaultPolicy: types.RunPolicySpec{Resources: &types.ResourceLimits{CPUMillis: 1000, MemoryMiB: 2048}}}}
	n := func(v int) *int { return &v }
	sized := types.GovernanceProfile{ID: uuid.New(), Name: "sized", Ceiling: types.RunPolicySpec{Resources: &types.ResourceLimits{CPUMillis: 3000}}}
	bare := types.GovernanceProfile{ID: uuid.New(), Name: "bare"}
	narrowed := types.GovernanceProfile{ID: uuid.New(), Name: "narrowed", BaseProfileID: &sized.ID,
		Overlay: &types.CeilingOverlay{Resources: &types.ResourcesOverlay{CPUMillis: n(2500)}}}
	composedBare := types.GovernanceProfile{ID: uuid.New(), Name: "composed", BaseProfileID: &bare.ID,
		Overlay: &types.CeilingOverlay{}}
	for _, tc := range []struct {
		name      string
		chain     []types.GovernanceProfile
		authored  types.ResourceLimits
		inherited int
	}{
		{"a standalone row's own size; memory inherited", []types.GovernanceProfile{sized}, types.ResourceLimits{CPUMillis: 3000}, 3000},
		{"a row with no size authors none", []types.GovernanceProfile{bare}, types.ResourceLimits{}, 1000},
		{"an overlay's size is the meet's", []types.GovernanceProfile{narrowed, sized}, types.ResourceLimits{CPUMillis: 2500}, 2500},
		{"a composed chain that sets none authors none", []types.GovernanceProfile{composedBare, bare}, types.ResourceLimits{}, 1000},
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
