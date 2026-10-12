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
// candidate set, intersect it with fresh facts PER CANDIDATE, and leave the fully eligible set on the
// fold. Create alone then picks one target (runPlacement.apply, once the run id exists) and stamps it on
// the run row; dispatch routes to exactly that target. A pool is a label for a choice and never
// admission, and a self-hosted pool never makes another person's runner a candidate.

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
	// eligible is every candidate that survived the intersection, by target id, never empty once pooled.
	eligible []poolTarget
	// targets is every candidate with its own facts, before any demand: Review's barrier options.
	targets []poolTarget
	// choices is the caller's permitted catalogue with each pool's availability, for the two dry doors.
	choices []client.RunnerPoolChoice
	// limits are what this run may get, pooled or not; applied is the CPU and memory it gets.
	limits  types.EffectiveRunLimits
	applied client.AppliedResources
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
	choice := runnerpool.Choice{Hosting: hostingOfPlacement(f.req.Placement)}
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
	// deny_placements holds the placement the pool decides, not the request's: a member who denies remote
	// launches on a self-hosted pool, and one whose default is a remote pool is refused like an explicit ask.
	if refusal := s.placementDeniedRefusal(ctx, pool.HostingType.Placement(), f.ceiling); refusal != nil {
		return runPlacement{}, refusal
	}
	row, _ := catalogueRow(rows, pool.ID)
	// The pool's own limits admit by the run's STORED run type. A request that names none (an older
	// client) never satisfies a pool that sets limits, and is refused here at every door.
	if _, ref := runnerpool.PoolSource(row.Pool, types.RunnerPoolRunType(f.req.Experience)); ref != nil {
		return runPlacement{}, poolRefusal(ref)
	}
	targets, refusal := s.poolTargets(ctx, f.req, ps, row)
	if refusal != nil {
		return runPlacement{}, refusal
	}
	// The size is bounded before any candidate is held to it: the candidates that are online and can
	// enforce the barrier give the runner capacity, the ask is applied, and only the APPLIED size is
	// held per candidate, so an over-ask is reduced, never refused.
	demand := s.placementDemand(ctx, f)
	// A self-hosted pool offers only the person's own runners, so no deployment-wide sentence answers a
	// class none of them enforces: the class is held against each of them, the same at every door.
	if pool.HostingType == types.RunnerPoolSelfHosted && f.reqCCOK {
		if _, err := enforcedConfinement(f.floor, f.reqCC, nil, f.baseline); err == nil {
			demand.reqCCOK = true
		}
	}
	fit, _ := eligibleTargets(targets, demand)
	limits, applied, refusal := s.runSize(ctx, f, &row.Pool, selfHostedCapacity(row.Pool, fit, f.req.RunnerID))
	if refusal != nil {
		return runPlacement{}, refusal
	}
	demand.cpu, demand.mem = int64(applied.CPUMillis.Applied), int64(applied.MemoryMiB.Applied)
	eligible, reason := eligibleTargets(targets, demand)
	// A pin names one of the caller's own candidates (pinRefusal has refused every other id), so when that
	// candidate fails the demand the answer is ITS reason, not "no such runner" because another is fine.
	if pin := f.req.RunnerID; pin != "" {
		if i := slices.IndexFunc(targets, func(t poolTarget) bool { return t.id == pin }); i >= 0 {
			if why := demand.refusal(targets[i]); why != "" {
				return runPlacement{}, targetRefusal(why, row.Pool.Name, pool.HostingType)
			}
		}
	}
	var choices []client.RunnerPoolChoice
	if f.dryDoor {
		if choices, refusal = s.poolChoices(ctx, ps, rows, s.deniedPlacements(ctx, f.ceiling)); refusal != nil {
			return runPlacement{}, refusal
		}
	}
	offers := map[types.Placement]placementOffer{pool.HostingType.Placement(): offerOf(eligible, reason)}
	decision := placementFor(f.req.PlacementRequest(), offers, s.deniedPlacements(ctx, f.ceiling))
	if decision.Refusal != "" {
		return runPlacement{}, targetRefusal(decision.Refusal, row.Pool.Name, pool.HostingType)
	}
	return runPlacement{pooled: true, pool: pool, row: row.Pool, placement: decision.Placement, filled: decision.Filled,
		eligible: pickTargets(eligible, decision.Targets), targets: targets, choices: choices, limits: limits, applied: applied}, nil
}

// queues says a run bound for this pool waits for a slot instead of starting at once: the pool sets a
// concurrent-run cap. A pool with no cap never has a queue, and neither does an unpooled run.
func (p runPlacement) queues() bool {
	return p.pooled && p.row.Limits != nil && p.row.Limits.MaxConcurrentRuns != nil
}

// hostingOfPlacement is the hosting type a request's placement names; the pool decides when it is absent.
func hostingOfPlacement(p types.Placement) types.RunnerPoolHosting {
	switch p {
	case types.PlacementRemote:
		return types.RunnerPoolRemoteProvided
	case types.PlacementLocal:
		return types.RunnerPoolSelfHosted
	}
	return ""
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
	for _, row := range rows {
		st, refusal := s.poolAvailability(ctx, ps, row, denied)
		if refusal != nil {
			return nil, refusal
		}
		choice := st.choice(row)
		choice.MainDrive = s.poolMainDriveFor(row, st.own, read)
		out = append(out, choice)
	}
	return out, nil
}

// placementOffer is what one placement can give this run: the fully eligible targets, or the reason
// there are none.
type placementOffer struct {
	Targets []string
	Refusal placement.Reason
}

