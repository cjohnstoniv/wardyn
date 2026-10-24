// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The legacy-shift reads' bounds: each shift reads only the people whose winner its change can move,
// and reads their winners in one statement.

// countingQuerier counts the statements sent through it.
type countingQuerier struct {
	store.Querier
	n int
}

func (c *countingQuerier) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	c.n++
	return c.Querier.Exec(ctx, sql, args...)
}

func (c *countingQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c.n++
	return c.Querier.Query(ctx, sql, args...)
}

func (c *countingQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	c.n++
	return c.Querier.QueryRow(ctx, sql, args...)
}

// inRolledBackTx runs fn on a transaction it then rolls back.
func (e *unitAdminEnv) inRolledBackTx(fn func(tx pgx.Tx)) {
	e.t.Helper()
	tx, err := e.pool.Begin(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // a read-only probe; nothing to keep
	fn(tx)
}

// sameShift reports whether two shifts name the same units in the same order.
func sameShift(a, b legacyShift) bool {
	eq := func(x, y unitActivation) bool { return x.UnitID == y.UnitID }
	return slices.EqualFunc(a.activates, b.activates, eq) && slices.EqualFunc(a.vacates, b.vacates, eq)
}

// renameShiftAllPairsQ is renameShiftQ over every pair a rename was read for before the bound: each
// legacy row of the unit with every legacy row nameOrdersPair accepts, whatever its priority or unit.
func renameShiftAllPairsQ(ctx context.Context, q store.Querier, id uuid.UUID, name string) (legacyShift, error) {
	all, err := store.ListGovernanceMembershipsQ(ctx, q)
	if err != nil {
		return legacyShift{}, err
	}
	var rows []types.GovernanceMembership
	for _, m := range all {
		if m.Legacy && m.UnitID == id {
			rows = append(rows, m)
		}
	}
	was, now, err := winnersAroundQ(ctx, q, legacyPeople(rows, all, nameOrdersPair), func() error {
		return store.RenameGovernanceUnitQ(ctx, q, id, name)
	})
	if err != nil {
		return legacyShift{}, err
	}
	return shiftBetween(was, now, nil, func(a store.MembershipMatch) bool { return a.Unit.ID == id }), nil
}

// TestLegacyShiftBoundMatchesEveryPair: over a seeded mix of legacy rows at tied and differing
// priorities, of groups and of one person's own rows, every rename, move, priority re-post and delete
// shift read over the bounded pairs names exactly the units the read over every pair names.
func TestLegacyShiftBoundMatchesEveryPair(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	group, user := types.CapabilitySubjectGroup, types.CapabilitySubjectUser
	units := []types.GovernanceUnit{e.a, e.a1, e.a2, e.b}
	var ids []uuid.UUID
	for _, s := range []struct {
		unit     types.GovernanceUnit
		st       types.CapabilitySubjectType
		subject  string
		priority int
	}{
		{e.a1, group, "ua-g1", 5}, {e.a1, group, "ua-g2", 3}, {e.a2, group, "ua-g1", 5}, {e.a2, group, "ua-g3", 7},
		{e.b, group, "ua-g2", 5}, {e.b, group, "ua-g4", 3}, {e.a, group, "ua-g3", 5},
		{e.a1, user, "sub-ua-eq", 4}, {e.b, user, "sub-ua-eq", 4}, {e.a2, user, "sub-ua-eq", 6},
	} {
		ids = append(ids, treeMember(t, e.govEnv, s.unit, s.st, s.subject, s.priority, false).ID)
	}
	setLegacy(t, e.govEnv, ids...)
	treeMember(t, e.govEnv, e.a, group, "ua-g4", 9, false) // a current row, which no legacy shift reads
	rows, err := store.ListGovernanceMembershipsQ(context.Background(), e.pool)
	if err != nil {
		t.Fatal(err)
	}
	shifted := map[string]int{}
	compare := func(kind, what string, bounded func(store.Querier) (legacyShift, error), every func(store.Querier) (legacyShift, error)) {
		t.Helper()
		e.inRolledBackTx(func(tx pgx.Tx) {
			got, err := bounded(tx)
			if err != nil {
				t.Fatalf("%s %s, bounded: %v", kind, what, err)
			}
			want, err := every(tx)
			if err != nil {
				t.Fatalf("%s %s, every pair: %v", kind, what, err)
			}
			if !sameShift(got, want) {
				t.Errorf("%s %s: bounded %+v, every pair %+v", kind, what, got, want)
			}
			if len(want.activates)+len(want.vacates) > 0 {
				shifted[kind]++
			}
		})
	}
	ctx := context.Background()
	for _, u := range units {
		for _, name := range []string{"ua-0", "ua-a15", "ua-zz"} {
			compare("rename", u.Name+" to "+name,
				func(q store.Querier) (legacyShift, error) { return renameShiftQ(ctx, q, u.ID, name) },
				func(q store.Querier) (legacyShift, error) { return renameShiftAllPairsQ(ctx, q, u.ID, name) })
		}
	}
	for _, row := range rows {
		if !row.Legacy {
			continue
		}
		what := fmt.Sprintf("%s %s in %s at %d", row.SubjectType, row.Subject, row.UnitID, row.Priority)
		for _, to := range units {
			if to.ID == row.UnitID || slices.ContainsFunc(rows, func(m types.GovernanceMembership) bool {
				return m.UnitID == to.ID && m.SubjectType == row.SubjectType && m.Subject == row.Subject
			}) {
				continue // the move itself is refused there
			}
			move := func(q store.Querier) func() error {
				return func() error { _, _, err := moveMembershipQ(ctx, q, row.ID, to.ID); return err }
			}
			compare("move", what+" to "+to.Name,
				func(q store.Querier) (legacyShift, error) { return moveShiftQ(ctx, q, row, to.ID) },
				func(q store.Querier) (legacyShift, error) {
					return legacyShiftQ(ctx, q, row, to.ID, nameOrdersPair, move(q))
				})
		}
		for _, p := range []int{0, 3, 4, 5, 6, 8} {
			a := row
			a.Priority = p
			upsert := func(q store.Querier) func() error {
				return func() error { _, err := store.UpsertGovernanceMembershipQ(ctx, q, a); return err }
			}
			compare("re-post", fmt.Sprintf("%s at %d", what, p),
				func(q store.Querier) (legacyShift, error) { return upsertShiftQ(ctx, q, &row, a) },
				func(q store.Querier) (legacyShift, error) {
					return legacyShiftQ(ctx, q, row, uuid.Nil, nameOrdersPair, upsert(q))
				})
		}
		compare("delete", what,
			func(q store.Querier) (legacyShift, error) {
				act, err := deleteActivationsQ(ctx, q, row)
				return legacyShift{activates: act}, err
			},
			func(q store.Querier) (legacyShift, error) {
				return legacyShiftQ(ctx, q, row, uuid.Nil, nil, func() error { return store.DeleteGovernanceMembershipQ(ctx, q, row.ID) })
			})
	}
	for _, kind := range []string{"rename", "move", "re-post", "delete"} {
		if shifted[kind] == 0 {
			t.Errorf("no %s in the seeded set shifts anyone, so the comparison proves nothing for it", kind)
		}
	}
}

// TestLegacyShiftReadsDoNotGrowWithPairs: a rename of a unit whose n legacy group rows all tie with m
// legacy group rows of another unit reads n×m people, in the same number of statements for 1×2 as for
// 3×4; and with no row of another unit at the unit's priority it reads no one, in one statement.
func TestLegacyShiftReadsDoNotGrowWithPairs(t *testing.T) {
	statements := func(n, m, otherPriority int) int {
		t.Helper()
		e := newUnitAdminEnv(t, false)
		var ids []uuid.UUID
		for i := range n {
			ids = append(ids, treeMember(t, e.govEnv, e.a1, types.CapabilitySubjectGroup, fmt.Sprintf("ua-n%d", i), 5, false).ID)
		}
		for j := range m {
			ids = append(ids, treeMember(t, e.govEnv, e.b, types.CapabilitySubjectGroup, fmt.Sprintf("ua-m%d", j), otherPriority, false).ID)
		}
		setLegacy(t, e.govEnv, ids...)
		var count int
		e.inRolledBackTx(func(tx pgx.Tx) {
			q := &countingQuerier{Querier: tx}
			shift, err := renameShiftQ(context.Background(), q, e.a1.ID, "ua-zz")
			if err != nil {
				t.Fatal(err)
			}
			tied := otherPriority == 5
			if got := slices.ContainsFunc(shift.activates, func(a unitActivation) bool { return a.UnitID == e.b.ID }); got != tied {
				t.Errorf("%d×%d at %d: ua-b activated = %v, want %v", n, m, otherPriority, got, tied)
			}
			count = q.n
		})
		return count
	}
	small, large := statements(1, 2, 5), statements(3, 4, 5)
	if small != large {
		t.Errorf("a rename read in %d statements over 1×2 tied rows and %d over 3×4, want the same", small, large)
	}
	if n := statements(3, 4, 6); n != 1 {
		t.Errorf("a rename with no tie read in %d statements, want 1 (the membership list)", n)
	}
}
