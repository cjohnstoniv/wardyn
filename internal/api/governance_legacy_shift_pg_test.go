// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The legacy-shift reads' bounds: each shift reads only the people whose winner its change can move,
// and reads their winners in one statement; a rename reads one person for each tied row of another
// unit, never one for each tied pair.

// countingQuerier counts the statements sent through it and the rows they return.
type countingQuerier struct {
	store.Querier
	n, rows int
}

func (c *countingQuerier) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	c.n++
	return c.Querier.Exec(ctx, sql, args...)
}

func (c *countingQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c.n++
	rows, err := c.Querier.Query(ctx, sql, args...)
	if err != nil {
		return rows, err
	}
	return countedRows{Rows: rows, n: &c.rows}, nil
}

func (c *countingQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	c.n++
	c.rows++
	return c.Querier.QueryRow(ctx, sql, args...)
}

// countedRows counts the rows read through it into n.
type countedRows struct {
	pgx.Rows
	n *int
}

func (r countedRows) Next() bool {
	ok := r.Rows.Next()
	if ok {
		*r.n++
	}
	return ok
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

// renameShiftPairsQ is renameShiftQ read over the person of every pair of a legacy row of unit id and
// another legacy row that pair accepts (nil for every row), the enumeration a rename ran before it read
// one person for each tied row of another unit: n×m people for n tied rows of the unit and m of others.
func renameShiftPairsQ(ctx context.Context, q store.Querier, id uuid.UUID, name string, pair func(d, l types.GovernanceMembership) bool) (legacyShift, error) {
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
	was, now, err := winnersAroundQ(ctx, q, legacyPeople(rows, all, pair), func() error {
		return store.RenameGovernanceUnitQ(ctx, q, id, name)
	})
	if err != nil {
		return legacyShift{}, err
	}
	return shiftBetween(was, now, nil, func(a store.MembershipMatch) bool { return a.Unit.ID == id }), nil
}

// tiedElsewhere is the pair a rename of unit id was read over before the rivals query: a legacy row of
// the unit and one of another unit tied with it up to the name.
func tiedElsewhere(id uuid.UUID) func(d, l types.GovernanceMembership) bool {
	return func(d, l types.GovernanceMembership) bool { return l.UnitID != id && nameTiesPair(d, l) }
}

// legacyRowSpec is a legacy row a test seeds.
type legacyRowSpec struct {
	unit     types.GovernanceUnit
	st       types.CapabilitySubjectType
	subject  string
	priority int
}

func (r legacyRowSpec) String() string {
	return fmt.Sprintf("%s %s %q at %d", r.unit.Name, r.st, r.subject, r.priority)
}

// seedLegacyRowsQ writes rows on q as legacy rows.
func seedLegacyRowsQ(t *testing.T, q store.Querier, rows []legacyRowSpec) {
	t.Helper()
	for _, r := range rows {
		if err := store.InsertGovernanceMembershipQ(context.Background(), q, types.GovernanceMembership{ID: uuid.New(), SubjectType: r.st,
			Subject: r.subject, UnitID: r.unit.ID, Priority: r.priority, CreatedAt: time.Now(), CreatedBy: "seed", Legacy: true}); err != nil {
			t.Fatalf("seed legacy row %s: %v", r, err)
		}
	}
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
				func(q store.Querier) (legacyShift, error) {
					return renameShiftPairsQ(ctx, q, u.ID, name, nameOrdersPair)
				})
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

// TestLegacyShiftMatchesEveryPairOverRandomTies: over a fixed set and seeded random sets of legacy
// rows of users, groups, user types and everyone, most tied at one priority across units, every rename
// shift names exactly the units the read over every pair names, and the units the read over the
// unit's tied pairs names; and every move, priority re-post and delete shift names the units its read
// over every pair names. Each kind shifts someone in some set, and some rename's answer needs a person
// two different groups place, the pairs the rivals read stands in for. In the fixed set ua-a1's ua-g1
// ranks first outside ua-a1 at 9, so of its two groups tied with ua-b's ua-g3 only ua-g2's person
// trades ua-a1 for ua-b on a rename past ua-b, and no person of one subject does.
func TestLegacyShiftMatchesEveryPairOverRandomTies(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	ctx := context.Background()
	units := []types.GovernanceUnit{e.a, e.a1, e.a2, e.b}
	for _, name := range []string{"ua-B1", "ua_c"} {
		units = append(units, treeChild(t, e.govEnv, uaUnitBody(name, e.b.ID, map[string]any{}, nil)))
	}
	group, user, userType, all := types.CapabilitySubjectGroup, types.CapabilitySubjectUser, types.CapabilitySubjectUserType, types.CapabilitySubjectAll
	subjects := []struct {
		st      types.CapabilitySubjectType
		subject string
	}{
		{group, "ua-g1"}, {group, "ua-G2"}, {group, "ua_g3"}, {group, "ua-g4"}, {group, "ua-g5"},
		{user, "sub-ua-r1"}, {user, "ua-r1@example.com"}, {user, "sub-ua-r2"},
		{userType, "ut-ua-1"}, {userType, "ut-ua-2"}, {all, ""},
	}
	sets := [][]legacyRowSpec{{
		{e.a1, group, "ua-g1", 5}, {e.a1, group, "ua-g2", 5}, {e.a, group, "ua-g1", 9}, {e.b, group, "ua-g3", 5},
		{e.a2, user, "sub-ua-r1", 5}, {e.b, user, "sub-ua-r1", 5}, {e.a2, userType, "ut-ua-1", 5}, {e.a1, userType, "ut-ua-1", 5},
	}}
	rng := rand.New(rand.NewPCG(1, 2))
	for range 6 {
		var set []legacyRowSpec
		taken := map[string]bool{}
		for len(set) < 14 {
			u, s := units[rng.IntN(len(units))], subjects[rng.IntN(len(subjects))]
			if key := u.Name + " " + string(s.st) + " " + s.subject; !taken[key] {
				taken[key] = true
				set = append(set, legacyRowSpec{u, s.st, s.subject, []int{3, 5, 5, 5, 7}[rng.IntN(5)]})
			}
		}
		sets = append(sets, set)
	}
	shifted := map[string]int{}
	crossed := 0
	for i, set := range sets {
		e.inRolledBackTx(func(tx pgx.Tx) {
			seedLegacyRowsQ(t, tx, set)
			rows, err := store.ListGovernanceMembershipsQ(ctx, tx)
			if err != nil {
				t.Fatal(err)
			}
			read := func(kind, what string, f func() (legacyShift, error)) legacyShift {
				t.Helper()
				shift, err := f()
				if err != nil {
					t.Fatalf("set %d, %s %s (%s): %v", i, kind, what, setString(set), err)
				}
				return shift
			}
			compare := func(kind, what string, got legacyShift, want legacyShift, over string) {
				t.Helper()
				if !sameShift(got, want) {
					t.Errorf("set %d, %s %s: %+v, over %s %+v (%s)", i, kind, what, got, over, want, setString(set))
				}
				if len(want.activates)+len(want.vacates) > 0 {
					shifted[kind]++
				}
			}
			for _, u := range units {
				for _, name := range []string{"ua-0", "ua-a15", "ua-B0", "ua-bz", "ua_d"} {
					what := u.Name + " to " + name
					got := read("rename", what, func() (legacyShift, error) { return renameShiftQ(ctx, tx, u.ID, name) })
					every := read("rename", what, func() (legacyShift, error) { return renameShiftPairsQ(ctx, tx, u.ID, name, nil) })
					compare("rename", what, got, every, "every pair")
					tied := read("rename", what, func() (legacyShift, error) {
						return renameShiftPairsQ(ctx, tx, u.ID, name, tiedElsewhere(u.ID))
					})
					compare("rename", what, got, tied, "the tied pairs")
					one := read("rename", what, func() (legacyShift, error) {
						return renameShiftPairsQ(ctx, tx, u.ID, name, func(d, l types.GovernanceMembership) bool {
							return tiedElsewhere(u.ID)(d, l) && d.Subject == l.Subject
						})
					})
					if !sameShift(one, every) {
						crossed++
					}
				}
			}
			for _, row := range rows {
				if !row.Legacy {
					continue
				}
				what := fmt.Sprintf("%s %q in %s at %d", row.SubjectType, row.Subject, row.UnitID, row.Priority)
				for _, to := range units {
					if to.ID == row.UnitID || slices.ContainsFunc(rows, func(m types.GovernanceMembership) bool {
						return m.UnitID == to.ID && m.SubjectType == row.SubjectType && m.Subject == row.Subject
					}) {
						continue // the move itself is refused there
					}
					move := func() error { _, _, err := moveMembershipQ(ctx, tx, row.ID, to.ID); return err }
					got := read("move", what, func() (legacyShift, error) { return moveShiftQ(ctx, tx, row, to.ID) })
					every := read("move", what, func() (legacyShift, error) { return legacyShiftQ(ctx, tx, row, to.ID, nil, move) })
					compare("move", what+" to "+to.Name, got, every, "every pair")
				}
				for _, p := range []int{3, 7} {
					a := row
					a.Priority = p
					upsert := func() error { _, err := store.UpsertGovernanceMembershipQ(ctx, tx, a); return err }
					got := read("re-post", what, func() (legacyShift, error) { return upsertShiftQ(ctx, tx, &row, a) })
					every := read("re-post", what, func() (legacyShift, error) { return legacyShiftQ(ctx, tx, row, uuid.Nil, nil, upsert) })
					compare("re-post", fmt.Sprintf("%s at %d", what, p), got, every, "every pair")
				}
				remove := func() error { return store.DeleteGovernanceMembershipQ(ctx, tx, row.ID) }
				act, err := deleteActivationsQ(ctx, tx, row)
				if err != nil {
					t.Fatalf("set %d, delete %s: %v", i, what, err)
				}
				every := read("delete", what, func() (legacyShift, error) { return legacyShiftQ(ctx, tx, row, uuid.Nil, nil, remove) })
				compare("delete", what, legacyShift{activates: act}, legacyShift{activates: every.activates}, "every pair")
			}
		})
	}
	for _, kind := range []string{"rename", "move", "re-post", "delete"} {
		if shifted[kind] == 0 {
			t.Errorf("no %s in the seeded sets shifts anyone, so the comparison proves nothing for it", kind)
		}
	}
	if crossed == 0 {
		t.Error("no rename's answer needs a person two different groups place, so the sets prove nothing for the pairs the rivals read stands in for")
	}
}

// setString is a seeded set as its failure message names it.
func setString(set []legacyRowSpec) string {
	var b strings.Builder
	for i, r := range set {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(r.String())
	}
	return b.String()
}

// TestLegacyRenameShiftReadsGrowWithRows: a rename of a unit whose n legacy group rows all tie with m
// legacy group rows of another unit reads at most 3(n+m) rows, the rivals and then one person's winner
// for each before the rename and after it, in as many statements at 50×60 as at 2×3; the read over
// the tied pairs, a rename's before the rivals query, reads the n×m people twice, past that bound at
// 50×60, so the bound fails that read. With no row of another unit at the unit's priority it reads no
// one, in one statement.
func TestLegacyRenameShiftReadsGrowWithRows(t *testing.T) {
	e := newUnitAdminEnv(t, false)
	ctx := context.Background()
	type reads struct{ statements, rows int }
	measure := func(n, m, otherPriority int) (rivals, pairs reads) {
		t.Helper()
		var set []legacyRowSpec
		for i := range n {
			set = append(set, legacyRowSpec{e.a1, types.CapabilitySubjectGroup, fmt.Sprintf("ua-n%d", i), 5})
		}
		for j := range m {
			set = append(set, legacyRowSpec{e.b, types.CapabilitySubjectGroup, fmt.Sprintf("ua-m%d", j), otherPriority})
		}
		e.inRolledBackTx(func(tx pgx.Tx) {
			seedLegacyRowsQ(t, tx, set)
			read := func(f func(q store.Querier) (legacyShift, error)) (legacyShift, reads) {
				t.Helper()
				q := &countingQuerier{Querier: tx}
				shift, err := f(q)
				if err != nil {
					t.Fatalf("%d×%d at %d: %v", n, m, otherPriority, err)
				}
				return shift, reads{q.n, q.rows}
			}
			var got, want legacyShift
			got, rivals = read(func(q store.Querier) (legacyShift, error) { return renameShiftQ(ctx, q, e.a1.ID, "ua-zz") })
			want, pairs = read(func(q store.Querier) (legacyShift, error) {
				return renameShiftPairsQ(ctx, q, e.a1.ID, "ua-zz", tiedElsewhere(e.a1.ID))
			})
			if !sameShift(got, want) {
				t.Errorf("%d×%d at %d: %+v, over the tied pairs %+v", n, m, otherPriority, got, want)
			}
			tied := otherPriority == 5
			if activated := slices.ContainsFunc(got.activates, func(a unitActivation) bool { return a.UnitID == e.b.ID }); activated != tied {
				t.Errorf("%d×%d at %d: ua-b activated = %v, want %v", n, m, otherPriority, activated, tied)
			}
		})
		return rivals, pairs
	}
	small, _ := measure(2, 3, 5)
	if bound := 3 * (2 + 3); small.rows > bound {
		t.Errorf("a rename over 2×3 tied rows read %d rows, want at most %d", small.rows, bound)
	}
	large, pairs := measure(50, 60, 5)
	if bound := 3 * (50 + 60); large.rows > bound || pairs.rows <= bound {
		t.Errorf("a rename over 50×60 tied rows read %d rows, and %d over the tied pairs; want at most %d, and past it over the pairs",
			large.rows, pairs.rows, bound)
	}
	if small.statements != large.statements {
		t.Errorf("a rename read in %d statements over 2×3 tied rows and %d over 50×60, want the same", small.statements, large.statements)
	}
	if none, _ := measure(50, 60, 6); none.statements != 1 || none.rows != 0 {
		t.Errorf("a rename with no tie read %d rows in %d statements, want none in 1 (the rivals read)", none.rows, none.statements)
	}
}
