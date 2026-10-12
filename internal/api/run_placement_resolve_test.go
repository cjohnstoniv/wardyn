// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func target(id string, offline bool, advertised bool, cpu, mem int64, classes ...types.ConfinementClass) poolTarget {
	return poolTarget{id: id, executor: id, offline: offline,
		facts: runner.TargetFacts{Classes: classes, CapacityAdvertised: advertised, CPUMillisMax: cpu, MemoryMiBMax: mem}}
}

func ids(ts []poolTarget) []string {
	out := []string{}
	for _, t := range ts {
		out = append(out, t.id)
	}
	return out
}

func demand(floor, req types.ConfinementClass, cpu, mem int64) placementDemand {
	return placementDemand{floor: types.RunPolicySpec{MinConfinementClass: floor}, reqCC: req, reqCCOK: true, baseline: composer.Baseline{}, cpu: cpu, mem: mem}
}

// DONE WHEN 3: a heterogeneous pool cannot widen the barrier or the size. Each candidate is held to the
// demand on its OWN facts: the pool's strongest barrier and its largest machine are never joined into a
// target that does not exist.
func TestPlacement_PoolIntersectionNeverWidens(t *testing.T) {
	strongSmall := target("strong-small", false, true, 1000, 1024, types.CC1, types.CC2, types.CC3)
	weakBig := target("weak-big", false, true, 8000, 16384, types.CC1)
	midMid := target("mid-mid", false, true, 4000, 8192, types.CC1, types.CC2)
	pool := []poolTarget{strongSmall, weakBig, midMid}

	for _, tc := range []struct {
		name   string
		d      placementDemand
		want   []string
		reason placement.Reason
	}{
		{"barrier alone keeps the targets that offer it", demand(types.CC2, types.CC3, 0, 0), []string{"strong-small"}, ""},
		{"size alone keeps the targets that fit it, weak ones failing the floor", demand(types.CC2, "", 3000, 4096), []string{"mid-mid"}, ""},
		{"the union of the two would satisfy both; no single target does", demand(types.CC2, types.CC3, 3000, 4096), nil, placement.ReasonPlacementCapability},
		{"a size no target offers", demand(types.CC1, "", 0, 100000), nil, placement.ReasonPlacementCapability},
		{"an unasked-for run keeps every target that meets the floor", demand(types.CC1, "", 0, 0), []string{"strong-small", "weak-big", "mid-mid"}, ""},
	} {
		got, reason := eligibleTargets(pool, tc.d)
		if len(ids(got)) != len(tc.want) || (len(tc.want) == 0 && reason != tc.reason) {
			t.Errorf("%s: eligible %v (%q), want %v (%q)", tc.name, ids(got), reason, tc.want, tc.reason)
			continue
		}
		for i, id := range tc.want {
			if got[i].id != id {
				t.Errorf("%s: eligible %v, want %v", tc.name, ids(got), tc.want)
			}
		}
	}
}

// The evidence classification follows the placement the pool serves and nothing a member advertises: a
// local pool is runner_asserted and a remote one substrate, and the class a run lands at must be one the
// bound target itself can enforce.
func TestPlacement_PoolBindingNeverWidensEvidenceOrBarrier(t *testing.T) {
	runA, runB := uuid.New(), uuid.New()
	pool := uuid.New()
	local := runPlacement{pooled: true, pool: types.ResolvedRunnerPool{ID: pool, Name: "Laptops", Scope: types.RunnerPoolUser, Revision: 3, Selection: types.RunnerPoolSelectedPersonal},
		placement: types.PlacementLocal, filled: true,
		eligible: []poolTarget{
			{id: runA.String(), runnerID: &runA, facts: runner.TargetFacts{Classes: []types.ConfinementClass{types.CC1}}},
			{id: runB.String(), runnerID: &runB, facts: runner.TargetFacts{Classes: []types.ConfinementClass{types.CC1, types.CC3}}}}}
	for range 10 {
		run := types.AgentRun{ID: uuid.New()}
		if r := local.apply(&run, types.CC3); r != nil {
			t.Fatalf("apply = %v", r)
		}
		if run.RunnerID == nil || *run.RunnerID != runB {
			t.Fatalf("a CC3 run bound to %v: only %v enforces CC3", run.RunnerID, runB)
		}
		if run.Placement != types.PlacementLocal || !run.PlacementFilled || run.EvidenceSource != types.RunEvidenceRunnerAsserted ||
			run.RunnerPoolID == nil || *run.RunnerPoolID != pool || run.RunnerPoolRevision != 3 || run.RunnerPoolSelection != types.RunnerPoolSelectedPersonal || run.RunnerPoolExecutor != "" {
			t.Fatalf("binding = %+v", run)
		}
	}
	if r := local.apply(&types.AgentRun{ID: uuid.New()}, types.CC2); r == nil || r.body.Reason != string(placement.ReasonPlacementCapability) {
		t.Fatalf("no bound target enforces CC2, got %v", r)
	}

	remote := runPlacement{pooled: true, pool: types.ResolvedRunnerPool{ID: pool, Name: "Remote", Scope: types.RunnerPoolOrganization, Selection: types.RunnerPoolSelectedOrg},
		placement: types.PlacementRemote, eligible: []poolTarget{target("docker", false, false, 0, 0, types.CC2)}}
	run := types.AgentRun{ID: uuid.New()}
	if r := remote.apply(&run, types.CC2); r != nil || run.RunnerID != nil || run.RunnerPoolExecutor != "docker" || run.EvidenceSource != types.RunEvidenceSubstrate || run.PlacementFilled {
		t.Fatalf("remote binding = %+v (%v)", run, r)
	}
}

