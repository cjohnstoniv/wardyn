// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package orchestrator

import (
	"context"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A substrate's pool lists reach the merged capabilities, and a substrate that has none leaves them as they were.
func TestCapabilities_UnionsUntaintedPools(t *testing.T) {
	docker, k8s := runnertest.NewFake(), runnertest.NewFake()
	docker.Support = substrate.ClassSupport{Classes: []types.ConfinementClass{types.CC1}}
	k8s.Support = substrate.ClassSupport{Classes: []types.ConfinementClass{types.CC1, types.CC3},
		UntaintedPools: []types.ConfinementClass{types.CC3}, AnyNodeClasses: []types.ConfinementClass{types.CC3}}
	caps, err := New(namedFake{Fake: docker, name: "docker"}, namedFake{Fake: k8s, name: "k8s"}).Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if !slices.Equal(caps.UntaintedPools, []types.ConfinementClass{types.CC3}) {
		t.Errorf("UntaintedPools = %v, want [CC3]", caps.UntaintedPools)
	}
	if !slices.Equal(caps.AnyNodeClasses, []types.ConfinementClass{types.CC3}) {
		t.Errorf("AnyNodeClasses = %v, want [CC3]", caps.AnyNodeClasses)
	}
}
