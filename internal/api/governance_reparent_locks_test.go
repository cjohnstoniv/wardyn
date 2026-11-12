// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestGovernanceReparentOutOfALockIsHeld: moving a profile out from under a lock its old parents hold
// is a lock removed from above, so it is held for a second human even when its walls do not widen. A
// move that keeps every lock it was under, or only adds one, still applies.
func TestGovernanceReparentOutOfALockIsHeld(t *testing.T) {
	var (
		lockedA   = uuid.MustParse("dddddddd-0000-0000-0000-0000000000a1")
		lockedB   = uuid.MustParse("dddddddd-0000-0000-0000-0000000000a2")
		otherLock = uuid.MustParse("dddddddd-0000-0000-0000-0000000000a3")
		open      = uuid.MustParse("dddddddd-0000-0000-0000-0000000000a4")
		mid       = uuid.MustParse("dddddddd-0000-0000-0000-0000000000a5")
		team      = uuid.MustParse("dddddddd-0000-0000-0000-0000000000a6")
	)
	domains := []string{"github.com", "pypi.org"}
	standalone := func(id uuid.UUID, name string, locks ...string) types.GovernanceUnit {
		u := govCovStandalone(id, name, domains...)
		u.LockedKnobs = locks
		return u
	}
	rows := func(parent uuid.UUID) []types.GovernanceUnit {
		row := govCovComposedOn(team, "team", parent, domains...)
		return []types.GovernanceUnit{
			standalone(lockedA, "locker-a", "allowed_domains"),
			standalone(lockedB, "locker-b", "allowed_domains"),
			standalone(otherLock, "locker-other", "min_confinement_class"),
			standalone(open, "open"),
			govCovComposedOn(mid, "mid", lockedA, domains...),
			row,
		}
	}
	for _, c := range []struct {
		name     string
		from, to uuid.UUID
		want     bool
	}{
		{"out from under a lock to an unlocked profile is held", lockedA, open, false},
		{"out from under a lock to a profile locking another knob is held", lockedA, otherLock, false},
		{"out from under an inherited lock to an unlocked profile is held", mid, open, false},
		{"between two profiles locking the same knob applies", lockedA, lockedB, true},
		{"from an unlocked profile under a lock applies", open, lockedA, true},
		{"up to the profile that holds the lock applies", mid, lockedA, true},
		{"between two unlocked profiles applies", open, open, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := govCovServer(t, &govCovStore{})
			all := rows(c.from)
			p, res := govCovBuilt(t, s, all, team,
				`{"name":"team","parent_id":"`+c.to.String()+`","overlay":{"allowed_domains":["github.com","pypi.org"]}}`)
			if got := s.unitWriteExempt(context.Background(), all, p, res); got != c.want {
				t.Errorf("unitWriteExempt = %v, want %v", got, c.want)
			}
		})
	}
}
