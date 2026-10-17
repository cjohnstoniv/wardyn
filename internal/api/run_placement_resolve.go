// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"maps"
	"net/http"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// Where a run goes (0.9 runner pools). stepPlacement is the one fold step that decides it, the same at
// create, Review and the policy preview: resolve the pool (runnerpool.Resolve), take that pool's own
// candidate set of both kinds, intersect it with fresh facts PER CANDIDATE, decide the kind (the
// person's own runners or the remote-hosted ones), and leave that kind's fully eligible set on the fold.
// Create alone then picks one target (runPlacement.apply, once the run id exists) and stamps it on the
// run row; dispatch routes to exactly that target. A pool is a label for a choice and never admission,
// and a user pool never makes another person's runner a candidate.

// runPlacement is stepPlacement's answer. The zero value is an unpooled run: a store that cannot hold
// pools keeps today's behaviour, and a server-originated launcher never reaches the fold at all.
type runPlacement struct {
	pooled bool
	pool   types.ResolvedRunnerPool
	// row is the catalogue row the pool resolved to; its limits are what admission holds the run to.
	row       types.RunnerPool
	placement types.Placement
	// filled: the pool, not the request, decided the placement.
	filled bool
	// remoteOffered: the pool decided on the person's own runner while a remote-hosted runner could also
	// have taken the run, so a local refusal can say how to run it there.
	remoteOffered bool
	// eligible is every candidate of the decided kind that survived the intersection, by target id,
	// never empty once pooled.
	eligible []poolTarget
	// targets is every candidate of the decided kind with its own facts, before any demand: Review's
	// barrier options.
	targets []poolTarget
	// orgBarrier is the organisation's default barrier (runner_pool_org_defaults), which an untouched
	// launch gets when it is at or above the floor and the pool offers it.
	orgBarrier types.ConfinementClass
	// choices is the caller's permitted catalogue with each pool's availability, for the two dry doors.
	choices []client.RunnerPoolChoice
	// limits are what this run may get, pooled or not; applied is the CPU and memory it gets.
	limits  types.EffectiveRunLimits
	applied client.AppliedResources
	// runnersFull: the run would wait in the pool's queue, and every runner that could take it already
	// holds its max_runs, so the dry doors answer that it waits.
	runnersFull bool
	// waitsForRoom: no runner of the pool that could take the run has room for it now (its Runs at once
	// or a total), so the run waits in the pool's queue rather than being refused.
	waitsForRoom bool
	// unbound is the size the run gets before a runner's per-run limit bounds it (a disk of 0: its
	// runner's per-run disk), set by stepPlacement on a launch that waits.
	unbound *types.RunnerAmounts
	// noLocal: the run waits on the pool's remote-hosted runners and the local-eligibility step refuses
	// it on the person's own (stepLocalEligibility records it on a launch that waits).
	noLocal bool
}

// placementInput is what the fold knows of the run when it resolves where it goes.
type placementInput struct {
	req      *createRunRequest
	floor    types.RunPolicySpec
	baseline composer.Baseline
	reqCC    types.ConfinementClass
	// reqCCOK is false when the request's class did not parse; the confinement step answers that.
	reqCCOK bool
	// ceiling is the caller's governance ceiling; untouched and ask are the size inputs runSize keeps
	// apart (resourceBaseline, resourceAsk).
	ceiling        governanceCeiling
	untouched, ask types.ResourceLimits
	// dryDoor: the run is a preflight or policy preview, which answer the caller's pool choices; create
	// never reads them, so a pool the run is not using cannot fail it.
	dryDoor bool
	// promoted: the queue's start of a promoted run, whose runner the promotion already found room on.
	promoted bool
	// drives: the run mounts drives, which no person's own runner does.
	drives bool
	// noLocal: a waiting run the local-eligibility step refused on the person's own runners
	// (queuedLaunch.NoLocal); set by the queue's verdict and by a promoted run's start, never by a launch.
	noLocal bool
	// policyDiskMiB is the main-drive size the run's policy writes (spec.resources.disk_mib), 0 for none:
	// with the request's own it sizes the disk each candidate is held to.
	policyDiskMiB int
}

