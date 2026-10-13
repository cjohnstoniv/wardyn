// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Placement across a mixed pool against a real Postgres: a user pool that holds the person's own runner and a
// remote-hosted runner. The kind is decided once, in the placement step, and every later step sees it.
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

const everyonePeople = `{"people_pools":"everyone"}`

// setPeople writes a remote-hosted runner's people policy as the security admin (no second human here).
func (e *placementEnv) setPeople(rid, body string) {
	e.t.Helper()
	path := "/api/v1/remote-runners/" + rid + "/people-policy"
	if body == "" {
		if w := e.call(e.alice, http.MethodDelete, path, ""); w.Code != http.StatusNoContent {
			e.t.Fatalf("clear people policy = %d %s", w.Code, w.Body)
		}
		return
	}
	if w := e.call(e.alice, http.MethodPut, path, body); w.Code != http.StatusOK {
		e.t.Fatalf("people policy %s = %d %s", body, w.Code, w.Body)
	}
}

// maxRuns sets a runner's Runs at once as its owner would.
func (e *placementEnv) maxRuns(id uuid.UUID, owner string, n int) {
	e.t.Helper()
	if _, _, err := e.pg.UpdateRunnerMaxRuns(context.Background(), id, owner, n); err != nil {
		e.t.Fatal(err)
	}
}

// mixedPool is a user pool of the person's own holding their laptop, registered at the max_runs ceiling so
// several runs fit on it, and the remote-hosted runner alpha, which the admin made available to everyone.
func (e *placementEnv) mixedPool(name string) (types.RunnerPool, uuid.UUID) {
	e.t.Helper()
	pool := e.createOwnPool(e.person, name)
	laptop := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2, types.CC3)
	e.maxRuns(laptop, e.personP, types.RunnerMaxRunsCeiling)
	e.addToPool(e.person, pool.ID, laptop)
	e.setPeople("alpha", everyonePeople)
	if w := e.call(e.person, http.MethodPut, myRemotePath(pool.ID, "alpha"), ""); w.Code != http.StatusCreated {
		e.t.Fatalf("add alpha = %d %s", w.Code, w.Body)
	}
	return pool, laptop
}

// occupyRunner puts a RUNNING run of owner on a runner, holding one of its places with no dispatch to race.
func (e *placementEnv) occupyRunner(pool types.RunnerPool, runnerID uuid.UUID, owner string) {
	e.t.Helper()
	id, now := uuid.New(), time.Now().UTC()
	if _, err := e.pg.CreateRun(context.Background(), types.AgentRun{
		ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: owner, Agent: "claude-code", ConfinementClass: types.CC2,
		State: types.RunRunning, SPIFFEID: "spiffe://wardyn.local/agent-run/" + id.String(), RunnerTarget: "docker",
		Placement: types.PlacementLocal, EvidenceSource: types.RunEvidenceRunnerAsserted, RunnerID: &runnerID,
		RunnerPoolID: &pool.ID, RunnerPoolRevision: e.revision(pool.ID), RunnerPoolSelection: types.RunnerPoolSelectedExplicit,
	}); err != nil {
		e.t.Fatalf("occupy: %v", err)
	}
}

