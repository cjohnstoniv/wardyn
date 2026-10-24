// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The legacy shift's database work under the graph lock, read from the planner rather than from row
// or statement counts: each statement a move, rename or delete shift sends is first run under EXPLAIN
// (ANALYZE, BUFFERS, FORMAT JSON) and its plan logged. WARDYN_EXPLAIN_SCALE=1 seeds thousands of
// units and tens of thousands of rows; the default seeds two small sizes so the shard stays cheap.

// planNode is one node of an EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) plan.
type planNode struct {
	NodeType       string     `json:"Node Type"`
	Relation       string     `json:"Relation Name"`
	Index          string     `json:"Index Name"`
	Parallel       bool       `json:"Parallel Aware"`
	Rows           float64    `json:"Actual Rows"`
	Loops          float64    `json:"Actual Loops"`
	Time           float64    `json:"Actual Total Time"`
	RemovedFilter  float64    `json:"Rows Removed by Filter"`
	RemovedRecheck float64    `json:"Rows Removed by Index Recheck"`
	Hit            float64    `json:"Shared Hit Blocks"`
	Read           float64    `json:"Shared Read Blocks"`
	Plans          []planNode `json:"Plans"`
}

// walk calls fn on n and every node under it, with its depth.
func (n planNode) walk(depth int, fn func(n planNode, depth int)) {
	fn(n, depth)
	for _, c := range n.Plans {
		c.walk(depth+1, fn)
	}
}

// membershipRowsExamined is the membership rows n's scans of governance_memberships looked at over all
// their loops: the rows they returned and those their filter or recheck threw away.
func (n planNode) membershipRowsExamined() float64 {
	var sum float64
	n.walk(0, func(n planNode, _ int) {
		if n.Relation == "governance_memberships" {
			sum += (n.Rows + n.RemovedFilter + n.RemovedRecheck) * n.Loops
		}
	})
	return sum
}

// membershipRowsReturned is the membership rows n's scans of governance_memberships returned over all
// their loops: for a winners read, each person's own matching rows plus the winner's re-read.
func (n planNode) membershipRowsReturned() float64 {
	var sum float64
	n.walk(0, func(n planNode, _ int) {
		if n.Relation == "governance_memberships" {
			sum += n.Rows * n.Loops
		}
	})
	return sum
}

// usesIndex reports whether any node of n reads through index.
func (n planNode) usesIndex(index string) bool {
	var found bool
	n.walk(0, func(n planNode, _ int) { found = found || n.Index == index })
	return found
}

// repeatedMembershipSeqScan is a sequential scan of governance_memberships n runs more than once (a
// parallel scan's loops are its workers), "" for none.
func (n planNode) repeatedMembershipSeqScan() string {
	var found string
	n.walk(0, func(n planNode, _ int) {
		if found == "" && n.NodeType == "Seq Scan" && n.Relation == "governance_memberships" && !n.Parallel && n.Loops > 1 {
			found = fmt.Sprintf("Seq Scan on governance_memberships, %.0f loops of %.0f rows", n.Loops, n.Rows+n.RemovedFilter)
		}
	})
	return found
}

// explainedStatement is one statement a shift sent and its plan.
type explainedStatement struct {
	label  string
	people int // the people a LegacyWinnersQ read, 0 for any other statement
	plan   planNode
	execMS float64
}

// explainingQuerier runs every statement sent through it under EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)
// in a savepoint it rolls back, keeps the plan, then sends the statement itself.
type explainingQuerier struct {
	store.Querier
	t     *testing.T
	stmts []explainedStatement
}

func (x *explainingQuerier) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	x.explain(ctx, sql, args)
	return x.Querier.Exec(ctx, sql, args...)
}

func (x *explainingQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	x.explain(ctx, sql, args)
	return x.Querier.Query(ctx, sql, args...)
}

func (x *explainingQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	x.explain(ctx, sql, args)
	return x.Querier.QueryRow(ctx, sql, args...)
}

