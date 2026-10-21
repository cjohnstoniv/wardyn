// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A follow-up to the mount keep rules, against a real Postgres: a held fold is stale once the base chain
// it kept its mounts from changes.
//
// Guarded by WARDYN_TEST_PG (throwawayPGPool): skipped cleanly when unset, must PASS when set.

import (
	"net/http"
	"slices"
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