// DONE WHEN 4: a candidate whose facts are unknown or that is offline refuses; it is never read as zero
// (a bound of nothing) or as everything (no bound).
func TestPlacement_OfflineCandidateRefuses(t *testing.T) {
	offline := target("a", true, true, 0, 0)
	online := target("b", false, true, 4000, 8192, types.CC1, types.CC2)

	got, reason := eligibleTargets([]poolTarget{offline}, demand(types.CC1, "", 0, 0))
	if len(got) != 0 || reason != placement.ReasonRunnerOffline {
		t.Fatalf("only an offline target: %v %q", ids(got), reason)
	}
	got, reason = eligibleTargets([]poolTarget{offline, online}, demand(types.CC1, "", 0, 0))
	if reason != placement.ReasonRunnerOffline || len(got) != 1 || got[0].id != "b" {
		t.Fatalf("offline beside online: %v %q", ids(got), reason)
	}
	// A runner that advertises a bound of nothing is a bound of nothing, not "no bound".
	zero := target("z", false, true, 0, 0, types.CC1, types.CC2)
	if got, _ := eligibleTargets([]poolTarget{zero}, demand(types.CC1, "", 1000, 0)); len(got) != 0 {
		t.Fatalf("a zero advertised capacity admitted %v", ids(got))
	}
	// An executor advertises no bound, which is not a bound of nothing.
	exec := target("e", false, false, 0, 0, types.CC1, types.CC2)
	if got, _ := eligibleTargets([]poolTarget{exec}, demand(types.CC1, "", 1000, 1000)); len(got) != 1 {
		t.Fatalf("an executor with no advertised bound was refused")
	}
	if r := targetRefusal(placement.ReasonRunnerOffline, "Laptops", types.RunnerPoolUser); r.status != http.StatusUnprocessableEntity || r.body.Reason != "runner_offline" {
		t.Fatalf("offline refusal = %+v", r)
	}
	if r := targetRefusal(placement.ReasonRunnerOffline, "Remote", types.RunnerPoolOrganization); r.body.Reason != string(runnerpool.ReasonNoEligibleMember) {
		t.Fatalf("remote offline is no eligible member, got %+v", r)
	}
}

