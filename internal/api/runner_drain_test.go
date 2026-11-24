// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func drained(t poolTarget) poolTarget {
	t.draining = true
	return t
}

// A draining target answers runner_draining before offline, capability and fit; a pool answers the drain only
// when every refusing runner is drained, in either order, because an undrain of one runner would not help then.
func TestRefusal_DrainingIsFirst(t *testing.T) {
	d := demand(types.CC3, "", 0, 0)
	if got := d.refusal(drained(target("a", true, false, 0, 0))); got != placement.ReasonRunnerDraining {
		t.Fatalf("a drained offline target = %q, want runner_draining", got)
	}
	if got := d.refusal(drained(target("b", false, true, 4000, 8192, types.CC1))); got != placement.ReasonRunnerDraining {
		t.Fatalf("a drained target below the barrier = %q, want runner_draining", got)
	}
	free := func(id string) poolTarget {
		return target(id, false, true, 4000, 8192, types.CC1, types.CC2, types.CC3)
	}
	atCapacity := func(id string) poolTarget { x := free(id); x.full = true; return x }
	ok := demand(types.CC1, "", 0, 0)
	ok.holdRunnerCapacity = true
	for name, tc := range map[string]struct {
		in   []poolTarget
		want placement.Reason
	}{
		"drained then at capacity": {[]poolTarget{drained(free("d1")), atCapacity("c1")}, placement.ReasonRunnerAtCapacity},
		"at capacity then drained": {[]poolTarget{atCapacity("c2"), drained(free("d2"))}, placement.ReasonRunnerAtCapacity},
		"drained then offline":     {[]poolTarget{drained(free("d3")), target("o3", true, false, 0, 0)}, placement.ReasonRunnerOffline},
		"offline then drained":     {[]poolTarget{target("o4", true, false, 0, 0), drained(free("d4"))}, placement.ReasonRunnerOffline},
		"two drained":              {[]poolTarget{drained(free("d5")), drained(free("d6"))}, placement.ReasonRunnerDraining},
		"one drained":              {[]poolTarget{drained(free("d7"))}, placement.ReasonRunnerDraining},
	} {
		got, reason := eligibleTargets(tc.in, ok)
		if len(got) != 0 || reason != tc.want {
			t.Errorf("%s: eligible %v, reason %q, want none and %q", name, ids(got), reason, tc.want)
		}
	}
	r := targetRefusal(placement.ReasonRunnerDraining, "Laptops", types.RunnerPoolUser)
	if r.status != http.StatusConflict || r.body.Reason != "runner_draining" || !strings.Contains(r.body.Error, "Every runner in Laptops that could take this run is drained for maintenance.") {
		t.Fatalf("draining refusal = %+v", r)
	}
}

// A pool of one draining and one free runner offers the free one, and the pool's pick lands on it.
func TestEligibleTargets_DrainingPassedOver(t *testing.T) {
	free := target("f", false, true, 4000, 8192, types.CC1, types.CC2)
	got, _ := eligibleTargets([]poolTarget{drained(target("d", false, true, 4000, 8192, types.CC1, types.CC2)), free}, demand(types.CC1, "", 0, 0))
	if !slices.Equal(ids(got), []string{"f"}) {
		t.Fatalf("eligible = %v, want only the free runner", ids(got))
	}
	if pick := runnerpool.Select(uuid.New(), uuid.New(), ids(got)); pick != "f" {
		t.Fatalf("pick = %q, want f", pick)
	}
}

// drainsFake is a store whose drain read fails.
type drainsFake struct {
	store.Store
	err error
}

func (drainsFake) DrainRunner(context.Context, uuid.UUID, types.RunnerDrain) (types.RunnerDrain, bool, error) {
	return types.RunnerDrain{}, false, nil
}
func (drainsFake) UndrainRunner(context.Context, uuid.UUID) (types.RunnerDrain, bool, error) {
	return types.RunnerDrain{}, false, nil
}
func (f drainsFake) RunnerDrains(context.Context, []uuid.UUID) (map[uuid.UUID]types.RunnerDrain, error) {
	return nil, f.err
}

// An unreadable drain table refuses placement with a server error; it never places the run.
func TestRunnerDrain_ReadErrorRefusesPlacement(t *testing.T) {
	s := &Server{cfg: Config{Store: drainsFake{err: errors.New("db down")}}}
	targets, refusal := s.registeredRunnerTargets(context.Background(), []uuid.UUID{uuid.New()}, nil)
	if len(targets) != 0 || refusal == nil || refusal.operation != "read runner drains" || refusal.err == nil {
		t.Fatalf("targets %v, refusal %+v; want a server error `read runner drains`", targets, refusal)
	}
	// A store with no drain table reads as no drains.
	if drains, err := (&Server{cfg: Config{Store: struct{ store.Store }{}}}).runnerDrainsOf(context.Background(), []uuid.UUID{uuid.New()}); err != nil || drains != nil {
		t.Fatalf("a store without drains = %v, %v; want nil, nil", drains, err)
	}
}

type drainsWith struct {
	drainsFake
	rows map[uuid.UUID]types.RunnerDrain
}

func (d *drainsWith) RunnerDrains(context.Context, []uuid.UUID) (map[uuid.UUID]types.RunnerDrain, error) {
	return d.rows, nil
}