// launchStored is a create as the person and the run row it inserted, or the refusal.
func (e *placementEnv) launchStored(body string) (types.AgentRun, int, string) {
	e.t.Helper()
	w := e.call(e.person, http.MethodPost, "/api/v1/runs", body)
	if w.Code != http.StatusCreated {
		return types.AgentRun{}, w.Code, w.Body.String()
	}
	created := decodeJSON[types.AgentRun](e.t, w)
	run, err := e.pg.GetRun(context.Background(), created.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return run, w.Code, ""
}

// selfHostedRunsAreTheirOwners is the invariant the owner-keyed lease and revive slots rest on: every run on
// a self-hosted runner was created by that runner's owner. It answers how many runs are on one.
func (e *placementEnv) selfHostedRunsAreTheirOwners() int {
	e.t.Helper()
	if n := e.count(`SELECT count(*) FROM agent_runs a JOIN runners r ON r.id = a.runner_id WHERE a.created_by <> r.owner`); n != 0 {
		e.t.Errorf("%d run(s) sit on a runner their creator does not own", n)
	}
	return e.count(`SELECT count(*) FROM agent_runs WHERE runner_id IS NOT NULL`)
}

// RP-2 DONE 1: a mixed pool's candidates are the launcher's own claimed runners and the remote-hosted runners
// their people policy admits, nothing else. A forged membership row naming another person's runner, and one
// naming a remote-hosted runner whose policy is off, are never candidates, and no launch lands on them.
func TestPlacement_MixedPoolCandidatesAreOwnOrPermittedRemote(t *testing.T) {
	e := newPlacementEnv(t)
	pool, mine := e.mixedPool("Mixed")
	theirs := e.laptop(e.otherP, 8000, 16384, types.CC1, types.CC2, types.CC3)
	e.maxRuns(theirs, e.otherP, types.RunnerMaxRunsCeiling)
	ctx := context.Background()
	for _, forged := range []string{
		`INSERT INTO runner_pool_members (pool_id, runner_id) VALUES ($1, '` + theirs.String() + `')`,
		`INSERT INTO runner_pool_members (pool_id, remote_runner_id) VALUES ($1, 'beta')`,
	} {
		if _, err := e.pool.Exec(ctx, forged, pool.ID); err != nil {
			t.Fatal(err)
		}
	}
	ps := e.srv.cfg.Store.(store.RunnerPoolStore)
	candidates := func(who string) []string {
		t.Helper()
		pctx := operatorCtx(who, who+"@corp.example", "user")
		rows, refusal := e.srv.permittedPools(pctx, ps, true)
		if refusal != nil {
			t.Fatalf("%s: %+v", who, refusal)
		}
		row, found := catalogueRow(rows, pool.ID)
		if !found {
			return nil
		}
		ts, refusal := e.srv.poolTargets(pctx, &createRunRequest{}, ps, row)
		if refusal != nil {
			t.Fatalf("%s: %+v", who, refusal)
		}
		return ids(ts)
	}
	if got, want := candidates(e.personP), []string{mine.String(), "alpha"}; !slices.Equal(got, want) {
		t.Errorf("the person's candidates = %v, want %v", got, want)
	}
	if got := candidates(e.otherP); len(got) != 0 {
		t.Errorf("another person was offered %v from a pool that is not theirs", got)
	}
	// The candidate filter itself, given everything a forged row could hand it.
	members, err := ps.ListRunnerPoolMembers(ctx, pool.ID)
	if err != nil {
		t.Fatal(err)
	}
	all := []types.Runner{
		{ID: mine, Owner: e.personP, State: types.RunnerClaimed},
		{ID: theirs, Owner: e.otherP, State: types.RunnerClaimed},
	}
	if got := runnerpool.OwnRunnerCandidates(e.personP, members, all); !slices.Equal(got, []uuid.UUID{mine}) {
		t.Errorf("OwnRunnerCandidates = %v, want only %s", got, mine)
	}

	// Through the door, both kinds, many run ids: only the person's own runner or alpha.
	for i := 0; i < 6; i++ {
		for _, extra := range []string{``, `"placement":"remote",`} {
			run, code, body := e.launchStored(runBody(extra + `"runner_pool_id":"` + pool.ID.String() + `"`))
			if code != http.StatusCreated {
				t.Fatalf("launch %q = %d %s", extra, code, body)
			}
			onMine := run.RunnerID != nil && *run.RunnerID == mine
			if !onMine && !(run.RunnerID == nil && run.RunnerPoolExecutor == "alpha") {
				t.Errorf("launch %q landed on runner %v executor %q", extra, run.RunnerID, run.RunnerPoolExecutor)
			}
		}
	}
	if n := e.selfHostedRunsAreTheirOwners(); n != 6 {
		t.Errorf("%d runs on a self-hosted runner, want the 6 unasked ones on the person's own", n)
	}
}

// RP-2 DONE 2: a remote-hosted member of a user pool stops being a candidate as soon as its people policy no
// longer admits the person, at the very next launch; the membership row stays.
func TestPlacement_RemoteMemberDroppedWhenPeoplePolicyNarrows(t *testing.T) {
	e := newPlacementEnv(t)
	pool := e.createOwnPool(e.person, "Borrowed")
	e.setPeople("alpha", everyonePeople)
	if w := e.call(e.person, http.MethodPut, myRemotePath(pool.ID, "alpha"), ""); w.Code != http.StatusCreated {
		t.Fatalf("add alpha = %d %s", w.Code, w.Body)
	}
	body := runBody(`"runner_pool_id":"` + pool.ID.String() + `"`)
	run, code, refused := e.launchStored(body)
	if code != http.StatusCreated || run.RunnerPoolExecutor != "alpha" || run.Placement != types.PlacementRemote || run.RunnerID != nil {
		t.Fatalf("while admitted = %d %s, run on executor %q placement %q runner %v", code, refused, run.RunnerPoolExecutor, run.Placement, run.RunnerID)
	}
	for _, narrowed := range []string{`{"people_pools":"listed","subjects":[{"subject_type":"user","subject":"sub-someone-else"}]}`, ""} {
		e.setPeople("alpha", narrowed)
		before := e.count(`SELECT count(*) FROM agent_runs`)
		sameEverywhere(t, "narrowed to "+narrowed, e.answers(e.person, body), http.StatusUnprocessableEntity, string(runnerpool.ReasonNoEligibleMember))
		if after := e.count(`SELECT count(*) FROM agent_runs`); after != before {
			t.Errorf("narrowed to %q: the refused launch left %d run(s)", narrowed, after-before)
		}
		if e.members(pool.ID) != 1 {
			t.Errorf("narrowed to %q: the membership row went", narrowed)
		}
	}
}

// RP-2 DONE 3: deny_placements holds each kind in a mixed pool, never the pool as a whole. A member denied
// local launches lands only on its remote-hosted runner; one denied remote launches only on their own
// runner, and waits for it rather than spill onto the remote-hosted one when it is full. Asking for the
// denied kind is the profile's sentence.
func TestPlacement_DenyPlacementsFilterPerKind(t *testing.T) {
	for _, tc := range []struct {
		deny, lands types.Placement
	}{{types.PlacementLocal, types.PlacementRemote}, {types.PlacementRemote, types.PlacementLocal}} {
		t.Run("deny "+string(tc.deny), func(t *testing.T) {
			e := newPlacementEnv(t)
			pool, laptop := e.mixedPool("Mixed")
			e.assignProfile("walled", types.GovernanceLimits{DenyPlacements: []types.Placement{tc.deny}})
			body := runBody(`"runner_pool_id":"` + pool.ID.String() + `"`)
			for i := 0; i < 4; i++ {
				run, code, refused := e.launchStored(body)
				if code != http.StatusCreated {
					t.Fatalf("launch = %d %s, want the pool's other kind to take it", code, refused)
				}
				onLaptop := run.RunnerID != nil && *run.RunnerID == laptop
				if run.Placement != tc.lands || onLaptop != (tc.lands == types.PlacementLocal) {
					t.Errorf("run placed %q on runner %v executor %q, want %q", run.Placement, run.RunnerID, run.RunnerPoolExecutor, tc.lands)
				}
			}
			asked := runBody(`"placement":"` + string(tc.deny) + `","runner_pool_id":"` + pool.ID.String() + `"`)
			first := sameEverywhere(t, "asking for the denied kind", e.answers(e.person, asked), http.StatusForbidden, string(placement.ReasonPlacementDenied))
			if !strings.Contains(first, "walled") {
				t.Errorf("the refusal %s does not name the profile", first)
			}
			if tc.deny != types.PlacementRemote {
				return
			}
			// Their own runner full: the run is not moved onto the remote-hosted runner the profile denies.
			if _, err := e.pool.Exec(context.Background(), `UPDATE agent_runs SET state = 'COMPLETED'`); err != nil {
				t.Fatal(err)
			}
			e.maxRuns(laptop, e.personP, 1)
			e.occupyRunner(pool, laptop, e.personP)
			if _, code, refused := e.launchStored(body); code != http.StatusUnprocessableEntity || !strings.Contains(refused, string(placement.ReasonRunnerAtCapacity)) {
				t.Errorf("own runner full, remote denied = %d %s, want runner_at_capacity", code, refused)
			}
		})
	}
}

// RP-2 DONE 4: the kind is decided in the placement step, so a run bound to the person's own runner always
// passed local eligibility. With a deployment that cannot deliver a local run's credentials, an unasked launch
// into a mixed pool whose own runner has room is refused with that reason, never moved to the remote-hosted
// runner and never put on the laptop; the sentence says how to run it there. Asking for remote runs it there.
func TestPlacement_MixedPoolLocalRunPassesLocalEligibility(t *testing.T) {
	e := newPlacementEnv(t)
	pool, laptop := e.mixedPool("Mixed")
	body := runBody(`"runner_pool_id":"` + pool.ID.String() + `"`)
	run, code, refused := e.launchStored(body)
	if code != http.StatusCreated || run.Placement != types.PlacementLocal || !run.PlacementFilled || run.RunnerID == nil || *run.RunnerID != laptop {
		t.Fatalf("own runner with room = %d %s, run placed %q filled %v on %v; want local on %s", code, refused, run.Placement, run.PlacementFilled, run.RunnerID, laptop)
	}
	if _, err := e.pg.PutSiteConfig(context.Background(), types.SiteConfig{Runners: &types.RunnerSettings{Enabled: true}, UpstreamProxySecretRef: "corporate-parent"}); err != nil {
		t.Fatal(err)
	}
	onLaptop := e.count(`SELECT count(*) FROM agent_runs WHERE runner_id IS NOT NULL`)
	for i := 0; i < 8; i++ {
		w := e.call(e.person, http.MethodPost, "/api/v1/runs", body)
		if w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != string(placement.ReasonPlacementCredential) ||
			!strings.Contains(wireError(t, w), remoteInsteadMsg(pool.Name)) {
			t.Fatalf("unasked launch = %d %s, want placement_credential saying how to run it remote", w.Code, w.Body)
		}
	}
	for i := 0; i < 8; i++ {
		run, code, refused := e.launchStored(runBody(`"placement":"remote","runner_pool_id":"` + pool.ID.String() + `"`))
		if code != http.StatusCreated || run.RunnerID != nil || run.RunnerPoolExecutor != "alpha" || run.Placement != types.PlacementRemote {
			t.Fatalf("asked remote = %d %s, run on %v executor %q placement %q", code, refused, run.RunnerID, run.RunnerPoolExecutor, run.Placement)
		}
	}
	if n := e.count(`SELECT count(*) FROM agent_runs WHERE runner_id IS NOT NULL`); n != onLaptop {
		t.Errorf("%d run(s) reached the laptop without passing local eligibility", n-onLaptop)
	}
}

// RP-2 DONE 6: every run on a self-hosted runner was created by that runner's owner, whether create placed it
// or the queue promoted and started it, in a mixed pool that also holds a forged membership row of another
// person's runner. Revive keeps the runner the run already has (it never rebinds).
func TestPG_SelfHostedRunsAreAlwaysTheirOwners(t *testing.T) {
	e := newQueueEnv(t)
	pool, laptop := e.mixedPool("Mixed")
	theirs := e.laptop(e.otherP, 8000, 16384, types.CC1, types.CC2, types.CC3)
	e.maxRuns(theirs, e.otherP, types.RunnerMaxRunsCeiling)
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO runner_pool_members (pool_id, runner_id) VALUES ($1, $2)`, pool.ID, theirs); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, code, refused := e.launchStored(runBody(`"runner_pool_id":"` + pool.ID.String() + `"`)); code != http.StatusCreated {
			t.Fatalf("create = %d %s", code, refused)
		}
	}
	// The pool runs one run at once: a launch waits, and the promotion places it when the occupant ends.
	limits := types.RunnerPoolLimits{
		Background: &types.RunnerPoolBackgroundLimits{CPUMillis: types.RunnerPoolAmount{Default: 500, Cap: 4000}, MemoryMiB: types.RunnerPoolAmount{Default: 512, Cap: 8192},
			Lifetime: types.RunnerPoolDuration{DefaultSec: 3600, MaxSec: 7200}},
		Barriers: []types.ConfinementClass{types.CC1, types.CC2, types.CC3}, MaxConcurrentRuns: intp(1),
	}
	e.exec(`UPDATE agent_runs SET state = 'COMPLETED'`)
	if _, err := e.pg.UpdateRunnerPool(context.Background(), pool.ID, e.revision(pool.ID), nil, nil, &limits); err != nil {
		t.Fatal(err)
	}
	occupant := e.occupy(pool)
	queued := e.queuedLaunch(e.person, e.reread(pool), "")
	e.free(occupant)
	started := e.waitStarted(queued.ID)
	if started.RunnerID == nil || *started.RunnerID != laptop || started.Placement != types.PlacementLocal {
		t.Errorf("the promoted run started on runner %v placement %q, want its owner's laptop %s", started.RunnerID, started.Placement, laptop)
	}
	if n := e.selfHostedRunsAreTheirOwners(); n != 5 {
		t.Errorf("%d runs on a self-hosted runner, want the 4 created and the 1 promoted", n)
	}
}

// RP-2 r1 F1: a promoted run whose promotion took its owner's runner's last place starts on that runner.
// The start finds the run itself holding the place; it is the run's own, so the runner still has room for
// it and the run is not moved onto the pool's remote-hosted runner.
func TestPG_PromotedRunKeepsItsOwnRunnersLastPlace(t *testing.T) {
	e := newQueueEnv(t)
	pool, laptop := e.mixedPool("Mixed")
	e.maxRuns(laptop, e.personP, 1)
	limits := types.RunnerPoolLimits{
		Background: &types.RunnerPoolBackgroundLimits{CPUMillis: types.RunnerPoolAmount{Default: 500, Cap: 4000}, MemoryMiB: types.RunnerPoolAmount{Default: 512, Cap: 8192},
			Lifetime: types.RunnerPoolDuration{DefaultSec: 3600, MaxSec: 7200}},
		Barriers: []types.ConfinementClass{types.CC1, types.CC2, types.CC3}, MaxConcurrentRuns: intp(1),
	}
	if _, err := e.pg.UpdateRunnerPool(context.Background(), pool.ID, e.revision(pool.ID), nil, nil, &limits); err != nil {
		t.Fatal(err)
	}
	occupant := e.occupy(pool)
	queued := e.queuedLaunch(e.person, e.reread(pool), "")
	e.free(occupant)
	started := e.waitStarted(queued.ID)
	if started.RunnerID == nil || *started.RunnerID != laptop || started.Placement != types.PlacementLocal || started.RunnerPoolExecutor != "" {
		t.Errorf("the promoted run started on runner %v placement %q executor %q, want its owner's laptop %s (max_runs 1)",
			started.RunnerID, started.Placement, started.RunnerPoolExecutor, laptop)
	}
}

// RP-2 r1 F2: the pool list and the dry doors judge each kind of runner in a user pool as placement does,
// so a pool reads available exactly when a launch into it is placed, and placement_denied exactly when the
// launch is denied.
func TestPG_PoolAvailabilityMatchesPlacementPerKind(t *testing.T) {
	local, remote := types.PlacementLocal, types.PlacementRemote
	for _, tc := range []struct {
		name                  string
		deny                  []types.Placement
		own, ownOnline, alpha bool
		available             bool
		reason                runnerpool.Reason
		launch                int
	}{
		{"remote-hosted only, admitted", nil, false, false, true, true, "", http.StatusCreated},
		{"mixed, local denied", []types.Placement{local}, true, true, true, true, "", http.StatusCreated},
		{"mixed, own runner offline", nil, true, false, true, true, "", http.StatusCreated},
		{"own runner only, local denied", []types.Placement{local}, true, true, false, false, runnerpool.AvailabilityPlacementDenied, http.StatusForbidden},
		{"remote-hosted only, remote denied", []types.Placement{remote}, false, false, true, false, runnerpool.AvailabilityPlacementDenied, http.StatusForbidden},
		{"mixed, both denied", []types.Placement{local, remote}, true, true, true, false, runnerpool.AvailabilityPlacementDenied, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPlacementEnv(t)
			pool := e.createOwnPool(e.person, "Pool")
			if tc.own {
				laptop := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2, types.CC3)
				e.maxRuns(laptop, e.personP, types.RunnerMaxRunsCeiling)
				e.addToPool(e.person, pool.ID, laptop)
				e.online[laptop] = tc.ownOnline
			}
			if tc.alpha {
				e.setPeople("alpha", everyonePeople)
				if w := e.call(e.person, http.MethodPut, myRemotePath(pool.ID, "alpha"), ""); w.Code != http.StatusCreated {
					t.Fatalf("add alpha = %d %s", w.Code, w.Body)
				}
			}
			if tc.deny != nil {
				e.assignProfile("walled", types.GovernanceLimits{DenyPlacements: tc.deny})
			}
			wantAvailability := client.RunnerPoolUnavailable
			if tc.available {
				wantAvailability = client.RunnerPoolAvailable
			}
			if got := e.listed(e.person)[pool.ID]; got.Availability != wantAvailability || got.Reason != tc.reason {
				t.Errorf("list = %s %s, want %s %s", got.Availability, got.Reason, wantAvailability, tc.reason)
			}
			body := runBody(`"runner_pool_id":"` + pool.ID.String() + `"`)
			if w := e.call(e.person, http.MethodPost, "/api/v1/runs/preflight", body); tc.launch == http.StatusCreated {
				choices := decodeJSON[struct {
					RunnerPools []client.RunnerPoolChoice `json:"runner_pools"`
				}](t, w).RunnerPools
				if len(choices) != 1 || choices[0].Availability != wantAvailability || choices[0].Reason != tc.reason {
					t.Errorf("preflight = %d, choices %+v, want %s %s", w.Code, choices, wantAvailability, tc.reason)
				}
			}
			if w := e.call(e.person, http.MethodPost, "/api/v1/runs", body); w.Code != tc.launch {
				t.Errorf("launch = %d %s, want %d", w.Code, w.Body, tc.launch)
			}
		})
	}
}
