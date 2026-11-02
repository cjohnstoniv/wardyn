// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

// Live tests for LegacyRenameRivalsQ, the pairs a unit rename reads its legacy shift over. Guarded by
// WARDYN_TEST_PG like every *_pg_test.go here.

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_LegacyRenameRivals: of unit u's two group rows tied with v's rv-gc row, the rival is
// rv-gb's, whose group has no row outside u: rv-ga's row in w ranks first outside u for the person
// rv-ga and rv-gc place. v's rv-gd row, whose group's row in u ranks before every tied row, pairs with
// u's first tied row. A user's rows in v and w each pair with u's row of that user at their priority.
// Another user's row, a row at a priority u has no row at, and a current row pair with none.
func TestPG_LegacyRenameRivals(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	uniq := uuid.NewString()[:8]
	u := seedGovernanceUnit(t, st, "rv-u-"+uniq)
	v := seedGovernanceUnit(t, st, "rv-v-"+uniq)
	w := seedGovernanceUnit(t, st, "rv-w-"+uniq)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // every row is seeded for this read alone
	mine := map[uuid.UUID]bool{}
	row := func(unit types.GovernanceUnit, kind types.CapabilitySubjectType, subject string, priority int, legacy bool) uuid.UUID {
		t.Helper()
		m := types.GovernanceMembership{ID: uuid.New(), SubjectType: kind, Subject: subject + "-" + uniq, UnitID: unit.ID,
			Priority: priority, CreatedAt: time.Now(), CreatedBy: "seed", Legacy: legacy}
		if err := store.InsertGovernanceMembershipQ(ctx, tx, m); err != nil {
			t.Fatalf("seed %s %q in %s: %v", kind, m.Subject, unit.Name, err)
		}
		mine[m.ID] = true
		return m.ID
	}
	group, user := types.CapabilitySubjectGroup, types.CapabilitySubjectUser
	ga := row(u, group, "rv-ga", 5, true)
	gb := row(u, group, "rv-gb", 5, true)
	row(w, group, "rv-ga", 9, true)
	gc := row(v, group, "rv-gc", 5, true)
	row(u, group, "rv-gd", 7, true)
	gd := row(v, group, "rv-gd", 5, true)
	su := row(u, user, "sub-rv", 5, true)
	sv := row(v, user, "sub-rv", 5, true)
	sw := row(w, user, "sub-rv", 5, true)
	row(v, user, "sub-rv-other", 5, true)
	row(v, group, "rv-ge", 3, true)
	row(v, group, "rv-gf", 5, false)
	pairs, err := store.LegacyRenameRivalsQ(ctx, tx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]uuid.UUID{}
	for _, p := range pairs {
		if mine[p[1].ID] {
			if _, dup := got[p[1].ID]; dup {
				t.Errorf("row %s paired twice", p[1].ID)
			}
			got[p[1].ID] = p[0].ID
		}
	}
	if want := map[uuid.UUID]uuid.UUID{gc: gb, gd: ga, sv: su, sw: su}; !maps.Equal(got, want) {
		t.Errorf("rivals (row outside u: its row of u) = %v, want %v", got, want)
	}
}