// A drained runner is a draining target, except for the run whose promotion took its place before the drain.
func TestRunnerDrain_PromotedBeforeDrainStillStarts(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	s := &Server{cfg: Config{Store: &drainsWith{rows: map[uuid.UUID]types.RunnerDrain{a: {Reason: "upgrade"}, b: {Reason: "upgrade"}}}}}
	runners := []types.Runner{{ID: a}, {ID: b}}
	draining := func(ctx context.Context) map[string]bool {
		targets, refusal := s.registeredRunnerTargets(ctx, []uuid.UUID{a, b}, runners)
		if refusal != nil {
			t.Fatalf("targets refused: %+v", refusal)
		}
		out := map[string]bool{}
		for _, tg := range targets {
			out[tg.id] = tg.draining
		}
		return out
	}
	if got := draining(context.Background()); !got[a.String()] || !got[b.String()] {
		t.Fatalf("a new run sees draining = %v, want both runners draining", got)
	}
	got := draining(withPromotedStart(context.Background(), uuid.New(), &a, "", false))
	if got[a.String()] || !got[b.String()] {
		t.Fatalf("a run promoted onto a sees draining = %v, want a open and b still draining", got)
	}
}

// A lost run on a drained runner is refused and audited; a live run, a run with no runner and a run on another
// runner pass untouched.
func TestReviveDrainRefusal_LostOnlyOnDrainedRunner(t *testing.T) {
	id, other := uuid.New(), uuid.New()
	rows := &memAudit{}
	s := &Server{cfg: Config{Store: &drainsWith{rows: map[uuid.UUID]types.RunnerDrain{id: {Reason: "upgrade"}}}, Audit: rows, Now: time.Now}}
	lostAt := time.Now()
	lost := types.AgentRun{ID: uuid.New(), RunnerID: &id, LostAt: &lostAt, CreatedBy: "owner"}
	for name, tc := range map[string]struct {
		run  types.AgentRun
		deny bool
	}{
		"lost on the drained runner": {lost, true},
		"live on the drained runner": {types.AgentRun{ID: uuid.New(), RunnerID: &id}, false},
		"lost on another runner":     {types.AgentRun{ID: uuid.New(), RunnerID: &other, LostAt: &lostAt}, false},
		"lost with no runner":        {types.AgentRun{ID: uuid.New(), LostAt: &lostAt}, false},
	} {
		rerr := s.reviveDrainRefusal(context.Background(), tc.run, types.ActorHuman, "alice")
		if (rerr != nil) != tc.deny {
			t.Errorf("%s: refusal %+v, want deny=%v", name, rerr, tc.deny)
		}
		if tc.deny && (rerr == nil || rerr.status != http.StatusConflict || rerr.reason != "runner_draining") {
			t.Errorf("%s: refusal %+v, want 409 runner_draining", name, rerr)
		}
	}
	denied := rows.find("run.revive")
	if len(denied) != 1 || denied[0].Outcome != "denied" || !strings.Contains(string(denied[0].Data), `"runner_id":"`+id.String()+`"`) {
		t.Fatalf("run.revive rows = %+v, want one denied row naming the runner", denied)
	}
}

// A kept machine's resume pins its Mac through the drive pin (keptPlacementPin); the pin narrows the pool to
// that runner (targetsOn), and a drained Mac then refuses the resume with runner_draining, never a wait.
func TestRunnerDrain_KeptMachineResumeRefused(t *testing.T) {
	mac := uuid.New()
	free := target("free", false, true, 4000, 8192, types.CC1)
	macTarget := drained(target("mac", false, true, 4000, 8192, types.CC1))
	macTarget.runnerID = &mac
	drives, driveRunner := keptPlacementPin(false, nil, &mac)
	if !drives || driveRunner == nil || *driveRunner != mac {
		t.Fatalf("keptPlacementPin = %v %v, want a pin on the machine's Mac", drives, driveRunner)
	}
	pinned := targetsOn([]poolTarget{free, macTarget}, driveRunner)
	got, reason := eligibleTargets(pinned, demand(types.CC1, "", 0, 0))
	if len(got) != 0 || reason != placement.ReasonRunnerDraining {
		t.Fatalf("resume onto a drained Mac: eligible %v, reason %q; want none and runner_draining", ids(got), reason)
	}
	if r := targetRefusal(reason, "Macs", types.RunnerPoolUser); r.status != http.StatusConflict || r.body.Reason != "runner_draining" {
		t.Fatalf("resume refusal = %+v, want 409 runner_draining", r)
	}
}

// Attach, exec, SSH, folders, the gateway and a live run's proxy refresh never read a drain: only the
// placement targets, the revive check and the inventory do.
func TestRunnerDrain_AttachAndExecUnaffected(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var readers []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), "runnerDrainsOf(") || strings.Contains(string(src), "RunnerDrains(") {
			readers = append(readers, f)
		}
	}
	want := []string{"run_placement_targets.go", "runner_drain.go", "runner_inventory.go"}
	if !slices.Equal(readers, want) {
		t.Fatalf("files that read a runner's drain = %v, want %v: a door that reads it must be named here and in the drain's docs", readers, want)
	}
}

// reviveRunProxy, the one door a lost run's revive goes through, calls the drain refusal before it marks the
// run reviving. The PG test through the route pins the behaviour; this pins the call where no database runs.
func TestRunnerDrain_ReviveRunProxyCallsRefusal(t *testing.T) {
	src, err := os.ReadFile("run_revive.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	start := strings.Index(text, "func (s *Server) reviveRunProxy(")
	if start < 0 {
		t.Fatal("reviveRunProxy not found in run_revive.go")
	}
	body := text[start:]
	call, mark := strings.Index(body, "s.reviveDrainRefusal(ctx, run, actorType, actor)"), strings.Index(body, "s.markReviving(run)")
	if call < 0 || mark < 0 || call > mark {
		t.Fatalf("reviveRunProxy calls reviveDrainRefusal at %d and markReviving at %d; the refusal must come first", call, mark)
	}
}
