// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestValidateSiteSessions_Bounds(t *testing.T) {
	for _, ok := range []*types.SiteSessions{nil, {}, {UnconfirmedBudgetSec: ptr(0)}, {UnconfirmedBudgetSec: ptr(30)}, {UnconfirmedBudgetSec: ptr(120)}} {
		if err := validateSiteSessions(ok); err != nil {
			t.Errorf("validateSiteSessions(%+v) = %v, want nil", ok, err)
		}
	}
	for _, n := range []int{-1, 121} {
		err := validateSiteSessions(&types.SiteSessions{UnconfirmedBudgetSec: ptr(n)})
		if err == nil || !strings.Contains(err.Error(), "sessions.unconfirmed_budget_s") {
			t.Errorf("validateSiteSessions(%d) = %v, want an error naming sessions.unconfirmed_budget_s", n, err)
		}
	}
}

func TestSiteConfigPut_SessionsCarriedForwardAndCleared(t *testing.T) {
	stored := types.SiteConfig{Sessions: &types.SiteSessions{UnconfirmedBudgetSec: ptr(45)}}
	put := func(t *testing.T, body string) (*fakeSiteConfigStore, int, string) {
		t.Helper()
		fake := &fakeSiteConfigStore{cfg: stored}
		srv, _ := newSiteConfigHarness(t, fake)
		w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, body)
		return fake, w.Code, w.Body.String()
	}
	t.Run("a body without sessions keeps the stored budget", func(t *testing.T) {
		fake, code, body := put(t, `{}`)
		if code != http.StatusOK {
			t.Fatalf("PUT = %d; body=%s", code, body)
		}
		if got := fake.putSeen.Sessions; got == nil || got.UnconfirmedBudgetSec == nil || *got.UnconfirmedBudgetSec != 45 {
			t.Errorf("stored sessions = %+v, want 45 carried forward", got)
		}
	})
	for name, body := range map[string]string{"null": `{"sessions":null}`, "an empty block": `{"sessions":{}}`} {
		t.Run(name+" stores none", func(t *testing.T) {
			fake, code, resp := put(t, body)
			if code != http.StatusOK {
				t.Fatalf("PUT = %d; body=%s", code, resp)
			}
			if fake.putSeen.Sessions != nil {
				t.Errorf("stored sessions = %+v, want none", fake.putSeen.Sessions)
			}
		})
	}
	t.Run("0 survives as a stored 0", func(t *testing.T) {
		fake, code, body := put(t, `{"sessions":{"unconfirmed_budget_s":0}}`)
		if code != http.StatusOK {
			t.Fatalf("PUT = %d; body=%s", code, body)
		}
		if got := fake.putSeen.Sessions; got == nil || got.UnconfirmedBudgetSec == nil || *got.UnconfirmedBudgetSec != 0 {
			t.Errorf("stored sessions = %+v, want an explicit 0", got)
		}
	})
	t.Run("121 is refused", func(t *testing.T) {
		fake, code, body := put(t, `{"sessions":{"unconfirmed_budget_s":121}}`)
		if code != http.StatusBadRequest || !strings.Contains(body, reasonSiteConfigInvalid) {
			t.Fatalf("PUT = %d body=%s, want 400 with reason %s", code, body, reasonSiteConfigInvalid)
		}
		if fake.putSeen != nil {
			t.Error("a refused budget reached the store")
		}
	})
}

func TestSiteConfigWrite_DatumNamesTheSessionBudget(t *testing.T) {
	for name, tc := range map[string]struct {
		stored types.SiteConfig
		want   float64
	}{
		"no block":    {types.SiteConfig{}, 30},
		"a set block": {types.SiteConfig{Sessions: &types.SiteSessions{UnconfirmedBudgetSec: ptr(45)}}, 45},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeSiteConfigStore{cfg: tc.stored}
			srv, audit := newSiteConfigHarness(t, fake)
			w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, `{}`)
			if w.Code != http.StatusOK {
				t.Fatalf("PUT = %d; body=%s", w.Code, w.Body.String())
			}
			if got := siteConfigWriteDatum(t, audit)["session_unconfirmed_budget_s"]; got != tc.want {
				t.Errorf("session_unconfirmed_budget_s = %v, want %v", got, tc.want)
			}
		})
	}
}
