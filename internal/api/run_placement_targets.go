// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// poolTarget is one candidate of the resolved pool. id is the stable target id the selection scores:
// a runner's id, or an executor's configured id.
type poolTarget struct {
	id       string
	runnerID *uuid.UUID
	executor string
	facts    runner.TargetFacts
	// offline: not connected, or it has advertised nothing, so its facts are unknown.
	offline bool
	// full: a registered runner already holding its max_runs of sandbox-holding runs, as last read
	// without a lock. The insert, the promotion and the start count again under the runner's row lock.
	full bool
}

// personalPoolDefaults is the signed-in person's own saved defaults: empty for a caller who is no
// person (the admin token, a delegate, a device), who has none to inherit. A stored document that
// cannot be read is an error, never an empty one, so a configured default is not silently dropped.
func (s *Server) personalPoolDefaults(ctx context.Context) (types.RunnerPoolDefaults, *runRefusal) {
	var d types.RunnerPoolDefaults
	owner := poolOwner(ctx)
	prefs, ok := s.cfg.Store.(store.PrincipalPrefStore)
	if owner == "" || !ok {
		return d, nil
	}
	raw, err := prefs.GetPrincipalPref(ctx, owner, types.RunnerPoolDefaultsPrefKey)
	if errors.Is(err, store.ErrNotFound) {
		return d, nil
	}
	if err == nil {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		err = dec.Decode(&d)
	}
	if err != nil {
		return types.RunnerPoolDefaults{}, runServerError("read runner pool defaults", err)
	}
	return d, nil
}

// poolOwner is the person whose own runners a self-hosted pool may offer: the signed-in human of this
// request, and nobody for a bearer, a delegate, a device or the identity provider's connector, none of
// which owns a runner or may place a run on one.
func poolOwner(ctx context.Context) string {
	if neverOperator(ctx) {
		return ""
	}
	return oidcHumanFromContext(ctx)
}

// placementDemand reads the barrier the run's policy floor and its request ask for. Its size starts
// at none: resolvePlacement sets the APPLIED size once runSize has bounded the ask, never the request's
// own. A class no executor and no runner of the deployment enforces at all is left to the confinement
// step, whose sentence is the long-standing one for it; only a class SOME target enforces is held
// against each candidate here.
func (s *Server) placementDemand(ctx context.Context, in placementInput) placementDemand {
	d := placementDemand{floor: in.floor, reqCC: in.reqCC, reqCCOK: in.reqCCOK, baseline: in.baseline}
	advertised := s.advertisedConfinement(ctx, *in.req)
	// The org default barrier is left out ("") on purpose: it is chosen only from the advertised set,
	// so it never changes whether the resulting class is enforced.
	if enforced, _, err := enforcedConfinement(in.floor, in.reqCC, advertised, "", in.baseline); err != nil ||
		(enforced != "" && !slices.Contains(advertised, enforced)) {
		d.reqCCOK = false
	}
	return d
}

// poolTargets is the resolved pool's candidate set with each candidate's own facts, before any demand
// is held against them. A self-hosted pool yields only the launching person's own claimed runners that
// are bound to this organisation (the claim path's check); a remote-provided pool yields its
// configured executors that this deployment runs. The pool-use policy already narrowed who may reach a
// remote pool. A pinned runner must be one of the candidates; one that is not is answered like an
// unknown id and a runner that is not the caller's is never named.
func (s *Server) poolTargets(ctx context.Context, req *createRunRequest, ps store.RunnerPoolStore, row store.RunnerPoolCatalogueRow) ([]poolTarget, *runRefusal) {
	if row.Pool.HostingType == types.RunnerPoolRemoteProvided {
		return executorTargets(row, s.configuredExecutors(), func(e string) (runner.TargetFacts, bool) { return s.executorFacts(ctx, e) })
	}
	return s.ownRunnerTargets(ctx, req, ps, row)
}

// executorTargets is a remote pool's configured executors, each with the facts read reports for it.
func executorTargets(row store.RunnerPoolCatalogueRow, configured []string, read func(executor string) (runner.TargetFacts, bool)) ([]poolTarget, *runRefusal) {
	var out []poolTarget
	for _, e := range runnerpool.SortedUnique(row.ExecutorIDs) {
		if !slices.Contains(configured, e) {
			continue
		}
		facts, known := read(e)
		out = append(out, poolTarget{id: e, executor: e, facts: facts, offline: !known})
	}
	if len(out) == 0 {
		return nil, poolRefusal(&runnerpool.Refusal{Reason: runnerpool.ReasonNoEligibleMember, Name: row.Pool.Name, Hosting: row.Pool.HostingType})
	}
	return out, nil
}

