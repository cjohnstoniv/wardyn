// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Two follow-ups to the mount keep rules, against a real Postgres: a held fold is stale once the base
// chain it kept its mounts from changes, and an empty host path is a 400 on every write path and tier.
//
// Guarded by WARDYN_TEST_PG (throwawayPGPool): skipped cleanly when unset, must PASS when set.

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// F1: a security admin's held fold keeps the host paths the base chain resolved to at proposal. A super
// admin who re-points a base mount before the approval makes the fold stale: the approval refuses and
// nothing is written, so the kept set at apply is the kept set the reviewer saw.
func TestPolMount_F1_HeldFoldIsStaleAfterABaseSourceChange(t *testing.T) {
	e := newMountEnv(t, true)
	base := e.seedMountProfile("pm-f1-base", pinMounts())
	child := e.seedComposed("pm-f1-child", base, types.CeilingOverlay{})
	v, _ := e.view(e.alice, child.ID)
	c := v.Effective.Ceiling
	c.AllowedDomains = append(slices.Clone(c.AllowedDomains), "pypi.org")
	fold := e.pending(e.putProfile(e.alice, child.ID, map[string]any{"name": v.Name, "ceiling": c, "overlay": nil}))
	before := rowJSON(t, e.profile(child.ID))

	sv, _ := e.view(e.super, base.ID)
	ms := slices.Clone(sv.Ceiling.WorkspaceMounts)
	ms[0].Source = pinMountNew
	moved := e.pending(e.putProfile(e.super, base.ID, standaloneBody(sv, ms)))
	super2 := ssoSession(t, "sub-super-2", "super2@corp.example", oidc.RoleAdmin)
	if w := e.call(super2, http.MethodPost, approvePath(moved.ID), ""); w.Code != http.StatusOK {
		t.Fatalf("a second super admin's approval of the base change = %d %s, want 200", w.Code, w.Body)
	}
	if got := storedMount(t, e.profile(base.ID).Ceiling.WorkspaceMounts, pinTgtA); got.Source != pinMountNew {
		t.Fatalf("the base was not re-pointed: %+v", got)
	}

	w := e.call(e.bob, http.MethodPost, approvePath(fold.ID), "")
	if w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangeStale {
		t.Errorf("STALE FOLD: the approval after the base change = %d %s, want 409 %s", w.Code, w.Body, reasonGovernanceChangeStale)
	}
	if after := rowJSON(t, e.profile(child.ID)); after != before {
		t.Errorf("STALE FOLD: the refused approval changed the row:\n%s\n%s", before, after)
	}
}

// F2: a mount with an empty host path is the author's 400 "mount source is empty", for a security admin
// and a super admin alike, on a ceiling and on an overlay, and nothing is written.
func TestPolMount_F2_EmptySourceIs400AtEveryTier(t *testing.T) {
	e := newMountEnv(t, false)
	standalone := e.seedMountProfile("pm-f2", pinMounts())
	base := e.seedMountProfile("pm-f2-base", pinMounts())
	child := e.seedComposed("pm-f2-child", base, types.CeilingOverlay{})
	empty := []types.WorkspaceMount{{Source: "", Target: pinTgtA}}
	for _, tier := range []struct {
		name string
		c    *http.Cookie
	}{{"security admin", e.alice}, {"super admin", e.super}} {
		for _, path := range []struct {
			name string
			row  types.GovernanceProfile
			body func(governanceProfileView) map[string]any
		}{
			{"ceiling", standalone, func(v governanceProfileView) map[string]any { return standaloneBody(v, empty) }},
			{"overlay", child, func(v governanceProfileView) map[string]any { return overlayBody(v, &empty) }},
		} {
			before := rowJSON(t, e.profile(path.row.ID))
			v, _ := e.view(tier.c, path.row.ID)
			w := e.putProfile(tier.c, path.row.ID, path.body(v))
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "mount source is empty") {
				t.Errorf("%s, %s: an empty source = %d %s, want 400 mount source is empty", tier.name, path.name, w.Code, w.Body)
			}
			if after := rowJSON(t, e.profile(path.row.ID)); after != before {
				t.Errorf("%s, %s: the refused write changed the row", tier.name, path.name)
			}
		}
	}
}
