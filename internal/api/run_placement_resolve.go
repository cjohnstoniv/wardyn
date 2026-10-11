// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
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
	// choices is the caller's permitted catalogue with each pool's availability, for the two dry doors.
	choices []client.RunnerPoolChoice
}

// placementInput is what the fold knows of the run when it resolves where it goes.
type placementInput struct {
	req      *createRunRequest
	floor    types.RunPolicySpec
	baseline composer.Baseline
	reqCC    types.ConfinementClass
	// reqCCOK is false when the request's class did not parse; the confinement step answers that.
	reqCCOK bool
}

func (s *Server) resolvePlacement(ctx context.Context, in placementInput) (runPlacement, *runRefusal) {
	f := in
	ps, ok := s.cfg.Store.(store.RunnerPoolStore)
	// A deployment with no runner wired is headless: runs are recorded and never placed, so there is no
	// target to choose. A pool named there is refused rather than ignored.
	if !ok || s.cfg.Runner == nil {
		if f.req.RunnerPoolID != "" {
			return runPlacement{}, runError(runnerpool.ReasonPoolsUnavailable.Status(), string(runnerpool.ReasonPoolsUnavailable), runnerpool.UnavailableServerMsg())
		}
		return runPlacement{}, nil
	}
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
	demand := s.placementDemand(ctx, f)
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
	offers := map[types.Placement]placementOffer{pool.HostingType.Placement(): offerOf(eligible, reason)}
	decision := placementFor(f.req.PlacementRequest(), offers, nil)
	if decision.Refusal != "" {
		return runPlacement{}, targetRefusal(decision.Refusal, row.Pool.Name, pool.HostingType)
	}
	return runPlacement{pooled: true, pool: pool, row: row.Pool, placement: decision.Placement, filled: decision.Filled,
		eligible: pickTargets(eligible, decision.Targets), choices: s.poolChoices(rows)}, nil
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

// poolChoices is the permitted catalogue as the two dry doors show it. The candidate sets and limits
// are what a launch will intersect; a target is never named here, because the launch picks it from the
// run id it allocates.
func (s *Server) poolChoices(rows []store.RunnerPoolCatalogueRow) []client.RunnerPoolChoice {
	out := make([]client.RunnerPoolChoice, 0, len(rows))
	for _, row := range rows {
		availability, reason := s.poolAvailability(row)
		out = append(out, client.RunnerPoolChoice{ID: row.Pool.ID, Name: row.Pool.Name, HostingType: row.Pool.HostingType, Availability: availability, Reason: reason})
	}
	return out
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

// auditFields are the binding as run.create records it.
func (p runPlacement) auditFields(run types.AgentRun) map[string]any {
	if !p.pooled {
		return nil
	}
	data := map[string]any{
		"placement": run.Placement, "placement_filled": run.PlacementFilled, "evidence_source": run.EvidenceSource,
		"runner_pool_id": p.pool.ID, "runner_pool_revision": run.RunnerPoolRevision, "runner_pool_selection": run.RunnerPoolSelection,
	}
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
		return runError(reason.Status(), string(reason), "None of your runners in "+pool+" is online right now. Start one, or choose another pool.")
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
