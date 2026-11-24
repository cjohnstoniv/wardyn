// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// validateSiteSessions refuses a session budget outside 0 to types.SessionBudgetMaxSec seconds.
func validateSiteSessions(ss *types.SiteSessions) error {
	if ss == nil || ss.UnconfirmedBudgetSec == nil {
		return nil
	}
	if n := *ss.UnconfirmedBudgetSec; n < 0 || n > types.SessionBudgetMaxSec {
		return fmt.Errorf("sessions.unconfirmed_budget_s: %d is outside 0 to %d", n, types.SessionBudgetMaxSec)
	}
	return nil
}

// carryForwardSiteSessions keeps the stored sessions block when the body does not name it, on the provider
// blocks' MDM re-apply terms, and stores a block that sets nothing as none, so a GET stays byte-identical to an
// install that never set one.
func carryForwardSiteSessions(cfg *types.SiteConfig, existing types.SiteConfig, present map[string]bool) {
	if !present["sessions"] {
		cfg.Sessions = existing.Sessions
	} else if cfg.Sessions != nil && cfg.Sessions.UnconfirmedBudgetSec == nil {
		cfg.Sessions = nil
	}
}
