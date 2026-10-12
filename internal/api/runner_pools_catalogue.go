// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/setup"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// The pool list's enrichment (GET /runner-pools): per usable pool its run types, the limits a run of
// each type gets, its three barriers and the caller's own runners in it; for the caller, their
// allowance and the pool an untouched run resolves to. Every value comes from the function admission
// uses. Facts come from the hub and TargetInspector.ExecutorFacts, which caches a healthy executor's
// classes; one whose facts are not cached (a failing executor) is asked at most once per list,
// however many pools hold it. The list is advisory: create recomputes everything.

// dockerExecutor is the Docker substrate's executor name (its driverName), the one executor whose
// missing Vault the deployment's own KVM explains.
const dockerExecutor = "docker"

// poolListInput is what every row of one list is judged on, read once per request.
type poolListInput struct {
	ps      store.RunnerPoolStore
	ceiling governanceCeiling
	denied  []types.Placement
	// runType is the ?run_type= asked for; "" asks for every type a pool takes.
	runType types.RunnerPoolRunType
	// untouched is the size a run that names no policy resolves to (memberResources), as preflight's.
	untouched types.ResourceLimits
	// executors are the configured executors' facts, each read once per list however many pools hold it.
	executors map[string]executorRead
	// runnerNames are the caller's runners' names, read on the first self-hosted row that needs them.
	runnerNames map[uuid.UUID]string
	// org is the organisation's defaults: its barrier is every row's default when the row offers it.
	org types.RunnerPoolDefaults
	// slots are the counts of every pool the caller may use, read once per list.
	slots map[uuid.UUID]store.PoolCounts
}

func (s *Server) poolList(r *http.Request, ps store.RunnerPoolStore, rows []store.RunnerPoolCatalogueRow, subj callerSubjects,
	ceiling governanceCeiling, rt types.RunnerPoolRunType,
) (client.RunnerPoolList, *runRefusal) {
	ctx := r.Context()
	org, err := ps.GetRunnerPoolOrgDefaults(ctx)
	if err != nil {
		return client.RunnerPoolList{}, runServerError("read runner pool defaults", err)
	}
	in := &poolListInput{ps: ps, ceiling: ceiling, denied: s.deniedPlacements(ctx, ceiling), runType: rt, executors: map[string]executorRead{}, org: org}
	if res, _ := s.memberResources(ctx, ceiling.Spec.Resources, ceiling); res != nil {
		in.untouched = types.ResourceLimits{CPUMillis: res.CPUMillis, MemoryMiB: res.MemoryMiB}
	}
	out := client.RunnerPoolList{Pools: make([]client.RunnerPoolCatalogueEntry, 0, len(rows))}
	var permitted []types.RunnerPool
	owner := poolOwner(ctx)
	var usable []uuid.UUID
	for _, row := range rows {
		if subj.permits(row, owner) {
			usable = append(usable, row.Pool.ID)
		}
	}
	in.slots = s.slotCounts(ctx, usable)
	for _, row := range rows {
		if !subj.permits(row, owner) {
			// Listed for a manager only: they may manage it (or, another person's Self-Hosted pool, oversee
			// it), not use it, so nothing is computed for it.
			out.Pools = append(out.Pools, client.RunnerPoolCatalogueEntry{
				RunnerPoolChoice: poolState{availability: client.RunnerPoolUnavailable, reason: runnerpool.ReasonNotFound}.choice(row),
				RunTypes:         []types.RunnerPoolRunType{}, BarrierOptions: []client.BarrierOption{}, Owner: row.Pool.Owner,
			})
			continue
		}
		permitted = append(permitted, row.Pool)
		entry, refusal := s.catalogueEntry(r, in, row)
		if refusal != nil {
			return client.RunnerPoolList{}, refusal
		}
		out.Pools = append(out.Pools, entry)
	}
	var refusal *runRefusal
	if out.Default, refusal = s.poolDefault(r, permitted, org); refusal != nil {
		return client.RunnerPoolList{}, refusal
	}
	if out.Allowance, refusal = s.personAllowance(r, ceiling); refusal != nil {
		return client.RunnerPoolList{}, refusal
	}
	return out, nil
}

