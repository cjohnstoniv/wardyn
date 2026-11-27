// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package orchestrator

import (
	"context"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// gpuSubstrate is fakeSubstrate reporting the GPU kinds its classes hand in.
type gpuSubstrate struct {
	*fakeSubstrate
	gpu map[types.ConfinementClass][]types.GPUKind
}

func (g *gpuSubstrate) Classes(ctx context.Context) (substrate.ClassSupport, error) {
	cs, err := g.fakeSubstrate.Classes(ctx)
	cs.GPU = g.gpu
	return cs, err
}

// TestCapabilities_GPUComesFromTheRoutedSubstrate pins the per-class merge: both substrates list CC1 and only the
// second offers nvidia, so CC1's GPU is the first's (none): the substrate a class routes to decides, as for Freeze.
func TestCapabilities_GPUComesFromTheRoutedSubstrate(t *testing.T) {
	first := &gpuSubstrate{fakeSubstrate: &fakeSubstrate{name: "first", classes: []types.ConfinementClass{types.CC1}}}
	second := &gpuSubstrate{
		fakeSubstrate: &fakeSubstrate{name: "second", classes: []types.ConfinementClass{types.CC1}},
		gpu:           map[types.ConfinementClass][]types.GPUKind{types.CC1: {types.GPUNvidia}},
	}
	caps, err := New(first, second).Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if got := caps.GPU[types.CC1]; len(got) != 0 {
		t.Errorf("GPU[CC1] = %v, want none: CC1 routes to the first substrate, which offers no GPU", got)
	}

	// Positive control: with the offering substrate first, the kind reaches the capabilities.
	caps, err = New(second, first).Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if !slices.Equal(caps.GPU[types.CC1], []types.GPUKind{types.GPUNvidia}) {
		t.Errorf("GPU[CC1] = %v, want [nvidia]", caps.GPU[types.CC1])
	}
}