// §6.2, with pools: one placement is on offer, so the table is run for it.
func TestPlacementForTable(t *testing.T) {
	two := []string{"a", "b"}
	offers := func(remote, local placementOffer) map[types.Placement]placementOffer {
		return map[types.Placement]placementOffer{types.PlacementRemote: remote, types.PlacementLocal: local}
	}
	none := func(r placement.Reason) placementOffer { return placementOffer{Refusal: r} }
	for _, tc := range []struct {
		name   string
		req    placement.Request
		offers map[types.Placement]placementOffer
		denied []types.Placement
		want   placementDecision
	}{
		{"empty, only remote", placement.Request{}, offers(placementOffer{Targets: two}, none("")), nil,
			placementDecision{Placement: types.PlacementRemote, Filled: true, Targets: two}},
		{"empty, only local", placement.Request{}, offers(none(""), placementOffer{Targets: two}), nil,
			placementDecision{Placement: types.PlacementLocal, Filled: true, Targets: two}},
		{"empty, both", placement.Request{}, offers(placementOffer{Targets: two}, placementOffer{Targets: two}), nil,
			placementDecision{Refusal: placement.ReasonPlacementRequired}},
		{"empty, none names the first refusal", placement.Request{}, offers(none(placement.ReasonRunnerOffline), none(placement.ReasonPlacementCapability)), nil,
			placementDecision{Refusal: placement.ReasonRunnerOffline}},
		{"empty, none and no reason", placement.Request{}, map[types.Placement]placementOffer{}, nil,
			placementDecision{Refusal: placement.ReasonPlacementUnavailable}},
		{"remote not eligible", placement.Request{Placement: types.PlacementRemote}, offers(none(""), placementOffer{Targets: two}), nil,
			placementDecision{Refusal: placement.ReasonPlacementUnavailable}},
		{"local, offline", placement.Request{Placement: types.PlacementLocal}, offers(none(""), none(placement.ReasonRunnerOffline)), nil,
			placementDecision{Refusal: placement.ReasonRunnerOffline}},
		{"local denied", placement.Request{Placement: types.PlacementLocal}, offers(none(""), placementOffer{Targets: two}), []types.Placement{types.PlacementLocal},
			placementDecision{Refusal: placement.ReasonPlacementDenied}},
		{"local, several online and no pin: the pool selects, not a refusal", placement.Request{Placement: types.PlacementLocal}, offers(none(""), placementOffer{Targets: two}), nil,
			placementDecision{Placement: types.PlacementLocal, Targets: two}},
		{"local, one online", placement.Request{Placement: types.PlacementLocal}, offers(none(""), placementOffer{Targets: []string{"a"}}), nil,
			placementDecision{Placement: types.PlacementLocal, Targets: []string{"a"}}},
		{"pin narrows to the pinned runner", placement.Request{Placement: types.PlacementLocal, RunnerID: "b"}, offers(none(""), placementOffer{Targets: two}), nil,
			placementDecision{Placement: types.PlacementLocal, Targets: []string{"b"}}},
		{"pin that is not eligible", placement.Request{Placement: types.PlacementLocal, RunnerID: "c"}, offers(none(""), placementOffer{Targets: two}), nil,
			placementDecision{Refusal: placement.ReasonRunnerNotFound}},
	} {
		got := placementFor(tc.req, tc.offers, tc.denied)
		a, _ := json.Marshal(got)
		b, _ := json.Marshal(tc.want)
		if string(a) != string(b) {
			t.Errorf("%s: %s, want %s", tc.name, a, b)
		}
	}
}

// A store that cannot hold pools keeps today's unpooled behaviour: a run without a pool is created with
// no binding, and a run that names a pool is refused in the pool vocabulary, never silently unpooled.
func TestPlacement_StoreWithoutPoolsKeepsUnpooledBehaviour(t *testing.T) {
	h := newHarness(t)
	srv := New(baseTestConfig(h, newCreateDetachStore()))
	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, `{"agent":"claude-code","repo":"acme/widgets","task":"t"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	var run types.AgentRun
	if err := json.Unmarshal(w.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	if run.RunnerPoolID != nil || run.RunnerPoolSelection != "" || run.Placement != "" || run.EvidenceSource != "" {
		t.Fatalf("unpooled run = %+v", run)
	}
	for _, door := range []string{"/api/v1/runs", "/api/v1/runs/preflight", "/api/v1/runs/policy-preview"} {
		w := do(t, srv, http.MethodPost, door, adminToken, `{"agent":"claude-code","repo":"acme/widgets","task":"t","runner_pool_id":"`+uuid.NewString()+`"}`)
		if w.Code != http.StatusNotImplemented || errorReason(w) != string(runnerpool.ReasonPoolsUnavailable) {
			t.Errorf("%s: %d %s", door, w.Code, w.Body)
		}
	}
}

// bind keeps the runner a run was already bound to (the one its promotion found room on) whenever that
// runner can still take it, rather than re-picking by the run id.
func TestBindKeepsThePreferredRunner(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	p := runPlacement{pooled: true, eligible: []poolTarget{
		{id: a.String(), runnerID: &a, facts: runner.TargetFacts{Classes: []types.ConfinementClass{types.CC1}}},
		{id: b.String(), runnerID: &b, facts: runner.TargetFacts{Classes: []types.ConfinementClass{types.CC1}}},
	}}
	runID := uuid.New()
	picked, refusal := p.bind(runID, types.CC1, nil)
	if refusal != nil || picked.runnerID == nil {
		t.Fatalf("bind = %+v %+v", picked, refusal)
	}
	other := a
	if *picked.runnerID == a {
		other = b
	}
	if got, _ := p.bind(runID, types.CC1, &other); got.runnerID == nil || *got.runnerID != other {
		t.Fatalf("bind preferring %s = %v; want the preferred runner kept", other, got.runnerID)
	}
	gone := uuid.New()
	if got, _ := p.bind(runID, types.CC1, &gone); got.runnerID == nil || *got.runnerID != *picked.runnerID {
		t.Fatalf("bind preferring a runner no longer eligible = %v; want the run's own pick %s", got.runnerID, *picked.runnerID)
	}
}