// poolsPlaceRuns reports whether runner pools decide where this deployment's runs go: a store that holds
// pools and a wired runner. Otherwise every run is unpooled and keeps today's remote behaviour.
func (s *Server) poolsPlaceRuns() bool {
	_, ok := s.cfg.Store.(store.RunnerPoolStore)
	return ok && s.cfg.Runner != nil
}

func (s *Server) resolvePlacement(ctx context.Context, in placementInput) (runPlacement, *runRefusal) {
	f := in
	// A deployment with no runner wired is headless: runs are recorded and never placed, so there is no
	// target to choose. A pool named there is refused rather than ignored.
	if !s.poolsPlaceRuns() {
		if f.req.RunnerPoolID != "" {
			return runPlacement{}, runError(runnerpool.ReasonPoolsUnavailable.Status(), string(runnerpool.ReasonPoolsUnavailable), runnerpool.UnavailableServerMsg())
		}
		// Unpooled: the deployment and governance still size the run.
		limits, applied, refusal := s.runSize(ctx, f, nil, nil)
		return runPlacement{limits: limits, applied: applied}, refusal
	}
	ps := s.cfg.Store.(store.RunnerPoolStore)
	rows, refusal := s.permittedPools(ctx, ps, true)
	if refusal != nil {
		return runPlacement{}, refusal
	}
	personal, refusal := s.personalPoolDefaults(ctx)
	if refusal != nil {
		return runPlacement{}, refusal
	}
	org, err := ps.GetRunnerPoolOrgDefaults(ctx)
	if err != nil {
		return runPlacement{}, runServerError("read runner pool defaults", err)
	}
	// The request's placement no longer chooses a pool: it narrows the pool's runners to one kind
	// (kindTargets), after the pool is resolved.
	var choice runnerpool.Choice
	if f.req.RunnerPoolID != "" {
		id, _ := uuid.Parse(f.req.RunnerPoolID) // the contract step already refused a malformed id
		choice.PoolID = &id
	}
	permitted := make([]types.RunnerPool, len(rows))
	for i, row := range rows {
		permitted[i] = row.Pool
	}
	pool, ref := runnerpool.Resolve(permitted, choice, personal, org)
	if ref != nil {
		return runPlacement{}, poolRefusal(ref)
	}
	row, _ := catalogueRow(rows, pool.ID)
	targets, refusal := s.poolTargets(ctx, f.req, ps, row)
	if refusal != nil {
		return runPlacement{}, refusal
	}
	kinds, refusal := s.kindTargets(ctx, f, row.Pool, targets)
	if refusal != nil {
		return runPlacement{}, refusal
	}
	// The pool's own limits admit by the run's STORED run type. A request that names none (an older
	// client) never satisfies a pool that sets limits, and is refused here at every door.
	if _, ref := runnerpool.PoolSource(row.Pool, types.RunnerPoolRunType(f.req.Experience)); ref != nil {
		return runPlacement{}, poolRefusal(ref)
	}
	// A run that starts at once is held to each runner's room here, so it lands on one that has room; a
	// run that waits in the pool's queue is not (the promotion holds it), and neither is a promoted run's
	// start. When no runner of any kind has room, the run waits in the pool's queue, whatever the pool's
	// own cap (every pool can queue).
	capped := row.Pool.Limits != nil && row.Pool.Limits.MaxConcurrentRuns != nil
	demand := s.placementDemand(ctx, f)
	demand.holdRunnerCapacity = !capped && !f.promoted
	offered, offers, refusal := s.offerKinds(ctx, f, row.Pool, kinds, demand)
	if refusal != nil {
		return runPlacement{}, refusal
	}
	// The kind is decided here, once, before local eligibility judges a run bound for the person's own
	// runner: nothing after this step moves a run to the other kind.
	decision := placementFor(f.req.PlacementRequest(), offers, s.deniedPlacements(ctx, f.ceiling))
	// A launch waits when a kind it may use is refused only for room: an offline laptop does not hide a
	// remote-hosted runner that is merely full.
	waits := false
	if decision.Refusal != "" && demand.holdRunnerCapacity && anyAtCapacity(offers) {
		demand.holdRunnerCapacity, waits = false, true
		if offered, offers, refusal = s.offerKinds(ctx, f, row.Pool, kinds, demand); refusal != nil {
			return runPlacement{}, refusal
		}
		decision = placementFor(f.req.PlacementRequest(), offers, s.deniedPlacements(ctx, f.ceiling))
	}
	if decision.Refusal != "" {
		return runPlacement{}, targetRefusal(decision.Refusal, row.Pool.Name, pool.Scope)
	}
	var choices []client.RunnerPoolChoice
	if f.dryDoor {
		if choices, refusal = s.poolChoices(ctx, ps, rows, s.deniedPlacements(ctx, f.ceiling)); refusal != nil {
			return runPlacement{}, refusal
		}
	}
	chosen := offered[decision.Placement]
	picked := pickTargets(chosen.eligible, decision.Targets)
	remoteOffered := decision.Filled && decision.Placement == types.PlacementLocal && len(offers[types.PlacementRemote].Targets) > 0
	return runPlacement{pooled: true, pool: pool, row: row.Pool, placement: decision.Placement, filled: decision.Filled, remoteOffered: remoteOffered,
		eligible: picked, targets: chosen.targets, orgBarrier: org.Barrier, choices: choices, limits: chosen.limits, applied: chosen.applied,
		waitsForRoom: waits, runnersFull: (capped || waits) && len(picked) > 0 && !slices.ContainsFunc(picked, func(t poolTarget) bool { return !t.full })}, nil
}

