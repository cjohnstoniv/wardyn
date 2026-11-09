// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/lifecycle"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// touchDebounce bounds how often the decision ingest refreshes a run's
// updated_at, turning a chatty agent's burst into one UPDATE per window. The
// reaper adds the same constant as threshold slack (lifecycle.TouchDebounce),
// so the debounce can never make an active run look idle.
const touchDebounce = lifecycle.TouchDebounce

// decisionIsActivity reports whether a decision is the run doing something: egress.CountsAsActivity, the one
// predicate every idle clock shares (an allowed request, not a refusal, a hold, a token count or the toolgate's
// approval poll), minus one case. A macOS guest calls Apple's boot hosts in the background; a run that allows
// all egress, or whose allow covers one, gets those calls allowed, so on a macOS VM run they never count,
// allowed or refused. A runner's proxy applies the same list (proxy.Config.ActivityIgnoreHosts). On a VM run the
// proxy also marks every request that arrives on the vmnet relay lane (egress.Request.Relayed), and those never count.
func decisionIsActivity(kind types.MachineKind, dl egress.DecisionLog) bool {
	return egress.CountsAsActivityFor(dl, activityIgnoreHosts(kind))
}

// shouldTouch reports whether dl, a decision on a run of this kind, restarts the idle clock and runID's last
// touch is older than touchDebounce, recording now when it is. The map is pruned wholesale past a bound instead of
// per-run bookkeeping — worst case is one extra UPDATE per live run after a
// prune, which the debounce exists to make harmless.
// ponytail: in-process only; per-replica debounce is fine because the singleton
// control plane is a documented constraint (docs/OPERATIONS.md).
//
// The decision is judged by decisionIsActivity, the predicate the pause clock and the runner-side Activity
// flag use too: a refused request, a re-auth hold timing out and the toolgate's poll of an approval it is
// blocked on are not the agent doing something, so none restarts the idle clock.
func (s *Server) shouldTouch(runID uuid.UUID, kind types.MachineKind, dl egress.DecisionLog) bool {
	if !decisionIsActivity(kind, dl) {
		return false
	}
	return s.touchDue(runID)
}

// touchDue is the debounce alone, for a touch that is not an egress decision (a person's UI relay).
func (s *Server) touchDue(runID uuid.UUID) bool {
	now := time.Now()
	s.lastTouchMu.Lock()
	defer s.lastTouchMu.Unlock()
	if last, ok := s.lastTouch[runID]; ok && now.Sub(last) < touchDebounce {
		return false
	}
	if s.lastTouch == nil {
		s.lastTouch = make(map[uuid.UUID]time.Time)
	} else if len(s.lastTouch) > 4096 {
		clear(s.lastTouch)
	}
	s.lastTouch[runID] = now
	return true
}