func (x *explainingQuerier) explain(ctx context.Context, sql string, args []any) {
	x.t.Helper()
	fields := strings.Fields(sql)
	if len(fields) == 0 || !strings.Contains(" SELECT WITH INSERT UPDATE DELETE ", " "+strings.ToUpper(fields[0])+" ") {
		return // SAVEPOINT and ROLLBACK TO have no plan
	}
	s := explainedStatement{label: strings.Join(fields, " ")}
	s.plan, s.execMS = x.plan(ctx, `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) `+sql, args)
	switch {
	case strings.Contains(sql, "WITH ORDINALITY AS c(p, i)"):
		s.label = "LegacyWinnersQ"
		var claims []json.RawMessage
		b, ok := args[0].([]byte)
		if !ok || json.Unmarshal(b, &claims) != nil {
			x.t.Fatalf("LegacyWinnersQ claims argument %T is not a JSON array", args[0])
		}
		s.people = len(claims)
		x.stmts = append(x.stmts, s)
		// pgx's default exec mode prepares a statement once per connection, and from its sixth run
		// Postgres may keep a generic plan, one made without $1's value: read that plan too.
		x.exec(ctx, `PREPARE explain_generic(jsonb) AS `+sql)
		g := s
		g.label = "LegacyWinnersQ (generic plan)"
		g.plan, g.execMS = x.plan(ctx, `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) EXECUTE explain_generic('`+strings.ReplaceAll(string(b), "'", "''")+`')`,
			nil, `SET LOCAL plan_cache_mode = force_generic_plan`)
		x.exec(ctx, `DEALLOCATE explain_generic`)
		x.stmts = append(x.stmts, g)
		return
	case strings.Contains(sql, "row_number() OVER"):
		s.label = "LegacyRenameRivalsQ"
	case len(s.label) > 90:
		s.label = s.label[:90] + "..."
	}
	x.stmts = append(x.stmts, s)
}