// anyAtCapacity reports whether one of the kinds was refused only because its runners have no room now.
func anyAtCapacity(offers map[types.Placement]placementOffer) bool {
	for _, o := range offers {
		if o.Refusal == placement.ReasonRunnerAtCapacity {
			return true
		}
	}
	return false
}

// offerKinds is offerKind for each kind the run may use, in kindOrder.
func (s *Server) offerKinds(ctx context.Context, f placementInput, pool types.RunnerPool, kinds map[types.Placement][]poolTarget, demand placementDemand,
) (map[types.Placement]kindOffer, map[types.Placement]placementOffer, *runRefusal) {
	offered := map[types.Placement]kindOffer{}
	offers := map[types.Placement]placementOffer{}
	for _, kind := range kindOrder {
		ts, ok := kinds[kind]
		if !ok {
			continue
		}
		o, refusal := s.offerKind(ctx, f, pool, kind, ts, demand)
		if refusal != nil {
			return nil, nil, refusal
		}
		offered[kind], offers[kind] = o, offerOf(o.eligible, o.reason)
	}
	return offered, offers, nil
}

// kindTargets splits the pool's candidates by kind and keeps the kinds this run may use: the one its
// request asks for (a pin asks for local), then none its owner's profile denies, since deny_placements
// holds each kind, not the pool. A run with drives keeps only the remote-hosted kind when the pool has
// it, as no drive mounts on a person's own runner; with none it stays on the person's own runners for
// the drive step to refuse. A waiting run the local-eligibility step refused on the person's own runners
// (noLocal) keeps only the remote-hosted kind the same way. A kind the pool has no runner of is the kind
// mismatch, and a pool whose every kind is denied is the deny sentence for the first of them.
func (s *Server) kindTargets(ctx context.Context, f placementInput, pool types.RunnerPool, targets []poolTarget) (map[types.Placement][]poolTarget, *runRefusal) {
	kinds := map[types.Placement][]poolTarget{}
	for _, t := range targets {
		kinds[t.placement()] = append(kinds[t.placement()], t)
	}
	if asked := f.req.Placement; asked != "" {
		if len(kinds[asked]) == 0 {
			return nil, poolRefusal(&runnerpool.Refusal{Reason: runnerpool.ReasonMemberMismatch, Name: pool.Name, Scope: pool.Scope})
		}
		kinds = map[types.Placement][]poolTarget{asked: kinds[asked]}
	}
	var refused types.Placement
	for _, kind := range kindOrder {
		if _, ok := kinds[kind]; ok && slices.Contains(s.deniedPlacements(ctx, f.ceiling), kind) {
			refused = cmp.Or(refused, kind)
			delete(kinds, kind)
		}
	}
	if len(kinds) == 0 {
		return nil, s.placementDeniedRefusal(ctx, refused, f.ceiling)
	}
	if _, remote := kinds[types.PlacementRemote]; remote && (f.drives || f.noLocal) {
		delete(kinds, types.PlacementLocal)
	}
	return kinds, nil
}