func offerOf(eligible []poolTarget, reason placement.Reason) placementOffer {
	if len(eligible) == 0 {
		return placementOffer{Refusal: cmp.Or(reason, placement.ReasonPlacementUnavailable)}
	}
	ids := make([]string, len(eligible))
	for i, t := range eligible {
		ids[i] = t.id
	}
	return placementOffer{Targets: ids}
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
// With pools the table is run for the one placement the selected pool serves, and the "more than one
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

// placementForUnasked: no placement was asked for, so exactly one eligible placement is filled in, two
// are placement_required, and none answers with the first refusal in placement order (remote, local).
func placementForUnasked(eligible map[types.Placement]placementOffer, denied []types.Placement) placementDecision {
	var open []types.Placement
	var first placement.Reason
	for _, p := range []types.Placement{types.PlacementRemote, types.PlacementLocal} {
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
	switch len(open) {
	case 0:
		return placementDecision{Refusal: cmp.Or(first, placement.ReasonPlacementUnavailable)}
	case 1:
		return placementDecision{Placement: open[0], Filled: true, Targets: eligible[open[0]].Targets}
	}
	return placementDecision{Refusal: placement.ReasonPlacementRequired}
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
// never falls back to a candidate that cannot.
func (p runPlacement) bind(runID uuid.UUID, enforced types.ConfinementClass) (poolBinding, *runRefusal) {
	var able []poolTarget
	for _, t := range p.eligible {
		if enforced == "" || slices.Contains(t.facts.Classes, enforced) {
			able = append(able, t)
		}
	}
	ids := make([]string, len(able))
	for i, t := range able {
		ids[i] = t.id
	}
	chosen := runnerpool.Select(p.pool.ID, runID, runnerpool.SortedUnique(ids))
	i := slices.IndexFunc(able, func(t poolTarget) bool { return t.id == chosen })
	if i < 0 {
		return poolBinding{}, targetRefusal(placement.ReasonPlacementCapability, p.pool.Name, p.pool.HostingType)
	}
	return poolBinding{runnerID: able[i].runnerID, executor: able[i].executor}, nil
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
// the row is inserted. An unpooled run is left exactly as it was.
func (p runPlacement) apply(run *types.AgentRun, enforced types.ConfinementClass) *runRefusal {
	if !p.pooled {
		return nil
	}
	b, refusal := p.bind(run.ID, enforced)
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

// auditFields are the applied size and, for a pooled run, the binding, as run.create records them.
func (p runPlacement) auditFields(run types.AgentRun) map[string]any {
	data := map[string]any{"resources_applied": p.applied}
	if !p.pooled {
		return data
	}
	maps.Copy(data, map[string]any{
		"placement": run.Placement, "placement_filled": run.PlacementFilled, "evidence_source": run.EvidenceSource,
		"runner_pool_id": p.pool.ID, "runner_pool_revision": run.RunnerPoolRevision, "runner_pool_selection": run.RunnerPoolSelection,
	})
	if run.RunnerID != nil {
		data["runner_id"] = run.RunnerID
	}
	if run.RunnerPoolExecutor != "" {
		data["runner_pool_executor"] = run.RunnerPoolExecutor
	}
	return data
}

// targetRefusal words a placement reason for the person: the pool is named, no runner of anyone else.
func targetRefusal(reason placement.Reason, pool string, hosting types.RunnerPoolHosting) *runRefusal {
	switch reason {
	case placement.ReasonRunnerOffline:
		if hosting == types.RunnerPoolRemoteProvided {
			return poolRefusal(&runnerpool.Refusal{Reason: runnerpool.ReasonNoEligibleMember, Name: pool, Hosting: hosting})
		}
		return runError(reason.Status(), string(reason), runnerpool.NoOnlineRunnerMsg(pool))
	case placement.ReasonPlacementCapability:
		return runError(reason.Status(), string(reason), "No runner in "+pool+" offers the barrier or size this run needs. Choose another pool, or ask for less.")
	case placement.ReasonPlacementUnavailable:
		return runError(http.StatusUnprocessableEntity, string(reason), "Your own runner is not available: runners are switched off on this server.")
	}
	return runError(reason.Status(), string(reason), "This run cannot be placed in "+pool+".")
}

// placementDemand is what the run asks of its target, held against each candidate's own facts.
type placementDemand struct {
	floor    types.RunPolicySpec
	reqCC    types.ConfinementClass
	reqCCOK  bool // false: the request's class did not parse, and the confinement step answers it
	baseline composer.Baseline
	cpu, mem int64
}

// eligibleTargets keeps the candidates that meet EVERY demand on their OWN facts and answers the first
// refusing candidate's reason when none does. It is an intersection over each candidate and never a
// union: a pool's strongest barrier and its largest machine are not summed into a target that does not
// exist. A candidate whose facts are unknown is refused as offline, never read as zero or as everything.
func eligibleTargets(targets []poolTarget, d placementDemand) ([]poolTarget, placement.Reason) {
	var out []poolTarget
	var first placement.Reason
	for _, t := range targets {
		reason := d.refusal(t)
		if reason == "" {
			out = append(out, t)
			continue
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
		enforced, err := enforcedConfinement(d.floor, d.reqCC, t.facts.Classes, d.baseline)
		if err != nil || (enforced != "" && !slices.Contains(t.facts.Classes, enforced)) {
			return placement.ReasonPlacementCapability
		}
	}
	if t.facts.CapacityAdvertised && !(placement.Capacity{CPUMillisMax: t.facts.CPUMillisMax, MemoryMiBMax: t.facts.MemoryMiBMax}).Fits(d.cpu, d.mem) {
		return placement.ReasonPlacementCapability
	}
	return ""
}
