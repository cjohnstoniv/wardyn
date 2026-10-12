// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// The two placement tokens a pool row may hold are the placement package's, so the list and a launch
// on the pool answer one token.
func TestAvailabilityPlacementTokensAreThePlacementOnes(t *testing.T) {
	if runnerpool.AvailabilityPlacementDenied != runnerpool.Reason(placement.ReasonPlacementDenied) ||
		runnerpool.AvailabilityPlacementUnavailable != runnerpool.Reason(placement.ReasonPlacementUnavailable) {
		t.Fatal("the pool row's placement tokens drifted from placement.Reason")
	}
}

func classes(cs ...types.ConfinementClass) runner.TargetFacts { return runner.TargetFacts{Classes: cs} }

// barrierRow is each option's reason ("" when available) and which one is the default.
func barrierRow(opts []client.BarrierOption) (reasons [3]runnerpool.BarrierReason, def types.ConfinementClass) {
	for i, o := range opts {
		reasons[i] = o.ReasonCode
		if o.Default {
			def = o.Class
		}
	}
	return reasons, def
}

// T12: a pool's barriers by the governance floor, the pool's own limits and its known candidates' facts.
func TestBarrierOptions(t *testing.T) {
	remote := types.RunnerPool{HostingType: types.RunnerPoolRemoteProvided}
	onlyWall := remote
	onlyWall.Limits = &types.RunnerPoolLimits{Barriers: []types.ConfinementClass{types.CC1, types.CC2}}
	all := classes(types.CC1, types.CC2, types.CC3)
	cases := []struct {
		name    string
		pool    types.RunnerPool
		targets []poolTarget
		floor   types.ConfinementClass
		want    [3]runnerpool.BarrierReason
		def     types.ConfinementClass
	}{
		{"floor CC2", remote, []poolTarget{{id: "a", facts: all}}, types.CC2,
			[3]runnerpool.BarrierReason{runnerpool.BarrierBelowFloor, "", ""}, types.CC3},
		{"the pool allows CC1 and CC2", onlyWall, []poolTarget{{id: "a", facts: all}}, types.CC1,
			[3]runnerpool.BarrierReason{"", "", runnerpool.BarrierPoolNotAllowed}, types.CC2},
		{"no candidate offers CC3", remote, []poolTarget{{id: "a", facts: classes(types.CC1)}, {id: "b", facts: classes(types.CC1, types.CC2)}}, types.CC1,
			[3]runnerpool.BarrierReason{"", "", runnerpool.BarrierNotOffered}, types.CC2},
		{"every candidate offline", remote, []poolTarget{{id: "a", offline: true, facts: all}}, types.CC1,
			[3]runnerpool.BarrierReason{runnerpool.BarrierUnknown, runnerpool.BarrierUnknown, runnerpool.BarrierUnknown}, ""},
		{"an offline candidate's classes are not offered", remote, []poolTarget{{id: "a", offline: true, facts: all}, {id: "b", facts: classes(types.CC1)}}, types.CC1,
			[3]runnerpool.BarrierReason{"", runnerpool.BarrierNotOffered, runnerpool.BarrierNotOffered}, types.CC1},
		{"the floor wins over the pool", onlyWall, nil, types.CC2,
			[3]runnerpool.BarrierReason{runnerpool.BarrierBelowFloor, runnerpool.BarrierUnknown, runnerpool.BarrierPoolNotAllowed}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := runnerpool.BarrierOptions(barrierFacts(tc.pool, tc.targets, tc.floor, ""))
			if len(opts) != 3 || opts[0].Class != types.CC1 || opts[1].Class != types.CC2 || opts[2].Class != types.CC3 {
				t.Fatalf("options %+v, want CC1, CC2, CC3", opts)
			}
			got, def := barrierRow(opts)
			if got != tc.want || def != tc.def {
				t.Errorf("reasons %v default %q, want %v default %q", got, def, tc.want, tc.def)
			}
			for _, o := range opts {
				if o.Available != (o.ReasonCode == "") || (o.ReasonCode == runnerpool.BarrierBelowFloor) != (o.Floor == tc.floor && o.Floor != "") {
					t.Errorf("option %+v: availability or floor disagrees with its reason", o)
				}
				if o.Default && o.DefaultSource != runnerpool.BarrierDefaultServer {
					t.Errorf("default %+v names no source", o)
				}
			}
		})
	}
}

// kvm_missing is said only of a remote pool whose every executor without Vault is the Docker one.
func TestBarrierKVMMissingOnlyForTheDockerExecutor(t *testing.T) {
	remote := types.RunnerPool{HostingType: types.RunnerPoolRemoteProvided}
	if kvmExplainsNoVault(remote, []poolTarget{{executor: dockerExecutor, facts: classes(types.CC1)}, {executor: "k8s", facts: classes(types.CC1)}}) {
		t.Error("a non-Docker executor without Vault is explained by KVM")
	}
	if kvmExplainsNoVault(types.RunnerPool{HostingType: types.RunnerPoolSelfHosted}, []poolTarget{{executor: dockerExecutor, facts: classes(types.CC1)}}) {
		t.Error("a self-hosted pool's runners are explained by this host's KVM")
	}
}

// T13: Review's barriers carry this draft's policy floor and the Vault its access needs; the list has
// neither.
func TestPreflightBarrierOptionsDraftFloors(t *testing.T) {
	pool := types.RunnerPool{HostingType: types.RunnerPoolRemoteProvided}
	ceiling := governanceCeiling{Spec: types.RunPolicySpec{MinConfinementClass: types.CC1}}
	place := runPlacement{pooled: true, row: pool, targets: []poolTarget{{id: "a", facts: classes(types.CC1, types.CC2, types.CC3)}}}

	policy := types.RunPolicySpec{MinConfinementClass: types.CC2}
	opts := draftBarrierOptions(place, ceiling, policy, composer.Baseline{})
	if got, def := barrierRow(opts); got != [3]runnerpool.BarrierReason{runnerpool.BarrierPolicyFloor, "", ""} || def != types.CC3 || opts[0].Floor != types.CC2 {
		t.Errorf("policy min CC2: %+v", opts)
	}

	access := types.RunPolicySpec{MinConfinementClass: types.CC1, EligibleGrants: []types.GrantSpec{{Kind: types.GrantCloudSTS}}}
	opts = draftBarrierOptions(place, ceiling, access, composer.Baseline{})
	if got, def := barrierRow(opts); got != [3]runnerpool.BarrierReason{runnerpool.BarrierAccessFloor, runnerpool.BarrierAccessFloor, ""} || def != types.CC3 {
		t.Errorf("a write-capable grant: %+v", opts)
	}

	if opts := draftBarrierOptions(runPlacement{}, ceiling, policy, composer.Baseline{}); opts == nil || len(opts) != 0 {
		t.Errorf("an unpooled run has barrier options %v, want []", opts)
	}
}