// kindOrder is the order the pool offers its kinds in: the person's own runners first, which spend no
// shared capacity, then the remote-hosted ones.
var kindOrder = []types.Placement{types.PlacementLocal, types.PlacementRemote}

// kindOffer is what one kind of the pool's runners gives this run: its candidates before any demand,
// the ones that meet every demand at the size this kind gives, the first refusal when none does, and
// that size.
type kindOffer struct {
	targets, eligible []poolTarget
	reason            placement.Reason
	limits            types.EffectiveRunLimits
	applied           client.AppliedResources
}

// offerKind holds one kind's candidates to the run's demand. The size is bounded before any candidate
// is held to it: the kind's candidates that are online and can enforce the barrier give the runner
// capacity, the ask is applied, and only the APPLIED size is held per candidate, so an over-ask is
// reduced, never refused.
func (s *Server) offerKind(ctx context.Context, f placementInput, pool types.RunnerPool, kind types.Placement, targets []poolTarget, demand placementDemand) (kindOffer, *runRefusal) {
	// The person's own runners answer for themselves, so no deployment-wide sentence answers a class none
	// of them enforces: the class is held against each of them, the same at every door.
	if kind == types.PlacementLocal && f.reqCCOK {
		if _, _, err := enforcedConfinement(f.floor, f.reqCC, nil, "", f.baseline); err == nil {
			demand.reqCCOK = true
		}
	}
	// The disk the run asks for is its request's, else its policy's (mainDriveFor's order).
	requestDisk := 0
	if f.req.Resources != nil {
		requestDisk = f.req.Resources.DiskMiB
	}
	ask := types.RunnerAmounts{CPUMillis: int64(f.ask.CPUMillis), MemoryMiB: int64(f.ask.MemoryMiB), DiskMiB: int64(cmp.Or(requestDisk, f.policyDiskMiB))}
	fit, _ := eligibleTargets(targets, demand)
	// A promoted run's start is sized by the runner its promotion found room on, the size the promotion
	// admitted it at, so the start and the promotion agree.
	source := f.req.RunnerID
	if bound := promotedTarget(ctx); source == "" && slices.ContainsFunc(fit, func(t poolTarget) bool { return t.id == bound }) {
		source = bound
	}
	limits, applied, refusal := s.runSize(ctx, f, &pool, runnerSource(fit, source, ask))
	if refusal != nil {
		return kindOffer{}, refusal
	}
	// The main drive is sized here as stepMainDrive sizes it (mainDriveFor on this kind's disk limit), so
	// each candidate is held to the disk the run reserves: one whose per-run disk is under it is passed
	// over, and one whose disk total is spent has no room, as for CPU and memory.
	demand.cpu, demand.mem = int64(applied.CPUMillis.Applied), int64(applied.MemoryMiB.Applied)
	demand.disk = int64(mainDriveFor(f.policyDiskMiB, requestDisk, limits.DiskMiB).MiB)
	eligible, reason := eligibleTargets(targets, demand)
	// A pin names one of the caller's own candidates (pinRefusal has refused every other id), so when that
	// candidate fails the demand the answer is ITS reason, not "no such runner" because another is fine.
	// A pinned runner with no room now is no refusal: the run waits for it (resolvePlacement).
	if pin := f.req.RunnerID; pin != "" {
		if i := slices.IndexFunc(targets, func(t poolTarget) bool { return t.id == pin }); i >= 0 {
			switch why := demand.refusal(targets[i]); {
			case why == placement.ReasonRunnerAtCapacity:
				return kindOffer{targets: targets, reason: why, limits: limits, applied: applied}, nil
			case why != "":
				return kindOffer{}, targetRefusal(why, pool.Name, pool.Scope)
			}
		}
	}
	return kindOffer{targets: targets, eligible: eligible, reason: reason, limits: limits, applied: applied}, nil
}

