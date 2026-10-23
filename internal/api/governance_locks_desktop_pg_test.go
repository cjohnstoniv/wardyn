// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestDesktopGroupLock_ATeamBelowCannotSetOrClearIt (#2967): ua-a locks its Desktop group (all four
// directions). ua-a1's admin, a team below, cannot set a locked direction on ua-a1: blocking more is
// 409 unit_knob_locked naming ua-a, allowing more is the overlay's 400 (it can only block more). Nor
// can they clear the setting or the lock where ua-a holds them, nor the boundary's (403). Each refused
// write leaves the stored state as it was.
func TestDesktopGroupLock_ATeamBelowCannotSetOrClearIt(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	group := []string{"desktop.copy_out", "desktop.download", "desktop.paste_in", "desktop.upload"}
	pathA, pathA1 := "/api/v1/governance/units/"+e.a.ID.String(), "/api/v1/governance/units/"+e.a1.ID.String()
	aOverlay := map[string]any{"allowed_domains": []string{"pypi.org", "github.com", "npmjs.org"}, "desktop": map[string]string{"copy_out": "blocked", "upload": "blocked"}}
	if w := e.call(e.alice, http.MethodPut, pathA, e.restate(e.a, map[string]any{"overlay": aOverlay, "locked_knobs": group})); w.Code != http.StatusOK {
		t.Fatalf("locking ua-a's Desktop group = %d %s, want 200", w.Code, w.Body)
	}
	before := func(u types.GovernanceUnit) types.GovernanceUnit { return e.profile(u.ID) }
	a, a1 := before(e.a), before(e.a1)
	unchanged := func(step string) {
		t.Helper()
		if got := e.profile(e.a.ID); !slices.Equal(got.LockedKnobs, a.LockedKnobs) || !sameJSON(got.Overlay, a.Overlay) {
			t.Errorf("%s changed ua-a: locks %v overlay %+v", step, got.LockedKnobs, got.Overlay)
		}
		if got := e.profile(e.a1.ID); !sameJSON(got.Overlay, a1.Overlay) {
			t.Errorf("%s changed ua-a1's overlay to %+v", step, got.Overlay)
		}
	}

	a1Body := func(d map[string]string) string {
		return uaUnitBody("ua-a1", e.a.ID, map[string]any{"allowed_domains": []string{"pypi.org", "github.com"}, "desktop": d}, nil)
	}
	if w := e.call(e.adminA1, http.MethodPut, pathA1, a1Body(map[string]string{"paste_in": "blocked"})); w.Code != http.StatusConflict ||
		wireReason(t, w) != reasonUnitKnobLocked || !strings.Contains(w.Body.String(), "desktop.paste_in is locked by ua-a") {
		t.Errorf("ua-a1's admin blocking paste in on ua-a1 = %d %s, want 409 naming ua-a's lock", w.Code, w.Body)
	}
	unchanged("narrowing a locked direction below")
	if w := e.call(e.adminA1, http.MethodPut, pathA1, a1Body(map[string]string{"copy_out": "allowed"})); w.Code != http.StatusBadRequest ||
		wireReason(t, w) != reasonGovernanceOverlayInvalid {
		t.Errorf("ua-a1's admin allowing copy out on ua-a1 = %d %s, want 400 (an overlay only blocks more)", w.Code, w.Body)
	}
	unchanged("widening a locked direction below")

	for _, c := range []struct{ step, path, method, body string }{
		{"clearing ua-a's desktop", pathA, http.MethodPut, e.restate(e.a, map[string]any{"overlay": uaDomains("pypi.org", "github.com", "npmjs.org"), "locked_knobs": group})},
		{"clearing ua-a's group lock", pathA, http.MethodPut, e.restate(e.a, map[string]any{"locked_knobs": []string{}})},
		{"clearing the boundary's locks", "/api/v1/governance/boundary", http.MethodPut, `{"mode":"hard_limits","spec":{},"limits":{},"locked_knobs":[]}`},
	} {
		if w := e.call(e.adminA1, c.method, c.path, c.body); w.Code != http.StatusForbidden {
			t.Errorf("ua-a1's admin %s = %d %s, want 403", c.step, w.Code, w.Body)
		}
		unchanged(c.step)
	}
}
