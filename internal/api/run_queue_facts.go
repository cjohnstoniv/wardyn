// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// queuePass holds, for one promotion pass, the facts about an owner that a promotion cannot change: the
// pools the owner may use and their ceiling. A pass judges many waiting runs of few owners, and each
// judgement would read the same two facts again. What a promotion does change (the runners' room and
// usage) is never held here: queueJudging holds it for one pool's judging only. A pass judges one entry
// at a time, so nothing locks it, and it is dropped with the pass: no fact outlives the pass that read it.
type queuePass struct {
	owners map[string]*queueOwnerFacts
}

type queuePassKey struct{}

// queueOwnerFacts is one owner's facts, each read at most once per pass. An owner is keyed by name and
// identity (queueIdentity), as a hold key is: two runs of one person queued under different group
// snapshots resolve different pools and ceilings, while the session epoch, revocation sequence,
// delegated token id and agent that tell one person's sessions and tokens apart change neither answer
// (queueHoldKey says why).
type queueOwnerFacts struct {
	// pools are the owner's permitted pools by the unit their runs froze (frozenUnitKey): a unit row of a
	// use policy admits by that unit, so two runs of one owner in two units read two answers.
	pools       map[string]queuePoolFacts
	ceilingRead bool
	ceiling     governanceCeiling
	ceilingErr  error
}

type queuePoolFacts struct {
	rows    []store.RunnerPoolCatalogueRow
	refusal *runRefusal
}

type queueOwnerKey struct{}

// frozenUnitKey names the unit ctx judges a pool's unit rows by: "" for the owner's current unit, "-"
// for a run that froze none, else the frozen unit's id.
func frozenUnitKey(ctx context.Context) string {
	f, ok := ctx.Value(frozenPoolUnitKey{}).(frozenPoolUnit)
	switch {
	case !ok:
		return ""
	case f.id == nil:
		return "-"
	}
	return f.id.String()
}

// withQueuePass starts a pass's cache on ctx.
func withQueuePass(ctx context.Context) context.Context {
	return context.WithValue(ctx, queuePassKey{}, &queuePass{owners: map[string]*queueOwnerFacts{}})
}

// inheritQueuePass carries the caller's pass, or starts one when it has none, onto the pass's own context.
func inheritQueuePass(base, caller context.Context) context.Context {
	if p, ok := caller.Value(queuePassKey{}).(*queuePass); ok {
		return context.WithValue(base, queuePassKey{}, p)
	}
	return withQueuePass(base)
}

// withQueueOwner binds the entry's owner facts to ctx; without a pass on ctx it binds none, and every
// read goes to the store.
func withQueueOwner(ctx context.Context, owner string, authority []byte) context.Context {
	p, ok := ctx.Value(queuePassKey{}).(*queuePass)
	if !ok {
		return ctx
	}
	key := owner + "\x00" + queueIdentity(authority)
	f := p.owners[key]
	if f == nil {
		f = &queueOwnerFacts{}
		p.owners[key] = f
	}
	return context.WithValue(ctx, queueOwnerKey{}, f)
}

// queuePermittedPools is permittedPools for the owner on ctx, read once per pass for each frozen unit.
func (s *Server) queuePermittedPools(ctx context.Context, ps store.RunnerPoolStore) ([]store.RunnerPoolCatalogueRow, *runRefusal) {
	f, ok := ctx.Value(queueOwnerKey{}).(*queueOwnerFacts)
	if !ok {
		return s.permittedPools(ctx, ps, true)
	}
	key := frozenUnitKey(ctx)
	got, read := f.pools[key]
	if !read {
		got.rows, got.refusal = s.permittedPools(ctx, ps, true)
		if f.pools == nil {
			f.pools = map[string]queuePoolFacts{}
		}
		f.pools[key] = got
	}
	return got.rows, got.refusal
}

// queueCeiling is effectiveCeiling for the owner on ctx, read once per pass.
func (s *Server) queueCeiling(ctx context.Context) (governanceCeiling, error) {
	f, ok := ctx.Value(queueOwnerKey{}).(*queueOwnerFacts)
	if !ok {
		return s.effectiveCeiling(ctx)
	}
	if !f.ceilingRead {
		f.ceiling, f.ceilingErr = s.effectiveCeiling(ctx)
		f.ceilingRead = true
	}
	return f.ceiling, f.ceilingErr
}