// catalogueEntry is one usable pool's row.
func (s *Server) catalogueEntry(r *http.Request, in *poolListInput, row store.RunnerPoolCatalogueRow) (client.RunnerPoolCatalogueEntry, *runRefusal) {
	ctx := r.Context()
	st, refusal := s.poolAvailability(ctx, in.ps, row, in.denied)
	if refusal != nil {
		return client.RunnerPoolCatalogueEntry{}, refusal
	}
	e := client.RunnerPoolCatalogueEntry{RunnerPoolChoice: st.choice(row), Owner: row.Pool.Owner,
		RunTypes: []types.RunnerPoolRunType{types.RunnerPoolRunBackground, types.RunnerPoolRunInteractive}, RunTypesSource: client.RunTypesSourceNone}
	if row.Pool.Limits != nil {
		e.RunTypes, e.RunTypesSource = row.Pool.Limits.RunTypes(), client.RunTypesSourcePool
	}
	want := e.RunTypes
	if in.runType != "" {
		want = nil
		if slices.Contains(e.RunTypes, in.runType) {
			want = []types.RunnerPoolRunType{in.runType}
		} else if e.Reason == "" {
			e.Availability, e.Reason = client.RunnerPoolUnavailable, runnerpool.ReasonRunTypeNotAllowed
		}
	}
	targets := st.own
	if row.Pool.Scope == types.RunnerPoolOrganization && st.availability == client.RunnerPoolAvailable {
		// Only through the inspector's facts, never a lone driver's own probe.
		if _, ok := s.targetInspector(); ok {
			targets, _ = executorTargets(row, s.configuredExecutors(), func(e string) (runner.TargetFacts, bool) { return in.executorFacts(ctx, s, e) })
		}
	}
	e.MainDrive = s.poolMainDriveFor(row, st.own, func(x string) (runner.TargetFacts, bool) { return in.executorFacts(ctx, s, x) })
	fit, _ := eligibleTargets(targets, placementDemand{floor: in.ceiling.Spec, reqCCOK: true})
	for _, rt := range want {
		sources, ref, err := s.limitSources(ctx, rt, &row.Pool, in.ceiling, in.untouched, selfHostedCapacity(row.Pool, fit, ""))
		if err != nil {
			return client.RunnerPoolCatalogueEntry{}, runServerError("resolve pool limits", err)
		}
		if ref != nil {
			continue // want holds only run types the pool takes
		}
		eff, err := runnerpool.Effective(rt, sources...)
		if err != nil {
			return client.RunnerPoolCatalogueEntry{}, runServerError("resolve pool limits", err)
		}
		e.Limits = append(e.Limits, eff)
	}
	e.BarrierOptions = runnerpool.BarrierOptions(barrierFacts(row.Pool, targets, in.ceiling.Spec.MinConfinementClass, in.org.Barrier))
	if c, ok := in.slots[row.Pool.ID]; ok {
		waitSec := 0
		if in.runType != "" {
			waitSec = defaultMaxWaitSec(in.runType)
		}
		e.Slots = poolSlots(row.Pool, c, waitSec)
	}
	// Membership, not candidacy: every runner of the caller's in the pool, whether or not it could take a
	// run now, so a person can read which pools each of their runners is in.
	if len(row.OwnRunnerIDs) > 0 {
		names, refusal := s.ownRunnerNames(r, in)
		if refusal != nil {
			return client.RunnerPoolCatalogueEntry{}, refusal
		}
		for _, id := range row.OwnRunnerIDs {
			e.MyRunners = append(e.MyRunners, client.PoolRunner{ID: id, Name: names[id], Online: s.runnerOnline(id)})
		}
	}
	return e, nil
}

// executorRead is one executor's facts as the list read them.
type executorRead struct {
	facts runner.TargetFacts
	known bool
}

// executorFacts reads an executor's facts at most once per list. The orchestrator caches a healthy
// executor's classes; a failing one is asked again on the next list, never once per pool.
func (in *poolListInput) executorFacts(ctx context.Context, s *Server, executor string) (runner.TargetFacts, bool) {
	r, ok := in.executors[executor]
	if !ok {
		r.facts, r.known = s.executorFacts(ctx, executor)
		in.executors[executor] = r
	}
	return r.facts, r.known
}

