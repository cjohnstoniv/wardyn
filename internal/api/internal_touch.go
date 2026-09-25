// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/lifecycle"
)

// touchDebounce bounds how often the decision ingest refreshes a run's
// updated_at, turning a chatty agent's burst into one UPDATE per window. The
// reaper adds the same constant as threshold slack (lifecycle.TouchDebounce),
// so the debounce can never make an active run look idle.
const touchDebounce = lifecycle.TouchDebounce

// shouldTouch reports whether runID's last touch is older than touchDebounce,
// recording now when it is. The map is pruned wholesale past a bound instead of
// per-run bookkeeping — worst case is one extra UPDATE per live run after a
// prune, which the debounce exists to make harmless.
// ponytail: in-process only; per-replica debounce is fine because the singleton
// control plane is a documented constraint (docs/OPERATIONS.md).
//
// ruleSource excludes the ONE decision that is not real agent activity:
// credential:reauth-timeout is the proxy's own signal that a re-auth hold's
// WAIT ran out with nobody there (ruleSourceCredentialReauthTimeout, mirrored
// from internal/egress/proxy). A timeout reports that nobody answered, not
// that the agent did something, so it should not restart the idle clock.
func (s *Server) shouldTouch(runID uuid.UUID, ruleSource string) bool {
	if ruleSource == ruleSourceCredentialReauthTimeout {
		return false
	}
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