// queues says a run bound for this pool waits for a slot instead of starting at once: the pool sets a
// concurrent-run cap, or no runner that could take the run has room for it now. An unpooled run never
// queues.
func (p runPlacement) queues() bool {
	return p.pooled && (p.waitsForRoom || (p.row.Limits != nil && p.row.Limits.MaxConcurrentRuns != nil))
}

func poolRefusal(ref *runnerpool.Refusal) *runRefusal {
	return runError(ref.Reason.Status(), string(ref.Reason), ref.Message())
}

// poolChoices is the permitted catalogue as the two dry doors show it, each pool's availability the
// pool list's (poolAvailability). A target is never named here, because the launch picks it from the
// run id it allocates.
func (s *Server) poolChoices(ctx context.Context, ps store.RunnerPoolStore, rows []store.RunnerPoolCatalogueRow, denied []types.Placement) ([]client.RunnerPoolChoice, *runRefusal) {
	out := make([]client.RunnerPoolChoice, 0, len(rows))
	read := s.onceExecutorFacts(ctx)
	var names map[uuid.UUID]string // the caller's runners' names, read on the first row that has any
	for _, row := range rows {
		st, refusal := s.poolAvailability(ctx, ps, row, denied)
		if refusal != nil {
			return nil, refusal
		}
		choice := st.choice(row)
		choice.MainDrive = s.poolMainDriveFor(row, st.own, read)
		if len(row.OwnRunnerIDs) > 0 {
			if names == nil {
				if names, refusal = s.callerRunnerNames(ctx); refusal != nil {
					return nil, refusal
				}
			}
			choice.MyRunners = s.myRunnersIn(row, names)
		}
		out = append(out, choice)
	}
	return out, nil
}

// placementOffer is what one placement can give this run: the fully eligible targets, or the reason
// there are none. Room: one of the targets holds fewer runs than its max_runs, as last read.
type placementOffer struct {
	Targets []string
	Refusal placement.Reason
	Room    bool
}

func offerOf(eligible []poolTarget, reason placement.Reason) placementOffer {
	if len(eligible) == 0 {
		return placementOffer{Refusal: cmp.Or(reason, placement.ReasonPlacementUnavailable)}
	}
	ids := make([]string, len(eligible))
	for i, t := range eligible {
		ids[i] = t.id
	}
	return placementOffer{Targets: ids, Room: slices.ContainsFunc(eligible, func(t poolTarget) bool { return !t.full })}
}

// placementDecision is placementFor's answer. Targets is the set the pool's deterministic selection
// chooses among (a pinned runner narrows it to that one), never empty unless Refusal is set.
type placementDecision struct {
	Placement types.Placement
	Filled    bool
	Targets   []string
	Refusal   placement.Reason
}

// placementFor is the pure placement table (design §6.2). requested is the request's placement and
// runner pin; eligible is what each placement can offer; denied lists placements a ceiling denies.
// With pools the table is run for the kinds of runner the selected pool holds, and the "more than one
// runner online" row, runner_ambiguous, is answered by the pool's deterministic selection instead of a
// refusal: there is no ambiguity once a pool is chosen, and nothing is ever picked by list order.
func placementFor(requested placement.Request, eligible map[types.Placement]placementOffer, denied []types.Placement) placementDecision {
	asked := requested.Placement
	if asked == "" {
		return placementForUnasked(eligible, denied)
	}
	if slices.Contains(denied, asked) {
		return placementDecision{Refusal: placement.ReasonPlacementDenied}
	}
	offer := eligible[asked]
	if len(offer.Targets) == 0 {
		return placementDecision{Refusal: cmp.Or(offer.Refusal, placement.ReasonPlacementUnavailable)}
	}
	targets := offer.Targets
	if pin := requested.RunnerID; pin != "" {
		if !slices.Contains(targets, pin) {
			return placementDecision{Refusal: placement.ReasonRunnerNotFound}
		}
		targets = []string{pin}
	}
	return placementDecision{Placement: asked, Targets: targets}
}

