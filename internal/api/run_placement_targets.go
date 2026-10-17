// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
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
	// full: a runner with no room for the run now (its Runs at once, or a total), as last read without
	// a lock. The insert, the promotion and the start count again under the runner's row lock.
	full bool
	// limits is the runner's declared limit set, nil for none (set by placementDemand.withFit).
	limits *types.RunnerLimits
	// trust is a registered runner's stored trust class, which its credential plan is judged at.
	trust types.RunnerTrustClass
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
	raw, err := prefs.GetPrincipalPref(ctx, owner, types.RunnerPoolDefaultPrefKey)
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
func (s *Server) placementDemand(ctx context.Context, in placementInput) (placementDemand, *runRefusal) {
	fits, err := s.runnerFits(ctx)
	if err != nil {
		return placementDemand{}, runServerError("read runner limits", err)
	}
	d := placementDemand{floor: in.floor, reqCC: in.reqCC, reqCCOK: in.reqCCOK, baseline: in.baseline, fits: fits}
	advertised := s.advertisedConfinement(ctx, *in.req)
	// The org default barrier is left out ("") on purpose: it is chosen only from the advertised set,
	// so it never changes whether the resulting class is enforced.
	if enforced, _, err := enforcedConfinement(in.floor, in.reqCC, advertised, "", in.baseline); err != nil ||
		(enforced != "" && !slices.Contains(advertised, enforced)) {
		d.reqCCOK = false
	}
	return d, nil
}

// poolTargets is the resolved pool's candidate set of both kinds, each candidate with its own facts,
// before any demand is held against them. An organization pool yields its configured remote-hosted
// runners and its claimed organization runners, whom its use policy already admitted the person to (the
// catalogue the pool was resolved from is the caller's permitted one). A user pool yields the launching person's
// own claimed runners that are bound to this organisation (the claim path's check), and its configured
// remote-hosted runners whose people policy admits the person now (admittedRemoteMembers). When none of
// the person's own runners can be offered, the pool's remote-hosted ones still are, unless the request
// asks for the person's own runner (a placement of local or a pin), whose refusal is then the answer. A
// pinned runner must be one of the candidates; one that is not is answered like an unknown id and a
// runner that is not the caller's is never named.
func (s *Server) poolTargets(ctx context.Context, req *createRunRequest, ps store.RunnerPoolStore, row store.RunnerPoolCatalogueRow) ([]poolTarget, *runRefusal) {
	read := func(e string) (runner.TargetFacts, bool) { return s.executorFacts(ctx, e) }
	if row.Pool.Scope == types.RunnerPoolOrganization {
		remote, refusal := executorTargets(row, s.configuredExecutors(), read)
		org, orgRefusal := s.orgRunnerTargets(ctx, ps, row)
		if orgRefusal != nil {
			return nil, orgRefusal
		}
		if len(org) == 0 {
			return remote, refusal
		}
		return append(org, remote...), nil
	}
	admitted, refusal := s.admittedRemoteMembers(ctx, row)
	if refusal != nil {
		return nil, refusal
	}
	remote, _ := executorTargets(admitted, s.configuredExecutors(), read)
	own, refusal := s.ownRunnerTargets(ctx, req, ps, row)
	if refusal != nil && (len(remote) == 0 || req.Placement == placement.Local || refusal.status >= http.StatusInternalServerError) {
		return nil, refusal
	}
	return append(own, remote...), nil
}

// admittedRemoteMembers is a user pool's row narrowed to the remote-hosted members whose people policy
// admits the launching person. The policy was checked when the person added the runner and is checked
// again here at every launch, so a narrowed policy drops the runner from their candidates while the
// membership row stays. A store that cannot read remote runners admits none.
func (s *Server) admittedRemoteMembers(ctx context.Context, row store.RunnerPoolCatalogueRow) (store.RunnerPoolCatalogueRow, *runRefusal) {
	out := row
	out.ExecutorIDs = nil
	rs, ok := s.cfg.Store.(store.RemoteRunnerStore)
	if len(row.ExecutorIDs) == 0 || !ok {
		return out, nil
	}
	subj, err := s.callerSubjects(ctx)
	if err != nil {
		return out, ceilingRunRefusal(err)
	}
	for _, id := range row.ExecutorIDs {
		rr, err := rs.GetRemoteRunner(ctx, id)
		switch {
		case errors.Is(err, store.ErrNotFound):
			continue
		case err != nil:
			return out, runServerError("read remote runner", err)
		}
		if subj.admitsPeople(rr.Runner) {
			out.ExecutorIDs = append(out.ExecutorIDs, id)
		}
	}
	return out, nil
}