// queueJudging holds, for one judging of one pool's waiting runs, the runners' side of each hold key's
// verdicts: the organisation's pool defaults, the pool's targets, and the runners' limits and usage. A
// promotion changes the runners' room, so these never outlive the judging that read them: promoteQueue
// starts one for each pass over a pool, every verdict of that pass is asked before any of its runs is
// promoted (store.QueueAdmit), and the promotion counts each runner again under its lock. A hold key's
// later runs are judged by the runners as its first run of the judging read them, and cost no store
// read, so one person's many runs that each wait for their own runner do not spend the judging's time.
type queueJudging struct {
	keys map[string]*queueRunnerFacts
}

type queueJudgingKey struct{}

// queueRunnerFacts is one hold key's runner facts in a judging, each read at most once.
type queueRunnerFacts struct {
	orgRead bool
	org     types.RunnerPoolDefaults
	orgErr  error
	// targets are poolTargets' answers by the two parts of the request it reads: the placement and the
	// pinned runner.
	targets  map[string]queueTargets
	fitsRead bool
	fits     map[string]runnerFit
	fitsErr  error
}

type queueTargets struct {
	targets []poolTarget
	refusal *runRefusal
}

type queueRunnersKey struct{}

// withQueueJudging starts a judging's runner facts on ctx; a judging already on ctx is never shared.
func withQueueJudging(ctx context.Context) context.Context {
	return context.WithValue(ctx, queueJudgingKey{}, &queueJudging{keys: map[string]*queueRunnerFacts{}})
}

// withQueueRunners binds the runner facts of e's hold key (queueHoldKey) to ctx; outside a judging it
// binds none, and every runner read goes to the store. The key is never the owner alone: two identities
// or two frozen units of one owner see different pools and runners.
func withQueueRunners(ctx context.Context, e store.RunQueueEntry) context.Context {
	j, ok := ctx.Value(queueJudgingKey{}).(*queueJudging)
	if !ok {
		return ctx
	}
	key := queueHoldKey(e)
	f := j.keys[key]
	if f == nil {
		f = &queueRunnerFacts{}
		j.keys[key] = f
	}
	return context.WithValue(ctx, queueRunnersKey{}, f)
}

// queueOrgDefaults is the organisation's runner pool defaults, read once per judging for each hold key.
func queueOrgDefaults(ctx context.Context, ps store.RunnerPoolStore) (types.RunnerPoolDefaults, error) {
	f, ok := ctx.Value(queueRunnersKey{}).(*queueRunnerFacts)
	if !ok {
		return ps.GetRunnerPoolOrgDefaults(ctx)
	}
	if !f.orgRead {
		f.org, f.orgErr = ps.GetRunnerPoolOrgDefaults(ctx)
		f.orgRead = true
	}
	return f.org, f.orgErr
}

// queuePoolTargets is poolTargets for the run on ctx, read once per judging for each hold key, placement
// and pinned runner. The targets are shared by the key's runs and only read.
func (s *Server) queuePoolTargets(ctx context.Context, req *createRunRequest, ps store.RunnerPoolStore, row store.RunnerPoolCatalogueRow) ([]poolTarget, *runRefusal) {
	f, ok := ctx.Value(queueRunnersKey{}).(*queueRunnerFacts)
	if !ok {
		return s.poolTargets(ctx, req, ps, row)
	}
	key := string(req.Placement) + "\x00" + req.RunnerID
	got, read := f.targets[key]
	if !read {
		got.targets, got.refusal = s.poolTargets(ctx, req, ps, row)
		if f.targets == nil {
			f.targets = map[string]queueTargets{}
		}
		f.targets[key] = got
	}
	return got.targets, got.refusal
}

// queueRunnerFits is runnerFits for the owner on ctx, read once per judging for each hold key. The map is
// shared by the key's runs and only read.
func (s *Server) queueRunnerFits(ctx context.Context) (map[string]runnerFit, error) {
	f, ok := ctx.Value(queueRunnersKey{}).(*queueRunnerFacts)
	if !ok {
		return s.runnerFits(ctx)
	}
	if !f.fitsRead {
		f.fits, f.fitsErr = s.runnerFits(ctx)
		f.fitsRead = true
	}
	return f.fits, f.fitsErr
}