// placementForUnasked: no placement was asked for, so the pool decides among the open placements. The
// person's own runners come first when one of them has room, else the remote-hosted runners: the person
// put their own computer in the pool to use it, and it spends no shared capacity. None open answers with
// the first refusal in that order (local, remote).
func placementForUnasked(eligible map[types.Placement]placementOffer, denied []types.Placement) placementDecision {
	var open []types.Placement
	var first placement.Reason
	for _, p := range kindOrder {
		offer, ok := eligible[p]
		switch {
		case slices.Contains(denied, p):
			first = cmp.Or(first, placement.ReasonPlacementDenied)
		case ok && len(offer.Targets) > 0:
			open = append(open, p)
		case ok:
			first = cmp.Or(first, offer.Refusal)
		}
	}
	if len(open) == 0 {
		return placementDecision{Refusal: cmp.Or(first, placement.ReasonPlacementUnavailable)}
	}
	chosen := open[0]
	if len(open) > 1 && !eligible[chosen].Room {
		chosen = open[1]
	}
	return placementDecision{Placement: chosen, Filled: true, Targets: eligible[chosen].Targets}
}

// pickTargets keeps the eligible targets the decision names, in the decision's order.
func pickTargets(eligible []poolTarget, ids []string) []poolTarget {
	var out []poolTarget
	for _, id := range ids {
		if i := slices.IndexFunc(eligible, func(t poolTarget) bool { return t.id == id }); i >= 0 {
			out = append(out, eligible[i])
		}
	}
	return out
}

// poolBinding is the target a run was bound to, ready to stamp on its row.
type poolBinding struct {
	runnerID *uuid.UUID
	executor string
}

// bind picks the run's one target among the eligible ones that can also enforce the class the run
// ended up at, by the deterministic selection over the run id. The class check is the last
// intersection: the enforced class is known only after the confinement step. An empty set refuses; it
// never falls back to a candidate that cannot. prefer (a target id, "" for none), when one of them, is
// kept: the runner of either kind a promotion bound the run to and found room on.
func (p runPlacement) bind(runID uuid.UUID, enforced types.ConfinementClass, prefer string) (poolBinding, *runRefusal) {
	able := p.able(enforced)
	ids := make([]string, len(able))
	for i, t := range able {
		ids[i] = t.id
	}
	chosen := runnerpool.Select(p.pool.ID, runID, runnerpool.SortedUnique(ids))
	if prefer != "" && slices.Contains(ids, prefer) {
		chosen = prefer
	}
	i := slices.IndexFunc(able, func(t poolTarget) bool { return t.id == chosen })
	if i < 0 {
		return poolBinding{}, targetRefusal(placement.ReasonPlacementCapability, p.pool.Name, p.pool.Scope)
	}
	return poolBinding{runnerID: able[i].runnerID, executor: able[i].executor}, nil
}

// able is the eligible targets that enforce the class the run ended up at ("" for any).
func (p runPlacement) able(enforced types.ConfinementClass) []poolTarget {
	var out []poolTarget
	for _, t := range p.eligible {
		if enforced == "" || slices.Contains(t.facts.Classes, enforced) {
			out = append(out, t)
		}
	}
	return out
}

// otherRunners is the registered runners the run could be bound to in place of chosen: the ones its
// start may fall back to when chosen has no room under its lock.
func (p runPlacement) otherRunners(enforced types.ConfinementClass, chosen *uuid.UUID) []uuid.UUID {
	var out []uuid.UUID
	for _, t := range p.able(enforced) {
		if t.runnerID != nil && (chosen == nil || *t.runnerID != *chosen) {
			out = append(out, *t.runnerID)
		}
	}
	return out
}

// otherRemotes is the remote-hosted runners the run could be bound to in place of chosen, for the same
// fall-back at its start.
func (p runPlacement) otherRemotes(enforced types.ConfinementClass, chosen string) []string {
	var out []string
	for _, t := range p.able(enforced) {
		if t.runnerID == nil && t.executor != chosen {
			out = append(out, t.executor)
		}
	}
	return out
}

// eligibleRunners is the registered runners among the run's eligible targets: the person's own runners
// one of which a self-hosted run is bound to.
func (p runPlacement) eligibleRunners() []uuid.UUID {
	var out []uuid.UUID
	for _, t := range p.eligible {
		if t.runnerID != nil {
			out = append(out, *t.runnerID)
		}
	}
	return out
}

