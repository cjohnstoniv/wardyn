// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// roomyRunner gives the Mac per-run limits above the base's minimum, so a size under it is the run's own
// doing and the refusal is the size sentence, never the runner's.
func roomyRunner(r *types.Runner) {
	r.Limits = &types.RunnerLimits{PerRun: types.RunnerAmounts{CPUMillis: 8000, MemoryMiB: 16384}}
}

// capped is a field a limit cut: asked for more, got the cap, which source's cap it was.
func capped(asked, got int, src types.LimitSource, knob runnerpool.CapKnob) macOSSizeWhy {
	return macOSSizeWhy{
		Applied: client.AppliedAmount{Requested: asked, Applied: got, Capped: true, Source: src},
		Limit:   types.EffectiveLimit{Cap: types.LimitAmount{Value: got}, CapSource: src},
		Knob:    knob,
	}
}

// defaulted is a field that asked for nothing and got a source's default.
func defaulted(got int, src types.LimitSource) macOSSizeWhy {
	return macOSSizeWhy{
		Applied: client.AppliedAmount{Applied: got, Source: src},
		Limit:   types.EffectiveLimit{Default: types.LimitAmount{Value: got}, Cap: types.LimitAmount{Unlimited: true}, DefaultSource: src},
	}
}

const (
	sizeNeed = "A macOS VM needs at least 4 CPU and 8192 MiB. "
	cpuCeil  = "your governance profile Platform caps it with its ceiling's `resources.cpu_millis`, set on it or on a profile it inherits from. An admin raises that; `limits.max_cpu_millis` only lowers it."
	memCeil  = "your governance profile Platform caps it with its ceiling's `resources.memory_mib`, set on it or on a profile it inherits from. An admin raises that; `limits.max_memory_mib` only lowers it."
)

// A size under the base's minimum names what lowered it and the setting that raises it (MAC-Z01 to Z07, Z09),
// the leaf profile only; a run that asked for less itself keeps the plain minimum (MAC-Z08).
func TestPlacementCapability_MacOSVMSizeNamesItsCause(t *testing.T) {
	for _, tc := range []struct {
		name string
		ask  macOSVMAsk
		want string
	}{
		{"ceiling caps CPU", macOSVMAsk{CPUMillis: 1000, MemoryMiB: 8192, Profile: "Platform", CPU: capped(8000, 1000, types.LimitSourceGovernance, runnerpool.CapKnobCeiling)},
			sizeNeed + "This run gets 1 CPU: " + cpuCeil},
		{"limits cap CPU", macOSVMAsk{CPUMillis: 2000, MemoryMiB: 8192, Profile: "Platform", CPU: capped(8000, 2000, types.LimitSourceGovernance, runnerpool.CapKnobLimits)},
			sizeNeed + "This run gets 2 CPU: your governance profile Platform caps it with `limits.max_cpu_millis`. An admin raises that."},
		{"the policy's size holds CPU", macOSVMAsk{CPUMillis: 2000, MemoryMiB: 8192, Profile: "Platform", CPU: capped(8000, 2000, types.LimitSourceGovernance, runnerpool.CapKnobBaseline)},
			sizeNeed + "This run gets 2 CPU: your governance profile Platform keeps a run to the size its policy sets, or to the default size when it sets none. An admin raises that with the policy's `resources.cpu_millis`."},
		{"ceiling caps memory", macOSVMAsk{CPUMillis: 4000, MemoryMiB: 4096, Profile: "Platform", Memory: capped(16384, 4096, types.LimitSourceGovernance, runnerpool.CapKnobCeiling)},
			sizeNeed + "This run gets 4096 MiB: " + memCeil},
		{"limits cap memory", macOSVMAsk{CPUMillis: 4000, MemoryMiB: 4096, Profile: "Platform", Memory: capped(16384, 4096, types.LimitSourceGovernance, runnerpool.CapKnobLimits)},
			sizeNeed + "This run gets 4096 MiB: your governance profile Platform caps it with `limits.max_memory_mib`. An admin raises that."},
		{"the pool caps it", macOSVMAsk{CPUMillis: 2000, MemoryMiB: 8192, Pool: "Mac developers", CPU: capped(8000, 2000, types.LimitSourcePool, "")},
			sizeNeed + "This run gets 2 CPU: the pool Mac developers caps each run at that. An admin raises the pool's per-run limits."},
		{"the server caps both", macOSVMAsk{CPUMillis: 2000, MemoryMiB: 4096, CPU: capped(8000, 2000, types.LimitSourceDeployment, ""), Memory: capped(16384, 4096, types.LimitSourceDeployment, "")},
			sizeNeed + "This run gets 2 CPU and 4096 MiB: this server's limits cap it. Ask an operator to raise them."},
		{"a default under the minimum, from governance", macOSVMAsk{CPUMillis: 1000, MemoryMiB: 8192, Profile: "Platform", CPU: defaulted(1000, types.LimitSourceGovernance)},
			sizeNeed + "This run gets 1 CPU, the default size from your governance profile Platform. Ask for at least the minimum in the run's `resources`, or set it on Review."},
		{"a default under the minimum, from the pool", macOSVMAsk{CPUMillis: 1000, MemoryMiB: 8192, Pool: "Mac developers", CPU: defaulted(1000, types.LimitSourcePool)},
			sizeNeed + "This run gets 1 CPU, the default size from the pool Mac developers. Ask for at least the minimum in the run's `resources`, or set it on Review."},
		{"a default under the minimum, on the server", macOSVMAsk{CPUMillis: 1000, MemoryMiB: 8192, CPU: defaulted(1000, types.LimitSourceDeployment)},
			sizeNeed + "This run gets 1 CPU, the default size on this server. Ask for at least the minimum in the run's `resources`, or set it on Review."},
		// Both fields low: one cause shares a sentence and both settings, two causes get one each.
		{"both capped by the ceiling", macOSVMAsk{CPUMillis: 1000, MemoryMiB: 4096, Profile: "Platform",
			CPU: capped(8000, 1000, types.LimitSourceGovernance, runnerpool.CapKnobCeiling), Memory: capped(16384, 4096, types.LimitSourceGovernance, runnerpool.CapKnobCeiling)},
			sizeNeed + "This run gets 1 CPU and 4096 MiB: your governance profile Platform caps it with its ceiling's `resources.cpu_millis` and `resources.memory_mib`, set on it or on a profile it inherits from. An admin raises those; `limits.max_cpu_millis` and `limits.max_memory_mib` only lower them."},
		{"two causes", macOSVMAsk{CPUMillis: 1000, MemoryMiB: 4096, Profile: "Platform", Pool: "Mac developers",
			CPU: capped(8000, 1000, types.LimitSourceGovernance, runnerpool.CapKnobCeiling), Memory: capped(16384, 4096, types.LimitSourcePool, "")},
			sizeNeed + "This run gets 1 CPU: " + cpuCeil + " This run gets 4096 MiB: the pool Mac developers caps each run at that. An admin raises the pool's per-run limits."},
		// The run's own size: it asked for less, or the fold cannot say what lowered it.
		{"the run asked for less", macOSVMAsk{CPUMillis: 2000, MemoryMiB: 8192, Profile: "Platform",
			CPU: macOSSizeWhy{Applied: client.AppliedAmount{Requested: 2000, Applied: 2000}, Limit: types.EffectiveLimit{Cap: types.LimitAmount{Value: 8000}, CapSource: types.LimitSourceGovernance}}},
			"A macOS VM needs at least 4 CPU and 8192 MiB."},
		{"no cause known", macOSVMAsk{CPUMillis: 2000, MemoryMiB: 8192}, "A macOS VM needs at least 4 CPU and 8192 MiB."},
		{"governance cap with no profile name", macOSVMAsk{CPUMillis: 1000, MemoryMiB: 8192, CPU: capped(8000, 1000, types.LimitSourceGovernance, runnerpool.CapKnobCeiling)},
			"A macOS VM needs at least 4 CPU and 8192 MiB."},
		{"governance cap of no known setting", macOSVMAsk{CPUMillis: 1000, MemoryMiB: 8192, Profile: "Platform", CPU: capped(8000, 1000, types.LimitSourceGovernance, "")},
			"A macOS VM needs at least 4 CPU and 8192 MiB."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := capsServer(&fakeRunner{})
			id := macRunner(t, s, readyMac(), roomyRunner)
			ref := s.placementCapabilityRefusalFor(capsCtx, macReq(id), runPlacement{}, governanceCeiling{}, 0, tc.ask)
			if ref == nil || ref.status != http.StatusUnprocessableEntity || ref.body.Error != tc.want || ref.body.Field != "resources" {
				t.Fatalf("refusal = %+v, want 422 on resources saying %q", ref, tc.want)
			}
		})
	}
}

