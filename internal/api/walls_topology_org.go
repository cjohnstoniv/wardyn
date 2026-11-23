// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The security projection's extras on the Walls topology: the boundary's mode and the restrictions by name, the
// organisation's runners as GET /api/v1/runners serves them, an aggregate of the people's devices, the
// organisation pools, and the ways the organisation's traffic leaves it (gates). Unit admins get none of them.

// The bounds of the runner read (EST).
const (
	wallsTopoRunnerPage  = 500
	wallsTopoRunnerPages = 10   // active runner pages read before the list is partial
	wallsTopoMaxRunners  = 2000 // claimed runners kept, by id
)

// wallsTopoOrgExtras sets the security keys on body and returns each claimed runner's owner kind, for the run
// items' owner_kind. partial is set when the runner list was cut. ok is false when it wrote an error: a runner
// read that fails fails the topology whole, as it fails GET /runners.
func (s *Server) wallsTopoOrgExtras(w http.ResponseWriter, r *http.Request, body *WallsTopology,
	runs []types.AgentRun) (ownerKind map[uuid.UUID]types.RunnerOwnerKind, partial, ok bool) {
	ctx := r.Context()
	if b, err := s.governanceBoundary(ctx); err != nil {
		slog.WarnContext(ctx, "wardyn: the Walls view could not read the boundary; it is left out", slog.Any("err", err))
	} else {
		body.Boundary = &WallsBoundary{Mode: b.Mode}
	}
	body.Restrictions = s.wallsTopoRestrictions(ctx)
	org, devices, ownerKind, partial, ok := s.wallsTopoRunners(w, r)
	if !ok {
		return nil, false, false
	}
	body.Runners, body.Devices = org, devices
	body.Pools = s.wallsTopoPools(ctx, org)
	body.Gates = s.wallsTopoGates(ctx, runs)
	return ownerKind, partial, true
}

// wallsTopoRestrictions is every restriction as its id and name, never its spec; nil, logged, when the store
// has none or the read fails.
func (s *Server) wallsTopoRestrictions(ctx context.Context) []WallsRestrictionRef {
	rs, ok := s.restrictionStore()
	if !ok {
		return nil
	}
	list, err := rs.ListRestrictions(ctx)
	if err != nil {
		slog.WarnContext(ctx, "wardyn: the Walls view could not list restrictions; they are left out", slog.Any("err", err))
		return nil
	}
	out := make([]WallsRestrictionRef, len(list))
	for i, rr := range list {
		out[i] = WallsRestrictionRef{ID: rr.ID, Name: rr.Name}
	}
	return out
}

// wallsTopoRunners reads every claimed runner of the active list once and builds their views with the same call,
// the same admin=true and the same caller as GET /api/v1/runners, so each organisation runner's item is that
// route's bytes for it. org is the organisation's runners by id; devices folds the people's. ownerKind covers
// every claimed runner. A store without the runner inventory sends neither.
func (s *Server) wallsTopoRunners(w http.ResponseWriter, r *http.Request) (org []types.RunnerView, devices *WallsDevices,
	ownerKind map[uuid.UUID]types.RunnerOwnerKind, partial, ok bool) {
	ownerKind = map[uuid.UUID]types.RunnerOwnerKind{}
	rs, has := s.cfg.Store.(store.RunnerInventoryStore)
	if !has {
		return nil, nil, ownerKind, false, true
	}
	claimed, partial, err := wallsTopoClaimedRunners(r.Context(), rs, s.cfg.Now().UTC())
	if err != nil {
		writeServerError(w, r, "list runners", err)
		return nil, nil, nil, false, false
	}
	views, ok := s.runnerViews(w, r, rs, claimed, true)
	if !ok {
		return nil, nil, nil, false, false
	}
	org = []types.RunnerView{}
	devices = &WallsDevices{}
	for _, v := range views {
		ownerKind[v.ID] = v.OwnerKind
		switch v.OwnerKind {
		case types.RunnerOwnerOrg:
			org = append(org, v)
		case types.RunnerOwnerPerson:
			devices.Count++
			if v.Online {
				devices.Online++
			}
			devices.Runs += v.Usage.InUse.Runs
		}
	}
	return org, devices, ownerKind, partial, true
}