// candidateClasses is every barrier some eligible target enforces, the pool's allowed barriers aside
// (barrierRefusal answers those): what a pooled run's class can be held to before bind picks the one
// target that enforces it.
func (p runPlacement) candidateClasses() []types.ConfinementClass {
	var out []types.ConfinementClass
	for _, t := range p.eligible {
		for _, c := range t.facts.Classes {
			if !slices.Contains(out, c) {
				out = append(out, c)
			}
		}
	}
	return out
}

// barrierRefusal holds the barrier the run ended up at to the pool's allowed barriers. The class is
// known only after the confinement step, so that step asks.
func (p runPlacement) barrierRefusal(enforced types.ConfinementClass) *runRefusal {
	if !p.pooled || enforced == "" {
		return nil
	}
	if ref := runnerpool.CheckBarrier(p.row, enforced); ref != nil {
		return poolRefusal(ref)
	}
	return nil
}

// offeredClasses is the barriers the run's eligible candidates enforce, strongest last, nil for an
// unpooled run. The default class of a pooled run is chosen from it, so the class a door promises is
// one a candidate the run can be bound to enforces.
func (p runPlacement) offeredClasses() []types.ConfinementClass {
	if !p.pooled {
		return nil
	}
	out := []types.ConfinementClass{}
	for _, t := range p.eligible {
		for _, c := range t.facts.Classes {
			// A pool that sets limits allows only its own barriers: the default is the strongest class
			// that is both enforced by a candidate and allowed by the pool.
			if p.row.Limits != nil && !p.row.Limits.AllowsBarrier(c) {
				continue
			}
			if !slices.Contains(out, c) {
				out = append(out, c)
			}
		}
	}
	slices.SortFunc(out, func(a, b types.ConfinementClass) int { return cmp.Compare(a.Rank(), b.Rank()) })
	return out
}

// apply stamps the placement, the evidence source and the pool binding on the run row, once, before
// the row is inserted. An unpooled run is left exactly as it was. A runner of either kind already on run
// is kept when it is still one the run can be bound to.
func (p runPlacement) apply(run *types.AgentRun, enforced types.ConfinementClass) *runRefusal {
	if !p.pooled {
		return nil
	}
	prefer := run.RunnerPoolExecutor
	if run.RunnerID != nil {
		prefer = run.RunnerID.String()
	}
	b, refusal := p.bind(run.ID, enforced, prefer)
	if refusal != nil {
		return refusal
	}
	id := p.pool.ID
	run.Placement, run.PlacementFilled, run.EvidenceSource = p.placement, p.filled, placement.EvidenceFor(p.placement)
	run.RunnerID, run.RunnerPoolExecutor = b.runnerID, b.executor
	run.RunnerPoolID, run.RunnerPoolRevision, run.RunnerPoolSelection = &id, p.pool.Revision, p.pool.Selection
	return nil
}

// facts are the two dry doors' answer: the resolved pool and the caller's permitted catalogue. Both
// are empty for an unpooled run, and neither names a target.
func (p runPlacement) facts() (*client.ResolvedRunnerPool, []client.RunnerPoolChoice) {
	if !p.pooled {
		return nil, []client.RunnerPoolChoice{}
	}
	pool := p.pool
	return &pool, p.choices
}

// sizeFacts are the run's limits and applied size as the two dry doors show them, pooled or not.
func (p runPlacement) sizeFacts() (*types.EffectiveRunLimits, *client.AppliedResources) {
	limits, applied := p.limits, p.applied
	return &limits, &applied
}