func (s *Server) ownRunnerTargets(ctx context.Context, req *createRunRequest, ps store.RunnerPoolStore, row store.RunnerPoolCatalogueRow) ([]poolTarget, *runRefusal) {
	none := func() *runRefusal {
		return poolRefusal(&runnerpool.Refusal{Reason: runnerpool.ReasonNoEligibleMember, Name: row.Pool.Name, Hosting: row.Pool.HostingType})
	}
	switch err := s.runnersEnabled(ctx); {
	case errors.Is(err, errRunnersDisabled):
		return nil, targetRefusal(placement.ReasonPlacementUnavailable, row.Pool.Name, row.Pool.HostingType)
	case err != nil:
		return nil, runError(503, reasonRunnersConfigUnavailable, "runner configuration is unavailable; retry later")
	}
	owner := poolOwner(ctx)
	rs, ok := s.cfg.Store.(store.RunnerStore)
	// The catalogue offers a person only their own Self-Hosted pools; this holds again here, so a pool
	// that is not the launcher's yields no candidate even if one reached this far.
	if owner == "" || !ok || row.Pool.Owner != owner {
		return nil, none()
	}
	members, err := ps.ListRunnerPoolMembers(ctx, row.Pool.ID)
	if err != nil {
		return nil, runServerError("list runner pool members", err)
	}
	runners, err := rs.ListRunnersByOwner(ctx, owner)
	if err != nil {
		return nil, runServerError("list runners", err)
	}
	orgHash := federation.OrgURLSHA256(s.cfg.RunnerOrgURL)
	runners = slices.DeleteFunc(runners, func(r types.Runner) bool { return r.OrgURLSHA256 != orgHash })
	ids := runnerpool.OwnRunnerCandidates(owner, members, runners)
	if refusal := pinRefusal(req.RunnerID, row, ids, runners); refusal != nil {
		return nil, refusal
	}
	if len(ids) == 0 {
		return nil, none()
	}
	held, err := s.runnerSlotHolders(ctx, ids)
	if err != nil {
		return nil, runServerError("count runs by runner", err)
	}
	out := make([]poolTarget, 0, len(ids))
	for _, id := range ids {
		id := id
		facts, known := s.runnerFacts(id)
		i := slices.IndexFunc(runners, func(r types.Runner) bool { return r.ID == id })
		full := i >= 0 && runners[i].MaxRuns > 0 && held[id] >= runners[i].MaxRuns
		out = append(out, poolTarget{id: id.String(), runnerID: &id, facts: facts, offline: !known || !s.runnerOnline(id), full: full})
	}
	slices.SortFunc(out, func(a, b poolTarget) int { return cmp.Compare(a.id, b.id) })
	return out, nil
}

// runnerSlotHolders is the number of sandbox-holding runs on each runner, read without a lock; nil from
// a store that cannot count, whose runners are then never read as full.
func (s *Server) runnerSlotHolders(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]int, error) {
	rs, ok := s.cfg.Store.(store.RunnerInventoryStore)
	if !ok {
		return nil, nil
	}
	return rs.RunnerSlotHolders(ctx, ids)
}

// pinRefusal refuses a runner_id that is not one of the caller's own candidates in this pool. The
// sentence names the runner only when it is the caller's own; anything else is the id as sent.
func pinRefusal(pin string, row store.RunnerPoolCatalogueRow, candidates []uuid.UUID, own []types.Runner) *runRefusal {
	if pin == "" {
		return nil
	}
	id, err := uuid.Parse(pin)
	if err == nil && slices.Contains(candidates, id) {
		return nil
	}
	named := pin
	if err == nil {
		if i := slices.IndexFunc(own, func(r types.Runner) bool { return r.ID == id }); i >= 0 {
			named = own[i].Name
		}
	}
	return poolRefusal(&runnerpool.Refusal{Reason: runnerpool.ReasonMemberMismatch, Name: row.Pool.Name, Runner: named})
}

// targetInspector is the optional per-target fact source of the wired runner.
func (s *Server) targetInspector() (runner.TargetInspector, bool) {
	ti, ok := s.cfg.Runner.(runner.TargetInspector)
	return ti, ok
}

// runnerFacts is a registered runner's advertised facts; unknown without an inspector.
func (s *Server) runnerFacts(id uuid.UUID) (runner.TargetFacts, bool) {
	if ti, ok := s.targetInspector(); ok {
		return ti.RunnerFacts(id.String())
	}
	return runner.TargetFacts{}, false
}

// executorFacts is one configured executor's classes. A lone driver that is not an orchestrator is its
// own and only executor, so its own capabilities are its facts.
func (s *Server) executorFacts(ctx context.Context, executor string) (runner.TargetFacts, bool) {
	if ti, ok := s.targetInspector(); ok {
		return ti.ExecutorFacts(ctx, executor)
	}
	if s.cfg.Runner == nil || s.cfg.Runner.Name() != executor {
		return runner.TargetFacts{}, false
	}
	caps, err := s.cfg.Runner.Capabilities(ctx)
	if err != nil {
		return runner.TargetFacts{}, false
	}
	_, keeps := s.cfg.Runner.(runner.SandboxEnder)
	return runner.TargetFacts{Classes: caps.ConfinementClasses, EphemeralDiskEnforcement: caps.EphemeralDiskEnforcement,
		KeepsEnded: keeps, UserDrives: caps.UserDrives}, true
}
