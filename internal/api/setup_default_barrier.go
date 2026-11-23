// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// defaultBarrierUnsetCheck grades the default_barrier_unset row: with no organisation default barrier a
// launch that names none gets the strongest class offered (defaultBarrier), so offering CC2 or CC3 moves
// every such launch to it. Information, never a warning and never Blocking. ok is false when an
// organisation default is set or nothing stronger than CC1 is offered.
func defaultBarrierUnsetCheck(orgBarrier types.ConfinementClass, offered []types.ConfinementClass) (SetupCheck, bool) {
	best := bestClass(offered)
	if orgBarrier != "" || best.Rank() < types.CC2.Rank() {
		return SetupCheck{}, false
	}
	return SetupCheck{ID: "default_barrier_unset", Label: "Runs that name no barrier get the strongest class offered", Status: "info",
		Detail: fmt.Sprintf("The strongest class offered is %s, so a launch that names no barrier gets %s.", best, best),
		Fix:    "Set an organisation default barrier, so offering a stronger class does not move every unnamed run to it."}, true
}

// defaultBarrierUnsetRow reads what defaultBarrier reads: the organisation's default barrier
// (runner_pool_org_defaults) and the classes the executor and the claimed runners offer. A store that
// cannot be read leaves the row out; nothing is guessed.
func (s *Server) defaultBarrierUnsetRow(ctx context.Context, rnr SetupRunner) (SetupCheck, bool) {
	ps, ok := s.cfg.Store.(store.RunnerPoolStore)
	if !ok {
		return SetupCheck{}, false
	}
	org, err := ps.GetRunnerPoolOrgDefaults(ctx)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: setup status: read runner pool defaults", slog.Any("err", err))
		return SetupCheck{}, false
	}
	if org.Barrier != "" {
		return SetupCheck{}, false
	}
	var offered []types.ConfinementClass
	for _, c := range rnr.ConfinementClasses {
		offered = append(offered, types.ConfinementClass(c))
	}
	if rs, ok := s.cfg.Store.(store.RunnerStore); ok {
		runners, err := rs.ListRunners(ctx)
		if err != nil {
			slog.WarnContext(ctx, "wardynd: setup status: list runners", slog.Any("err", err))
			return SetupCheck{}, false
		}
		for _, r := range runners {
			if facts, ok := s.runnerFacts(r.ID); r.State == types.RunnerClaimed && ok {
				offered = append(offered, facts.Classes...)
			}
		}
	}
	return defaultBarrierUnsetCheck(org.Barrier, offered)
}