// placement is the placement a candidate's kind runs at: local on one of the person's own runners,
// remote on a remote-hosted runner.
func (t poolTarget) placement() types.Placement {
	if t.runnerID != nil {
		return types.PlacementLocal
	}
	return types.PlacementRemote
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
		return nil, poolRefusal(&runnerpool.Refusal{Reason: runnerpool.ReasonNoEligibleMember, Name: row.Pool.Name, Scope: row.Pool.Scope})
	}
	return out, nil
}

func (s *Server) ownRunnerTargets(ctx context.Context, req *createRunRequest, ps store.RunnerPoolStore, row store.RunnerPoolCatalogueRow) ([]poolTarget, *runRefusal) {
	none := func() *runRefusal {
		return poolRefusal(&runnerpool.Refusal{Reason: runnerpool.ReasonNoEligibleMember, Name: row.Pool.Name, Scope: row.Pool.Scope})
	}
	switch err := s.runnersEnabled(ctx); {
	case errors.Is(err, errRunnersDisabled):
		return nil, targetRefusal(placement.ReasonPlacementUnavailable, row.Pool.Name, row.Pool.Scope)
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
	return s.registeredRunnerTargets(ctx, ids, runners)
}

// orgRunnerTargets is an organization pool's claimed organization runners bound to this organisation:
// none while runners are switched off or the store keeps none, so the pool's remote-hosted runners are
// still offered. A member that is no longer a runner is skipped.
func (s *Server) orgRunnerTargets(ctx context.Context, ps store.RunnerPoolStore, row store.RunnerPoolCatalogueRow) ([]poolTarget, *runRefusal) {
	rs, ok := s.cfg.Store.(store.RunnerStore)
	if !ok || s.runnersEnabled(ctx) != nil {
		return nil, nil
	}
	members, err := ps.ListRunnerPoolMembers(ctx, row.Pool.ID)
	if err != nil {
		return nil, runServerError("list runner pool members", err)
	}
	orgHash := federation.OrgURLSHA256(s.cfg.RunnerOrgURL)
	var runners []types.Runner
	for _, m := range members {
		if m.RunnerID == nil {
			continue
		}
		rn, err := rs.GetRunner(ctx, *m.RunnerID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			continue
		case err != nil:
			return nil, runServerError("read runner", err)
		}
		if rn.OrgURLSHA256 == orgHash {
			runners = append(runners, rn)
		}
	}
	ids := runnerpool.OrgRunnerCandidates(members, runners)
	if len(ids) == 0 {
		return nil, nil
	}
	return s.registeredRunnerTargets(ctx, ids, runners)
}

// registeredRunnerTargets is each runner of ids, among runners, with its facts, room and trust class.
func (s *Server) registeredRunnerTargets(ctx context.Context, ids []uuid.UUID, runners []types.Runner) ([]poolTarget, *runRefusal) {
	held, err := s.runnerSlotHolders(ctx, ids)
	if err != nil {
		return nil, runServerError("count runs by runner", err)
	}
	// A promoted run's start finds the run itself holding the place its promotion took on its runner:
	// that place is this run's, so the runner is not full for it.
	if bound := promotedRunner(ctx); bound != nil && held[*bound] > 0 {
		held[*bound]--
	}
	out := make([]poolTarget, 0, len(ids))
	for _, id := range ids {
		id := id
		facts, known := s.runnerFacts(id)
		i := slices.IndexFunc(runners, func(r types.Runner) bool { return r.ID == id })
		if i < 0 {
			continue
		}
		full := runners[i].MaxRuns > 0 && held[id] >= runners[i].MaxRuns
		out = append(out, poolTarget{id: id.String(), runnerID: &id, facts: facts, offline: !known || !s.runnerOnline(id), full: full, trust: runners[i].TrustClass})
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
// own and only executor, so its own capabilities are its facts, and a failed read is its ProbeErr.
func (s *Server) executorFacts(ctx context.Context, executor string) (runner.TargetFacts, bool) {
	if ti, ok := s.targetInspector(); ok {
		return ti.ExecutorFacts(ctx, executor)
	}
	if s.cfg.Runner == nil || s.cfg.Runner.Name() != executor {
		return runner.TargetFacts{}, false
	}
	caps, err := s.cfg.Runner.Capabilities(ctx)
	if err != nil {
		return runner.TargetFacts{ProbeErr: err}, false
	}
	_, keeps := s.cfg.Runner.(runner.SandboxEnder)
	return runner.TargetFacts{Classes: caps.ConfinementClasses, EphemeralDiskEnforcement: caps.EphemeralDiskEnforcement,
		KeepsEnded: keeps, UserDrives: caps.UserDrives, Records: caps.SessionRecording}, true
}