// wallsTopoClaimedRunners pages the active runners (unclaimed and claimed) and keeps the claimed ones, each once,
// sorted by id. Past wallsTopoMaxRunners, or when the page bound stops the read first, partial is set and the
// first ones by id are kept.
func wallsTopoClaimedRunners(ctx context.Context, rs store.RunnerInventoryStore, now time.Time) ([]types.Runner, bool, error) {
	seen := map[uuid.UUID]bool{}
	var claimed []types.Runner
	partial := true
	page := store.Page{Limit: wallsTopoRunnerPage}
	for range wallsTopoRunnerPages {
		rows, err := rs.ListRunnersPage(ctx, types.RunnerFilterActive, now, page)
		if err != nil {
			return nil, false, err
		}
		for _, row := range rows {
			if !seen[row.ID] && row.State == types.RunnerClaimed {
				seen[row.ID] = true
				claimed = append(claimed, row)
			}
		}
		if len(rows) < page.Limit {
			partial = false
			break
		}
		page.Offset += page.Limit
	}
	slices.SortFunc(claimed, func(a, b types.Runner) int { return strings.Compare(a.ID.String(), b.ID.String()) })
	if len(claimed) > wallsTopoMaxRunners {
		claimed, partial = claimed[:wallsTopoMaxRunners], true
	}
	return claimed, partial, nil
}

// wallsTopoPools is the organisation pools with a member among org or any remote-hosted executor, each with its
// organisation runners that are in org, by id. A store without pools sends none; a failed read sends none,
// logged. site_id is left unset until the organisation's sites are served.
func (s *Server) wallsTopoPools(ctx context.Context, org []types.RunnerView) []WallsPool {
	ps, ok := s.cfg.Store.(store.RunnerPoolStore)
	if !ok {
		return nil
	}
	rows, err := ps.ListRunnerPoolCatalogue(ctx, "", false)
	if err != nil {
		slog.WarnContext(ctx, "wardyn: the Walls view could not list runner pools; they are left out", slog.Any("err", err))
		return nil
	}
	inOrg := make(map[string]uuid.UUID, len(org))
	for _, v := range org {
		inOrg[v.ID.String()] = v.ID
	}
	out := []WallsPool{}
	for _, row := range rows {
		if row.Pool.Scope != types.RunnerPoolOrganization {
			continue
		}
		ids := []uuid.UUID{}
		for _, id := range row.OrgRunnerIDs {
			if u, ok := inOrg[id]; ok {
				ids = append(ids, u)
			}
		}
		if len(ids) == 0 && len(row.ExecutorIDs) == 0 {
			continue
		}
		slices.SortFunc(ids, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
		out = append(out, WallsPool{ID: row.Pool.ID, Name: row.Pool.Name, RunnerIDs: ids})
	}
	return out
}

// wallsTopoGates is gates[] for the snapshot's runs: the shared gate rule (wallsGates) over the upstream start
// rows of one probe per runner (wallsRunnerStarts), so a run that started before the window keeps its gate.
// Addresses go to a super admin only.
func (s *Server) wallsTopoGates(ctx context.Context, runs []types.AgentRun) []WallsGate {
	runnerOf := make(map[uuid.UUID]*uuid.UUID, len(runs))
	var hosted []types.AgentRun
	for _, run := range runs {
		runnerOf[run.ID] = run.RunnerID
		if run.RunnerID != nil {
			hosted = append(hosted, run)
		}
	}
	starts := s.wallsRunnerStarts(ctx, hosted)
	gates, _ := s.wallsGates(ctx, starts, func(id uuid.UUID) *uuid.UUID { return runnerOf[id] }, s.isOperator(ctx))
	return gates
}