// auditFields are the applied size and where the run goes, as run.create records them: the placement
// always (empty for a run nothing placed), and for a pooled run the pool and the binding. runnerName is
// the bound runner's display name, "" when there is none or it could not be read.
func (p runPlacement) auditFields(run types.AgentRun, runnerName string) map[string]any {
	data := map[string]any{
		"resources_applied": p.applied, "placement": run.Placement, "placement_filled": run.PlacementFilled, "evidence_source": run.EvidenceSource,
	}
	if p.pooled {
		maps.Copy(data, map[string]any{
			"runner_pool_id": p.pool.ID, "runner_pool_name": p.pool.Name, "runner_pool_revision": run.RunnerPoolRevision,
			"runner_pool_selection": run.RunnerPoolSelection, "reserved": run.Reserved,
		})
	}
	if run.RunnerID != nil {
		data["runner_id"] = run.RunnerID
	}
	if run.RunnerPoolExecutor != "" {
		data["remote_runner_id"] = run.RunnerPoolExecutor
	}
	if runnerName != "" {
		data["runner_name"] = runnerName
	}
	return data
}

// targetRefusal words a placement reason for the person: the pool is named, no runner of anyone else.
func targetRefusal(reason placement.Reason, pool string, scope types.RunnerPoolScope) *runRefusal {
	switch reason {
	case placement.ReasonRunnerOffline:
		if scope == types.RunnerPoolOrganization {
			return poolRefusal(&runnerpool.Refusal{Reason: runnerpool.ReasonNoEligibleMember, Name: pool, Scope: scope})
		}
		return runError(reason.Status(), string(reason), runnerpool.NoOnlineRunnerMsg(pool))
	case placement.ReasonPlacementCapability:
		return runError(reason.Status(), string(reason), "No runner in "+pool+" offers the barrier or size this run needs. Choose another pool, or ask for less.")
	case placement.ReasonPlacementUnavailable:
		return runError(http.StatusUnprocessableEntity, string(reason), "Your own runner is not available: runners are switched off on this server.")
	case placement.ReasonRunnerAtCapacity:
		return runError(reason.Status(), string(reason), runnerpool.RunnerAtCapacityMsg(pool))
	case placement.ReasonRunnerLimitsUnset:
		return runError(reason.Status(), string(reason), runnerpool.RunnerLimitsUnsetMsg(pool))
	}
	return runError(reason.Status(), string(reason), "This run cannot be placed in "+pool+".")
}

// placementDemand is what the run asks of its target, held against each candidate's own facts.
type placementDemand struct {
	floor    types.RunPolicySpec
	reqCC    types.ConfinementClass
	reqCCOK  bool // false: the request's class did not parse, and the confinement step answers it
	baseline composer.Baseline
	// cpu, mem and disk are what the run reserves on its runner; 0 is not known yet.
	cpu, mem, disk int64
	// holdRunnerCapacity refuses a runner with no room for the run now (fitRefusal).
	holdRunnerCapacity bool
	// fits is each runner's declared limits and In use, by target id (runnerFits).
	fits map[string]runnerFit
}

// eligibleTargets keeps the candidates that meet EVERY demand on their OWN facts and answers the first
// refusing candidate's reason when none does. It is an intersection over each candidate and never a
// union: a pool's strongest barrier and its largest machine are not summed into a target that does not
// exist. A candidate whose facts are unknown is refused as offline, never read as zero or as everything.
// A runner refused only for being at its max_runs could take the run once one of its runs ends, so that
// is the answer whenever one candidate was refused for it alone.
func eligibleTargets(targets []poolTarget, d placementDemand) ([]poolTarget, placement.Reason) {
	var out []poolTarget
	var first placement.Reason
	for _, t := range targets {
		reason := d.refusal(t)
		if reason == "" {
			out = append(out, d.withFit(t))
			continue
		}
		if reason == placement.ReasonRunnerAtCapacity {
			first = reason
		}
		first = cmp.Or(first, reason)
	}
	return out, first
}

func (d placementDemand) refusal(t poolTarget) placement.Reason {
	if t.offline {
		return placement.ReasonRunnerOffline
	}
	if d.reqCCOK {
		// No org default here: it is chosen only from the candidate's own classes, so it never changes
		// whether the resulting class is enforced.
		enforced, _, err := enforcedConfinement(d.floor, d.reqCC, t.facts.Classes, "", d.baseline)
		if err != nil || (enforced != "" && !slices.Contains(t.facts.Classes, enforced)) {
			return placement.ReasonPlacementCapability
		}
	}
	return d.fitRefusal(t)
}