// The fold hands the sentence its cause: the leaf profile's name and the setting governance's member cap
// came from, read off the ceiling the way the cap was built. Raising limits.max_cpu_millis does nothing
// while the ceiling's own size binds, and the sentence says so.
func TestStepPlacementCapability_MacOSVMSizeReadsTheCeilingsKnob(t *testing.T) {
	s := capsServer(&fakeRunner{})
	id := macRunner(t, s, readyMac(), roomyRunner)
	gov := func(ceilingCPU, maxCPU int) governanceCeiling {
		return governanceCeiling{
			Unit:   &ResolvedUnit{Name: "Platform", AuthoredResources: types.ResourceLimits{CPUMillis: ceilingCPU}},
			Limits: types.GovernanceLimits{MaxCPUMillis: maxCPU},
		}
	}
	for _, tc := range []struct {
		name         string
		ceiling      governanceCeiling
		wantContains string
		wantNot      string
	}{
		{"the ceiling's size binds", gov(1000, 8000), "caps it with its ceiling's `resources.cpu_millis`", ""},
		{"the profile maximum binds", gov(0, 1000), "caps it with `limits.max_cpu_millis`", "its ceiling's"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := macReq(id)
			req.Agent = "claude-code"
			w := httptest.NewRecorder()
			f := &runFold{w: w, r: httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(capsCtx), req: &req, ceiling: tc.ceiling}
			f.place.applied = client.AppliedResources{
				CPUMillis: client.AppliedAmount{Requested: 8000, Applied: 1000, Capped: true, Source: types.LimitSourceGovernance, SourceName: "Platform"},
				MemoryMiB: client.AppliedAmount{Applied: 8192},
			}
			f.place.limits.CPUMillis = types.EffectiveLimit{Cap: types.LimitAmount{Value: 1000}, CapSource: types.LimitSourceGovernance}
			if s.stepPlacementCapability(f, nil) {
				t.Fatal("a 1 CPU size passed a 4 CPU minimum")
			}
			body := w.Body.String()
			if !strings.Contains(body, "This run gets 1 CPU: your governance profile Platform "+tc.wantContains) || (tc.wantNot != "" && strings.Contains(body, tc.wantNot)) {
				t.Fatalf("body %s, want %q", body, tc.wantContains)
			}
		})
	}
}