// plan runs setup and then explain, an EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON), in a savepoint it rolls
// back, and returns the plan and its execution time.
func (x *explainingQuerier) plan(ctx context.Context, explain string, args []any, setup ...string) (planNode, float64) {
	x.t.Helper()
	x.exec(ctx, `SAVEPOINT explain_probe`)
	for _, st := range setup {
		x.exec(ctx, st)
	}
	var raw []byte
	err := x.Querier.QueryRow(ctx, explain, args...).Scan(&raw)
	if _, rerr := x.Querier.Exec(ctx, `ROLLBACK TO SAVEPOINT explain_probe`); err == nil && rerr != nil {
		err = rerr
	}
	if _, rerr := x.Querier.Exec(ctx, `RELEASE SAVEPOINT explain_probe`); err == nil && rerr != nil {
		err = rerr
	}
	if err != nil {
		x.t.Fatalf("%s: %v", strings.Join(strings.Fields(explain), " "), err)
	}
	var out []struct {
		Plan   planNode `json:"Plan"`
		ExecMS float64  `json:"Execution Time"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out) != 1 {
		x.t.Fatalf("decode EXPLAIN JSON (%v): %s", err, raw)
	}
	return out[0].Plan, out[0].ExecMS
}

// exec sends stmt, which takes no arguments, past the EXPLAIN.
func (x *explainingQuerier) exec(ctx context.Context, stmt string) {
	x.t.Helper()
	if _, err := x.Querier.Exec(ctx, stmt); err != nil {
		x.t.Fatalf("%s: %v", stmt, err)
	}
}

// subjectIndex is the index a person's membership lookup reads (migration 0233).
const subjectIndex = "governance_memberships_subject"

// indexedFrom is the table size from which the plan rules on the index hold: below it the planner may
// rightly prefer a scan of a few pages.
const indexedFrom = 3000

// explainTies is the size of the probe's tie group, the same at every size: the people a move or a
// rename of the probe unit reads stay fixed while the table grows around them.
const explainTies = 30

// seedExplainScaleQ seeds n units on q through the production writers (WriteGovernanceUnitQ under the
// graph lock, InsertGovernanceMembershipQ for the legacy rows), and returns the units and the probe
// row: unit 0's group row in the tie group. Every row is legacy.
//
//   - tie group: one group row ex-tie-j at priority 7 in units 0..explainTies-1, plus one person's sub
//     and email rows at priority 5 in each of them;
//   - fixed: an all row (priority 1) and a user_type row (priority 2) in units 0-2;
//   - background, every unit but 0: six group rows at priority 3 (one tie group of 6×(units-1) rows),
//     two subs shared with the next unit and two emails, at priorities 0-4 and 8-9.
func seedExplainScaleQ(t *testing.T, q store.Querier, n int) (units []types.GovernanceUnit, probe types.GovernanceMembership, rows int) {
	t.Helper()
	ctx := context.Background()
	for i := range n {
		u, err := store.WriteGovernanceUnitQ(ctx, q, uuid.Nil, func([]types.GovernanceUnit, []types.GovernanceMembership) (types.GovernanceUnit, error) {
			return types.GovernanceUnit{Name: fmt.Sprintf("ex-%05d", i), CreatedBy: "seed"}, nil
		})
		if err != nil {
			t.Fatalf("seed unit %d: %v", i, err)
		}
		units = append(units, u)
	}
	put := func(u types.GovernanceUnit, st types.CapabilitySubjectType, subject string, priority int) types.GovernanceMembership {
		t.Helper()
		m := types.GovernanceMembership{ID: uuid.New(), SubjectType: st, Subject: subject, UnitID: u.ID, Priority: priority,
			CreatedAt: time.Now(), CreatedBy: "seed", Legacy: true}
		if err := store.InsertGovernanceMembershipQ(ctx, q, m); err != nil {
			t.Fatalf("seed %s %q in %s: %v", st, subject, u.Name, err)
		}
		rows++
		return m
	}
	group, user := types.CapabilitySubjectGroup, types.CapabilitySubjectUser
	for j := range min(explainTies, n) {
		m := put(units[j], group, fmt.Sprintf("ex-tie-%d", j), 7)
		if j == 0 {
			probe = m
		}
		put(units[j], user, "sub-ex-tie", 5)
		put(units[j], user, "ex-tie@corp.example", 5)
	}
	for j := range min(3, n) {
		put(units[j], types.CapabilitySubjectAll, "", 1)
		put(units[j], types.CapabilitySubjectUserType, types.UserTypeStandard, 2)
	}
	for i := 1; i < n; i++ {
		for k := range 6 {
			put(units[i], group, fmt.Sprintf("ex-bg-%d-%d", i, k), 3)
		}
		put(units[i], user, fmt.Sprintf("sub-ex-bg-%d", i/2), i%5)
		put(units[i], user, fmt.Sprintf("sub-ex-bg-%d", (i+1)/2+n), 8+i%2)
		put(units[i], user, fmt.Sprintf("bg%d@corp.example", i), i%5)
		put(units[i], user, fmt.Sprintf("bg%d-alt@corp.example", i), 8+i%2)
	}
	if _, err := q.Exec(ctx, `ANALYZE governance_memberships, governance_units`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	return units, probe, rows
}

// shiftCost is what one shift's statements cost at one size.
type shiftCost struct {
	people            int
	examinedPerPerson float64 // membership rows a LegacyWinnersQ examined per person, its largest read
	buffersPerPerson  float64 // shared blocks (hit + read) a LegacyWinnersQ touched per person, its largest read
}

// logShift logs each statement's plan, trimmed to node type, rows, loops, buffers and time.
func logShift(t *testing.T, head string, stmts []explainedStatement) {
	t.Helper()
	var b strings.Builder
	for _, s := range stmts {
		fmt.Fprintf(&b, "%s | %s", head, s.label)
		if s.people > 0 {
			fmt.Fprintf(&b, " people=%d examined_per_person=%.1f returned_per_person=%.1f uses_%s=%v", s.people,
				s.plan.membershipRowsExamined()/float64(s.people), s.plan.membershipRowsReturned()/float64(s.people),
				subjectIndex, s.plan.usesIndex(subjectIndex))
		}
		fmt.Fprintf(&b, " exec=%.3fms memberships_examined=%.0f\n", s.execMS, s.plan.membershipRowsExamined())
		s.plan.walk(1, func(n planNode, depth int) {
			on := ""
			if n.Relation != "" {
				on = " on " + n.Relation
			}
			if n.Index != "" {
				on += " using " + n.Index
			}
			fmt.Fprintf(&b, "%s%s%s rows=%.0f loops=%.0f removed=%.0f hit=%.0f read=%.0f time=%.3fms\n",
				strings.Repeat("  ", depth), n.NodeType, on, n.Rows, n.Loops, n.RemovedFilter+n.RemovedRecheck, n.Hit, n.Read, n.Time*n.Loops)
		})
	}
	t.Log("\n" + b.String())
}

// costOf is the LegacyWinnersQ cost of stmts, and the first repeated sequential scan of the membership
// table any statement ran ("" for none).
func costOf(stmts []explainedStatement) (c shiftCost, repeated string) {
	for _, s := range stmts {
		if r := s.plan.repeatedMembershipSeqScan(); r != "" && repeated == "" {
			repeated = s.label + ": " + r
		}
		if s.people == 0 {
			continue
		}
		c.people = s.people
		c.examinedPerPerson = max(c.examinedPerPerson, s.plan.membershipRowsExamined()/float64(s.people))
		c.buffersPerPerson = max(c.buffersPerPerson, (s.plan.Hit+s.plan.Read)/float64(s.people))
	}
	return c, repeated
}

// TestLegacyShiftLockedWorkIsBounded: the statements a move, a rename and a delete shift send under the
// graph lock do per-person work that does not grow with the membership table. The move and the rename
// read a tie group of fixed size, so their people stay fixed while the background grows; the delete
// reads every person a row may follow, so its people grow with the table, and its per-person cost
// must not. Assertions, each from the plan:
//
//   - a LegacyWinnersQ's membership rows examined per person (rows returned plus rows its filter or
//     recheck discarded, over every loop) at a size at most 1.5× that at the size before, plus 8: an
//     index lookup examines the rows a person matches, which the seed holds fixed across sizes, while a
//     scan of the table per person grows with every size step (3× or more here);
//   - its shared blocks per person likewise, plus 16 for a btree gaining a level;
//   - every other statement (the membership list, LegacyRenameRivalsQ, the change itself) examines at
//     most 3× the table's rows: one pass over it, plus a primary-key lookup for each row it returns;
//   - at 3000 rows or more (indexedFrom; below it the planner may rightly prefer a scan of a few
//     pages): no statement runs a sequential scan of governance_memberships more than once (once per
//     person is the quadratic shape), and each LegacyWinnersQ, under its custom plan and under a forced
//     generic plan alike, reads governance_memberships_subject and examines at most twice the rows it
//     returns plus two per person: about each person's own rows.
func TestLegacyShiftLockedWorkIsBounded(t *testing.T) {
	sizes := []int{40, 300}
	if os.Getenv("WARDYN_EXPLAIN_SCALE") == "1" {
		sizes = []int{300, 1000, 3000}
	}
	pool := throwawayPGPool(t)
	ctx := context.Background()
	shifts := []struct {
		name string
		run  func(q store.Querier, units []types.GovernanceUnit, probe types.GovernanceMembership) error
	}{
		{"move", func(q store.Querier, units []types.GovernanceUnit, probe types.GovernanceMembership) error {
			_, err := moveShiftQ(ctx, q, probe, units[1].ID)
			return err
		}},
		{"rename", func(q store.Querier, units []types.GovernanceUnit, _ types.GovernanceMembership) error {
			_, err := renameShiftQ(ctx, q, units[0].ID, "ex-zz")
			return err
		}},
		{"delete", func(q store.Querier, _ []types.GovernanceUnit, probe types.GovernanceMembership) error {
			_, err := deleteActivationsQ(ctx, q, probe)
			return err
		}},
	}
	prev := map[string]shiftCost{}
	for si, n := range sizes {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		units, probe, rows := seedExplainScaleQ(t, tx, n)
		t.Logf("size %d: %d units, %d legacy membership rows, seeded in %s", si, n, rows, time.Since(start).Round(time.Millisecond))
		for _, sh := range shifts {
			x := &explainingQuerier{Querier: tx, t: t}
			if err := sh.run(x, units, probe); err != nil {
				t.Fatalf("%s at %d units: %v", sh.name, n, err)
			}
			logShift(t, fmt.Sprintf("units=%d rows=%d %s", n, rows, sh.name), x.stmts)
			c, repeated := costOf(x.stmts)
			if rows >= indexedFrom && repeated != "" {
				t.Errorf("%s at %d rows: %s", sh.name, rows, repeated)
			}
			for _, s := range x.stmts {
				if s.people > 0 && rows >= indexedFrom {
					if !s.plan.usesIndex(subjectIndex) {
						t.Errorf("%s at %d rows: %s does not read %s", sh.name, rows, s.label, subjectIndex)
					}
					if e, r := s.plan.membershipRowsExamined(), s.plan.membershipRowsReturned(); e > 2*r+2*float64(s.people) {
						t.Errorf("%s at %d rows: %s examined %.0f membership rows to return %.0f over %d people, want about each person's own rows",
							sh.name, rows, s.label, e, r, s.people)
					}
				}
				if e := s.plan.membershipRowsExamined(); s.people == 0 && e > 3*float64(rows)+100 {
					t.Errorf("%s: %s examined %.0f membership rows of %d, want one pass", sh.name, s.label, e, rows)
				}
			}
			if p, ok := prev[sh.name]; ok {
				if c.examinedPerPerson > 1.5*p.examinedPerPerson+8 {
					t.Errorf("%s: LegacyWinnersQ examined %.1f membership rows per person over %d people at %d units, %.1f over %d at the size before",
						sh.name, c.examinedPerPerson, c.people, n, p.examinedPerPerson, p.people)
				}
				if c.buffersPerPerson > 1.5*p.buffersPerPerson+16 {
					t.Errorf("%s: LegacyWinnersQ touched %.1f shared blocks per person over %d people at %d units, %.1f over %d at the size before",
						sh.name, c.buffersPerPerson, c.people, n, p.buffersPerPerson, p.people)
				}
			}
			prev[sh.name] = c
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		// The next size must not read this size's rolled-back rows as dead tuples.
		if _, err := pool.Exec(ctx, `VACUUM governance_memberships, governance_units`); err != nil {
			t.Fatal(err)
		}
	}
}
