// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// backgroundWatchLimitRefusal is the control over watching a background macOS run's desktop, asked by the
// door that opens a watch and again at its re-check: nil when watching is allowed, else the refusal. Two
// controls answer, and a profile's deny_interactive and deny_ui_apps (uiSessionUnitDenies) are asked beside
// it, not here. The site switch comes first (macos_vm.allow_background_watch, absent reads allowed): off
// answers 403 reasonRunBackgroundWatchSiteOff. Then the run's profile as it stands now (runUnit), so a limit
// set later reaches a run already going: deny_background_desktop_watch answers as governance_unit on
// runs.desktop_watch. An unreadable site config or profile refuses too; the door stays shut.
//
// It judges neither the run's shape (background, macOS, view only) nor the caller, and exempts no role, a
// super admin included: those belong to the door.
func (s *Server) backgroundWatchLimitRefusal(ctx context.Context, run types.AgentRun) *runRefusal {
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return runServerError("read site config", err)
	}
	if !sc.MacOSVM.BackgroundWatchAllowed() {
		return runError(http.StatusForbidden, reasonRunBackgroundWatchSiteOff,
			"Watching background desktops is turned off for this install. An admin can turn it on.")
	}
	p, err := s.runUnit(ctx, run)
	if err != nil {
		if u, ok := isOverlayUnsatisfiable(err); ok {
			return runDenied(authz.Deny(authz.ReasonGovernanceOverlayUnsatisfiable, "runs.desktop_watch", u.Error()).OnRun(run.ID))
		}
		return runServerError("read the run's governance profile", err)
	}
	if p == nil || !p.Limits.DenyBackgroundDesktopWatch {
		return nil
	}
	return runDenied(authz.Deny(authz.ReasonGovernanceUnit, "runs.desktop_watch",
		"Your organisation's governance profile doesn't allow watching a background run's desktop.").
		OnRun(run.ID).WithPolicy(s.unitRefWithSiteHelp(ctx, p)).OnKnob(s.runLockOf(ctx, p, "deny_background_desktop_watch")))
}
