// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Each desktop direction locks on its own (#2967, board M7-Knobs-B): desktop.paste_in, desktop.copy_out,
// desktop.upload and desktop.download. A lock fixes the direction at the locker's rule, its default
// when the locker leaves it unset, and a unit below that states another rule is refused naming the
// locker.

// desktopUnit is a unit composed on parent (nil: the deployment default) whose overlay states d.
func desktopUnit(name string, parent *uuid.UUID, d types.DesktopControls, locks ...string) types.GovernanceUnit {
	return types.GovernanceUnit{ID: uuid.New(), Name: name, ParentID: parent, Overlay: &types.CeilingOverlay{Desktop: &d}, LockedKnobs: locks}
}

// lockRefusal is checkUnitLocks' refusal message for p under all and b, "" when it applies.
func lockRefusal(t *testing.T, srv *Server, p types.GovernanceUnit, all []types.GovernanceUnit, b types.Boundary) string {
	t.Helper()
	err := srv.checkUnitLocks(p, nil, all, b, store.MaxUnitDepthCap)
	if err == nil {
		return ""
	}
	var we *unitWriteError
	if !errors.As(err, &we) || we.reason != reasonUnitKnobLocked {
		t.Fatalf("checkUnitLocks = %v, want a %s refusal", err, reasonUnitKnobLocked)
	}
	return we.Error()
}

func TestDesktopDirectionLocks_AreLockableKnobs(t *testing.T) {
	got, msg := normalizeLockedKnobs([]string{"desktop.copy_out", "desktop.paste_in", "desktop.upload", "desktop.download", "desktop"})
	if msg != "" || !slices.Equal(got, []string{"desktop", "desktop.copy_out", "desktop.download", "desktop.paste_in", "desktop.upload"}) {
		t.Errorf("normalizeLockedKnobs = %v %q, want the five desktop knobs", got, msg)
	}
	if _, msg := normalizeLockedKnobs([]string{"desktop.keystrokes"}); msg == "" {
		t.Error("desktop.keystrokes was accepted as a knob")
	}
}

// TestDesktopDirectionLock_BoundaryRefusesAnotherRuleBelow: the boundary in hard_limits locks one
// direction; a unit stating it looser is refused naming the boundary, the same rule or another
// direction is not, and a direction the boundary's desktop leaves unset is held at its default.
func TestDesktopDirectionLock_BoundaryRefusesAnotherRuleBelow(t *testing.T) {
	srv := New(Config{AdminToken: adminToken})
	b := types.Boundary{Mode: types.BoundaryHardLimits, Spec: types.BoundarySpec{Desktop: &types.DesktopControls{CopyOut: types.TransferAllowed}},
		LockedKnobs: []string{"desktop.download"}}
	for _, c := range []struct {
		name string
		d    types.DesktopControls
		want string
	}{
		{"allowing the locked direction", types.DesktopControls{Download: types.TransferAllowed}, "desktop.download is locked by the Wardyn boundary"},
		{"the locked direction at its default", types.DesktopControls{Download: types.TransferBlocked}, ""},
		{"another direction", types.DesktopControls{CopyOut: types.TransferBlocked, Upload: types.TransferBlocked}, ""},
	} {
		if got := lockRefusal(t, srv, desktopUnit("team", nil, c.d), nil, b); (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: refusal %q, want %q", c.name, got, c.want)
		}
	}
	b.Mode = types.BoundaryNoLimits
	if got := lockRefusal(t, srv, desktopUnit("team", nil, types.DesktopControls{Download: types.TransferAllowed}), nil, b); got != "" {
		t.Errorf("a no_limits boundary's lock refused %q", got)
	}
}

// TestDesktopDirectionLock_UnitRefusesAnotherRuleBelow: a unit's lock on a direction fixes it at that
// unit's effective rule for every unit below.
func TestDesktopDirectionLock_UnitRefusesAnotherRuleBelow(t *testing.T) {
	srv := New(Config{AdminToken: adminToken})
	eng := desktopUnit("Engineering", nil, types.DesktopControls{Upload: types.TransferBlocked}, "desktop.download", "desktop.upload")
	all := []types.GovernanceUnit{eng}
	for _, c := range []struct {
		name string
		d    types.DesktopControls
		want string
	}{
		{"allowing an upload Engineering blocks", types.DesktopControls{Upload: types.TransferAllowed}, "desktop.upload is locked by Engineering"},
		{"allowing a download Engineering leaves at its default", types.DesktopControls{Download: types.TransferAllowed}, "desktop.download is locked by Engineering"},
		{"blocking a download Engineering leaves at its default", types.DesktopControls{Download: types.TransferBlocked}, ""},
		{"a direction Engineering does not lock", types.DesktopControls{PasteIn: types.TransferBlocked}, ""},
	} {
		if got := lockRefusal(t, srv, desktopUnit("Platform team", &eng.ID, c.d), all, types.Boundary{}); (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: refusal %q, want %q", c.name, got, c.want)
		}
	}
}

// TestDesktopDirectionLock_BoundaryLockListsUnitsWithAnotherRule: a boundary write newly locking a
// direction lists the units stating it otherwise.
func TestDesktopDirectionLock_BoundaryLockListsUnitsWithAnotherRule(t *testing.T) {
	units := []types.GovernanceUnit{
		desktopUnit("Mobile team", nil, types.DesktopControls{CopyOut: types.TransferAllowed}),
		desktopUnit("Research", nil, types.DesktopControls{CopyOut: types.TransferBlocked}),
	}
	after := types.Boundary{Mode: types.BoundaryHardLimits, Spec: types.BoundarySpec{Desktop: &types.DesktopControls{}},
		LockedKnobs: []string{"desktop.copy_out"}}
	if got := boundaryLockConflicts(units, types.Boundary{}, after); !slices.Equal(got, []string{"desktop.copy_out: Mobile team"}) {
		t.Errorf("conflicts = %v, want only Mobile team's copy out", got)
	}
}
