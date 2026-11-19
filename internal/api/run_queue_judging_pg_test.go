// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// What one promotion pass reads of the runners, against a real Postgres. Guarded by WARDYN_TEST_PG.

// passRunnerReads is the store counting the runner reads a promotion pass makes, a read whose context
// carries a pass: the organisation's pool defaults, the reads behind a pool's targets, and the runners'
// limits and usage. A create's own reads and a start's carry no pass and are not counted.
type passRunnerReads struct {
	store.PG
	n *atomic.Int64
}

func (s passRunnerReads) count(ctx context.Context) {
	if _, ok := ctx.Value(queuePassKey{}).(*queuePass); ok {
		s.n.Add(1)
	}
}

func (s passRunnerReads) GetRunnerPoolOrgDefaults(ctx context.Context) (types.RunnerPoolDefaults, error) {
	s.count(ctx)
	return s.PG.GetRunnerPoolOrgDefaults(ctx)
}

func (s passRunnerReads) GetSiteConfig(ctx context.Context) (types.SiteConfig, error) {
	s.count(ctx)
	return s.PG.GetSiteConfig(ctx)
}

func (s passRunnerReads) ListRunnerPoolMembers(ctx context.Context, id uuid.UUID) ([]types.RunnerPoolMember, error) {
	s.count(ctx)
	return s.PG.ListRunnerPoolMembers(ctx, id)
}

func (s passRunnerReads) ListRunnersByOwner(ctx context.Context, owner string) ([]types.Runner, error) {
	s.count(ctx)
	return s.PG.ListRunnersByOwner(ctx, owner)
}

func (s passRunnerReads) ListRunners(ctx context.Context) ([]types.Runner, error) {
	s.count(ctx)
	return s.PG.ListRunners(ctx)
}

func (s passRunnerReads) ListRemoteRunners(ctx context.Context) ([]store.RemoteRunnerRow, error) {
	s.count(ctx)
	return s.PG.ListRemoteRunners(ctx)
}

func (s passRunnerReads) RunnerUsage(ctx context.Context, runners []uuid.UUID, remotes []string, except uuid.UUID) (map[string]types.RunnerTally, error) {
	s.count(ctx)
	return s.PG.RunnerUsage(ctx, runners, remotes, except)
}

// One person's waiting runs that each wait for their own runner cost a pass one reading of the runners,
// however many there are: another person's run queued behind as many of them as a person may queue is
// judged and promoted by its own create's pass, and that pass reads the runners no more often than with
// one of them ahead. alpha alone enforces Wall and is full; beta enforces only Fence and is idle, so each
// Wall run waits for alpha and holds none of its owner's later runs.
func TestPG_OnePersonsWaitingRunsCostAPassOneReadingOfTheRunners(t *testing.T) {
	ctx := context.Background()
	e := newQueueEnv(t)
	e.srv.cfg.DefaultPolicy.MinConfinementClass = types.CC1 // the Fence runs below ask for less than the environment's Wall floor
	e.runner.execFacts["beta"] = runner.TargetFacts{Classes: []types.ConfinementClass{types.CC1}}
	reads := &atomic.Int64{}
	e.srv.cfg.Store = passRunnerReads{PG: e.pg, n: reads}
	p := e.cappedPool("Shared", 2*queueMaxPerOwner)
	e.addExecutors(p.ID, "beta")
	p = e.reread(p)
	occupyRemote(e.placementEnv, p, "alpha")
	first := e.run(e.queuedLaunch(e.person, p, `,"confinement_class":"CC2"`).ID)
	fence := func() int64 {
		t.Helper()
		before := reads.Load()
		out, w := e.launch(e.other, p, `,"confinement_class":"CC1"`)
		if w.Code != http.StatusCreated || out.State == types.RunQueued {
			t.Fatalf("the other person's Fence run = %d %s; want it promoted by its own create's pass", w.Code, w.Body)
		}
		n := reads.Load() - before
		if got := e.waitStarted(out.ID); got.RunnerPoolExecutor != "beta" {
			t.Fatalf("the other person's Fence run started on %q (%s); want beta", got.RunnerPoolExecutor, got.StatusDetail)
		}
		return n
	}
	one := fence()
	entry, err := e.pg.GetRunQueueEntry(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	for range queueMaxPerOwner - 1 {
		now, id := time.Now().UTC(), uuid.New()
		if _, err := e.pg.CreateQueuedRun(ctx, types.AgentRun{
			ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: first.CreatedBy, Agent: first.Agent, ConfinementClass: types.CC2,
			State: types.RunQueued, RunnerTarget: first.RunnerTarget, RunnerPoolID: &p.ID, RunnerPoolRevision: first.RunnerPoolRevision,
			RunnerPoolSelection: types.RunnerPoolSelectedExplicit,
		}, store.RunQueueEntry{QueuedAt: now, DeadlineAt: now.Add(time.Hour), PolicyRef: entry.PolicyRef, Authority: entry.Authority, Launch: entry.Launch}, 0); err != nil {
			t.Fatal(err)
		}
	}
	if many := fence(); many != one {
		t.Errorf("with %d of the person's waiting runs ahead the pass made %d runner reads, and %d with one; want the same", queueMaxPerOwner, many, one)
	}
	if n := e.count(`SELECT count(*) FROM run_queue WHERE pool_id = $1 AND owner = $2`, p.ID, first.CreatedBy); n != queueMaxPerOwner {
		t.Errorf("%d of the person's Wall runs wait; want all %d, each still waiting for alpha", n, queueMaxPerOwner)
	}
}