// ownRunnerNames is the caller's own runners by id, read once per list.
func (s *Server) ownRunnerNames(r *http.Request, in *poolListInput) (map[uuid.UUID]string, *runRefusal) {
	if in.runnerNames != nil {
		return in.runnerNames, nil
	}
	in.runnerNames = map[uuid.UUID]string{}
	if rs, ok := s.cfg.Store.(store.RunnerStore); ok {
		runners, err := rs.ListRunnersByOwner(r.Context(), poolOwner(r.Context()))
		if err != nil {
			return nil, runServerError("list runners", err)
		}
		for _, rn := range runners {
			in.runnerNames[rn.ID] = rn.Name
		}
	}
	return in.runnerNames, nil
}

// barrierFacts is what a pool's barriers are judged on from its candidates' own facts: the classes the
// candidates that are known enforce (the offeredClasses rule over every candidate, not only those a
// draft admits), at the governance floor.
func barrierFacts(pool types.RunnerPool, targets []poolTarget, floor, org types.ConfinementClass) runnerpool.BarrierFacts {
	f := runnerpool.BarrierFacts{Limits: pool.Limits, Floor: floor, Org: org}
	for _, t := range targets {
		if t.offline {
			continue
		}
		f.Known = true
		for _, c := range t.facts.Classes {
			if !slices.Contains(f.Offered, c) {
				f.Offered = append(f.Offered, c)
			}
		}
	}
	f.KVMMissing = f.Known && !slices.Contains(f.Offered, types.CC3) && kvmExplainsNoVault(pool, targets)
	return f
}

// kvmExplainsNoVault: a remote-provided pool every one of whose executors without Vault is this
// deployment's Docker substrate, on a host with no KVM, the fact /setup/status reports.
func kvmExplainsNoVault(pool types.RunnerPool, targets []poolTarget) bool {
	if pool.Scope != types.RunnerPoolOrganization {
		return false
	}
	for _, t := range targets {
		if t.executor != dockerExecutor && (t.offline || !slices.Contains(t.facts.Classes, types.CC3)) {
			return false
		}
	}
	return !setup.DetectPlatform().KVM
}

// draftBarrierOptions is Review's barriers for the run's resolved pool: the list's, plus this draft's
// policy floor and the Vault its access needs. An unpooled run has none.
func draftBarrierOptions(place runPlacement, ceiling governanceCeiling, floor types.RunPolicySpec, baseline composer.Baseline) []client.BarrierOption {
	if !place.pooled {
		return []client.BarrierOption{}
	}
	bf := barrierFacts(place.row, place.targets, ceiling.Spec.MinConfinementClass, place.orgBarrier)
	bf.PolicyFloor = floor.MinConfinementClass
	bf.AccessFloor = composer.RequiredConfinementFloor(floor, baseline) == types.CC3
	return runnerpool.BarrierOptions(bf)
}

// poolDefault is the pool a run that names none resolves to, by today's resolver; a default that no
// longer applies is said with whose it was, and none is nil.
func (s *Server) poolDefault(r *http.Request, permitted []types.RunnerPool, org types.RunnerPoolDefaults) (*client.PoolDefault, *runRefusal) {
	personal, refusal := s.personalPoolDefaults(r.Context())
	if refusal != nil {
		return nil, refusal
	}
	pool, ref := runnerpool.Resolve(permitted, runnerpool.Choice{}, personal, org)
	switch {
	case ref == nil:
		id := pool.ID
		return &client.PoolDefault{PoolID: &id, Selection: pool.Selection}, nil
	case ref.Reason == runnerpool.ReasonDefaultUnavailable:
		return &client.PoolDefault{Selection: ref.Source, Reason: ref.Reason}, nil
	}
	return nil, nil
}

// personAllowance is the caller's runs against their governance run cap: counts only. A caller whose
// runs stand outside governance has none.
func (s *Server) personAllowance(r *http.Request, ceiling governanceCeiling) (*client.PersonAllowance, *runRefusal) {
	if s.runUngoverned(r.Context()) {
		return nil, nil
	}
	active, err := s.cfg.Store.CountActiveRunsBy(r.Context(), principalFromRequest(r))
	if err != nil {
		return nil, runServerError("count active runs", err)
	}
	a := &client.PersonAllowance{Active: active}
	if n := ceiling.Limits.MaxConcurrentRuns; n > 0 {
		a.Max = &n
	}
	if ceiling.Profile != nil {
		a.ProfileName = ceiling.Profile.Name
	}
	return a, nil
}
