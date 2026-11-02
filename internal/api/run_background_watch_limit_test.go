// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// bgWatchLimitUnreadable serves the run's profile from attrStore, except that its chain cannot be read.
type bgWatchLimitUnreadable struct{ *attrStore }

func (bgWatchLimitUnreadable) GetGovernanceUnitChain(context.Context, uuid.UUID) ([]types.GovernanceUnit, error) {
	return nil, errors.New("conn closed by peer")
}

// TestBgWatchLimitRefusal: watching a background desktop is allowed unless the site switch is off or the run's
// profile sets deny_background_desktop_watch; an unreadable site config or profile refuses; a limit set after the
// run began reaches it, because the profile is read as it stands now.
func TestBgWatchLimitRefusal(t *testing.T) {
	off, on := false, true
	ask := func(t *testing.T, site types.SiteConfig, limits *types.GovernanceLimits, unreadable bool) *runRefusal {
		t.Helper()
		srv, st := runLimitsFixture(t, &capStore{})
		st.siteConfig = site
		run := types.AgentRun{ID: uuid.New()}
		if limits != nil {
			p := limitsProfile("quiet", *limits)
			run.UnitID = &p.ID
			attr := &attrStore{govEscapeStore: st, profiles: []types.GovernanceUnit{*p}}
			srv.cfg.Store = attr
			if unreadable {
				srv.cfg.Store = bgWatchLimitUnreadable{attr}
			}
		}
		return srv.backgroundWatchLimitRefusal(context.Background(), run)
	}
	siteOff := types.SiteConfig{MacOSVM: &types.SiteMacOSVM{AllowBackgroundWatch: &off}}

	t.Run("a run with no profile and a default site is allowed", func(t *testing.T) {
		if got := ask(t, types.SiteConfig{}, nil, false); got != nil {
			t.Errorf("refusal = %+v, want allowed", got)
		}
	})
	t.Run("an explicit true and a block without the field are allowed", func(t *testing.T) {
		for _, site := range []types.SiteConfig{
			{MacOSVM: &types.SiteMacOSVM{AllowBackgroundWatch: &on}},
			{MacOSVM: &types.SiteMacOSVM{MaxBaseAgeDays: 30}},
		} {
			if got := ask(t, site, &types.GovernanceLimits{}, false); got != nil {
				t.Errorf("site %+v: refusal = %+v, want allowed", site.MacOSVM, got)
			}
		}
	})
	t.Run("the site switch off refuses with its own reason", func(t *testing.T) {
		got := ask(t, siteOff, nil, false)
		if got == nil || got.status != http.StatusForbidden || got.body.Reason != reasonRunBackgroundWatchSiteOff {
			t.Fatalf("refusal = %+v, want 403 %s", got, reasonRunBackgroundWatchSiteOff)
		}
	})
	t.Run("the profile limit refuses as governance_unit on runs.desktop_watch", func(t *testing.T) {
		got := ask(t, types.SiteConfig{}, &types.GovernanceLimits{DenyBackgroundDesktopWatch: true}, false)
		if got == nil || got.decision == nil || got.decision.Reason != authz.ReasonGovernanceUnit || got.decision.Target != "runs.desktop_watch" {
			t.Fatalf("refusal = %+v, want a governance_unit decision on runs.desktop_watch", got)
		}
		if got.decision.Policy == nil {
			t.Error("the refusal names no policy")
		}
	})
	t.Run("a profile that sets other limits only is allowed", func(t *testing.T) {
		if got := ask(t, types.SiteConfig{}, &types.GovernanceLimits{DenyUIApps: true}, false); got != nil {
			t.Errorf("refusal = %+v, want allowed: deny_ui_apps has its own arm", got)
		}
	})
	t.Run("both off answers the site switch", func(t *testing.T) {
		got := ask(t, siteOff, &types.GovernanceLimits{DenyBackgroundDesktopWatch: true}, false)
		if got == nil || got.body.Reason != reasonRunBackgroundWatchSiteOff {
			t.Fatalf("refusal = %+v, want the site switch's reason", got)
		}
	})
	t.Run("an unreadable profile refuses", func(t *testing.T) {
		got := ask(t, types.SiteConfig{}, &types.GovernanceLimits{}, true)
		if got == nil || got.err == nil {
			t.Fatalf("refusal = %+v, want a read failure that refuses", got)
		}
	})
	t.Run("an unreadable site config refuses", func(t *testing.T) {
		srv, st := runLimitsFixture(t, &capStore{})
		st.failSiteConfigReadFrom = "backgroundWatchLimitRefusal"
		got := srv.backgroundWatchLimitRefusal(context.Background(), types.AgentRun{ID: uuid.New()})
		if got == nil || got.err == nil {
			t.Fatalf("refusal = %+v, want a read failure that refuses", got)
		}
	})
}
