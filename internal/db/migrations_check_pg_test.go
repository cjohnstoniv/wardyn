// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// THE CLOSED-ENUM PARITY GUARD, ASKED OF POSTGRES ITSELF.
//
// TestClosedEnumChecksMatchConstants reads the MIGRATION TEXT and models it. That
// model was once blind to a disjunct outside the IN-list — a value the database
// admitted and the guard reported clean, which is the finding this file closes
// the other half of. The text parser was fixed to read the whole expression and
// to REFUSE a clause it cannot model, which is the right discipline; it is still
// a model. `pg_get_constraintdef` is not: it is the constraint the server will
// actually enforce, after every migration in the tree has run, normalised by
// Postgres rather than by a regexp of ours.
//
// The two guards share one case table (closedEnumChecks) and one comparison
// (compareClosedEnum), so they cannot come to different ideas of what parity
// means — and this one runs against the LIVE lane, so a constraint that was
// hand-altered, dropped, or written in a form the text parser reads differently
// from the server shows up here.
//
// IT CARRIES ITS OWN COUNTERFACTUAL. A parity guard that cannot fail is the
// original defect wearing a different hat, so the last subtest widens a real
// constraint in the throwaway schema — the way a hand-applied ALTER would, with
// no migration to show for it — and asserts this guard reports it.

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// liveCheckDef returns the ONE CHECK constraint definition the live database
// holds for table.column, as Postgres renders it.
//
// Exactly one, and a second is a failure rather than a merge: two CHECKs on one
// column are ANDed, so the admitted set is their intersection and no set of
// literals describes it. Refusing beats reporting a set this guard cannot stand
// behind — the same rule the text parser's checkGlueRe follows.
func liveCheckDef(t *testing.T, table, column string) string {
	t.Helper()
	rows, err := pgPool(t).Query(context.Background(), `
		SELECT pg_get_constraintdef(con.oid)
		FROM pg_constraint con
		JOIN pg_class rel ON rel.oid = con.conrelid
		JOIN pg_namespace n ON n.oid = rel.relnamespace
		JOIN pg_attribute a ON a.attrelid = rel.oid AND a.attnum = ANY (con.conkey)
		WHERE con.contype = 'c' AND rel.relname = $1 AND a.attname = $2
		  AND n.nspname = ANY (current_schemas(false))`, table, column)
	if err != nil {
		t.Fatalf("read the live CHECK for %s.%s: %v", table, column, err)
	}
	defer rows.Close()
	var defs []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			t.Fatalf("scan constraintdef: %v", err)
		}
		defs = append(defs, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read constraintdefs: %v", err)
	}
	switch len(defs) {
	case 1:
		return defs[0]
	case 0:
		t.Fatalf("the live database holds NO CHECK on %s.%s, while the migrations define one — the column accepts "+
			"anything the Go side does not, and no amount of write-boundary validation is what this test is about", table, column)
	default:
		t.Fatalf("the live database holds %d CHECKs on %s.%s (%v). They are ANDed, so the admitted set is their "+
			"intersection and a list of literals cannot describe it — model it here rather than letting this guard "+
			"report a set it cannot stand behind", len(defs), table, column, defs)
	}
	return ""
}

var (
	// pgCastRe strips the type annotations Postgres renders on every literal
	// and array ('once'::text, ARRAY[...]::text[]).
	pgCastRe = regexp.MustCompile(`::\s*[a-zA-Z_][a-zA-Z0-9_ ]*(?:\[\])?`)
	// pgCheckGlueRe is everything a normalised CHECK may be left holding once
	// its literals, casts and the column name are removed: the CHECK keyword
	// pg_get_constraintdef prefixes, the ANY/ARRAY form it renders an IN-list
	// as, OR, and punctuation. Anything else — a second column, a function call,
	// a <>, a range test — narrows or widens what the server accepts in a way a
	// set of literals cannot express, and is refused rather than guessed at.
	pgCheckGlueRe = regexp.MustCompile(`(?is)^[\s()\[\],=]*(?:(?:CHECK|ANY|ALL|ARRAY|OR)[\s()\[\],=]*)*$`)
)

// parseLiveCheckValues returns every value a rendered CHECK definition admits
// for column, or an error naming the clause it could not model.
//
// It is a PURE function of the constraintdef text, deliberately — the same shape
// columnCheckValues has on the migration-text side. The database is what
// PRODUCES the definition; modelling it is arithmetic, and arithmetic that only
// a provisioned Postgres lane can execute is arithmetic nobody checks on the
// lane that always runs. TestClosedEnumLiveCheckParserModelsTheWholeConstraintdef
// drives this function with no database at all.
//
// It reads the WHOLE definition, not the ANY/ARRAY list: the defect this file
// closes was a guard that saw only a list and never the empty-string equality
// disjunct hung off the end of it, so a value the server admitted read as clean.
func parseLiveCheckValues(def, column string) (map[string]bool, error) {
	vals := map[string]bool{}
	for _, q := range quotedAnyRe.FindAllStringSubmatch(def, -1) {
		vals[q[1]] = true
	}
	rest := quotedAnyRe.ReplaceAllString(def, " ")
	rest = pgCastRe.ReplaceAllString(rest, " ")
	rest = regexp.MustCompile(`(?i)\b`+regexp.QuoteMeta(column)+`\b`).ReplaceAllString(rest, " ")
	if !pgCheckGlueRe.MatchString(rest) {
		return nil, fmt.Errorf("the live CHECK %q holds a clause this guard cannot model (%q). Model it here or "+
			"simplify the constraint — a parity guard that silently reads only part of what the server enforces "+
			"reports the database and the Go set as agreeing when they do not", def, strings.TrimSpace(rest))
	}
	return vals, nil
}

// liveCheckValues is parseLiveCheckValues bound to a test: an unmodellable
// clause fails the guard rather than being quietly dropped from the set.
func liveCheckValues(t *testing.T, def, column string) map[string]bool {
	t.Helper()
	vals, err := parseLiveCheckValues(def, column)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return vals
}

// TestClosedEnumLiveCheckParserModelsTheWholeConstraintdef pins the live-catalog
// guard's MODEL, driven with no database at all.
//
// TestPG_ClosedEnumChecksMatchTheLiveCatalog cannot run on a lane without
// Postgres — and a lane without Postgres is exactly where `--- SKIP` -> `ok` ->
// exit 0 would hide a parser that reads only half a constraint, which is the
// shape of the defect this file closes. So everything about the guard that is
// not the round trip to the server (the parse, and the comparison it feeds) is
// pinned here, on the lane that always runs, against the exact text
// pg_get_constraintdef renders for 0039's constraint.
func TestClosedEnumLiveCheckParserModelsTheWholeConstraintdef(t *testing.T) {
	const column = "decision_scope"
	// Verbatim pg_get_constraintdef output for approvals_decision_scope_check,
	// the constraint whose OR-disjunct made the migration-text guard blind.
	const shipped = `CHECK (((decision_scope = ANY (ARRAY['once'::text, 'run'::text, 'until'::text, 'always'::text])) OR (decision_scope = ''::text)))`
	// The same constraint after a hand-applied ALTER: drift with no migration to
	// show for it, which the text guard cannot see by construction.
	const widened = `CHECK (((decision_scope = ANY (ARRAY['once'::text, 'run'::text, 'until'::text, 'always'::text])) OR (decision_scope = ''::text) OR (decision_scope = 'ZZZSNEAK'::text)))`

	t.Run("the shipped constraint parses to exactly the Go set", func(t *testing.T) {
		got, err := parseLiveCheckValues(shipped, column)
		if err != nil {
			t.Fatalf("parseLiveCheckValues(shipped): %v", err)
		}
		// '' lives OUTSIDE the ARRAY list in 0039 and is a real state of the
		// column, so seeing it is the parse's whole-expression property.
		if !got[""] {
			t.Errorf("the parser did not see the '' disjunct outside the ARRAY list: %v", got)
		}
		var rec recordingErrorf
		compareClosedEnumTo(&rec, closedEnumCheckFor(t, "approvals", column), got, "the shipped live CHECK")
		if rec.n != 0 {
			t.Errorf("the shipped constraint and the Go set disagree in %d place(s); parsed %v", rec.n, got)
		}
	})

	t.Run("a value hand-added outside the ARRAY list is seen and rejected", func(t *testing.T) {
		got, err := parseLiveCheckValues(widened, column)
		if err != nil {
			t.Fatalf("parseLiveCheckValues(widened): %v", err)
		}
		if !got["ZZZSNEAK"] {
			t.Fatalf("the parser read only part of %q: %v — a widened live constraint would be reported clean, "+
				"which is the finding this guard exists to close", widened, got)
		}
		var rec recordingErrorf
		compareClosedEnumTo(&rec, closedEnumCheckFor(t, "approvals", column), got, "the widened live CHECK")
		if rec.n == 0 {
			t.Fatalf("compareClosedEnumTo accepted %q as a defined Go constant; the parity guard cannot fail and "+
				"is therefore proving nothing", "ZZZSNEAK")
		}
	})

	t.Run("a clause the parser cannot model is refused, not half-read", func(t *testing.T) {
		const unmodellable = `CHECK (((decision_scope = ANY (ARRAY['once'::text])) OR (length(decision_scope) < 3)))`
		got, err := parseLiveCheckValues(unmodellable, column)
		if err == nil {
			t.Fatalf("parseLiveCheckValues accepted %q and returned %v; no list of literals describes that "+
				"constraint, and returning one asserts a parity the server does not enforce", unmodellable, got)
		}
	})
}

func TestPG_ClosedEnumChecksMatchTheLiveCatalog(t *testing.T) {
	for _, c := range closedEnumChecks() {
		t.Run(c.table+"."+c.column, func(t *testing.T) {
			def := liveCheckDef(t, c.table, c.column)
			compareClosedEnum(t, c, liveCheckValues(t, def, c.column), "the LIVE CHECK "+def)
		})
	}

	// THE COUNTERFACTUAL, in a throwaway schema, against a constraint widened
	// the way drift actually arrives: a hand-applied ALTER with no migration to
	// show for it, which the migration-text guard cannot see by construction.
	t.Run("a value hand-added to the live CHECK is caught", func(t *testing.T) {
		pool, schema := probeSchemaPool(t)
		ctx := context.Background()
		const (
			table  = "approvals"
			column = "decision_scope"
			sneak  = "ZZZSNEAK"
		)
		var name string
		if err := pool.QueryRow(ctx, `
			SELECT con.conname
			FROM pg_constraint con
			JOIN pg_class rel ON rel.oid = con.conrelid
			JOIN pg_namespace n ON n.oid = rel.relnamespace
			JOIN pg_attribute a ON a.attrelid = rel.oid AND a.attnum = ANY (con.conkey)
			WHERE con.contype = 'c' AND rel.relname = $1 AND a.attname = $2 AND n.nspname = $3`,
			table, column, schema).Scan(&name); err != nil {
			t.Fatalf("find the %s.%s constraint in %s: %v", table, column, schema, err)
		}
		if _, err := pool.Exec(ctx, fmt.Sprintf(
			`ALTER TABLE %s.%s DROP CONSTRAINT %s, ADD CONSTRAINT %s CHECK (%s IN ('once','run','until','always') OR %s = '' OR %s = '%s')`,
			schema, table, name, name, column, column, column, sneak)); err != nil {
			t.Fatalf("widen the live constraint in %s: %v", schema, err)
		}

		var def string
		if err := pool.QueryRow(ctx, `
			SELECT pg_get_constraintdef(con.oid)
			FROM pg_constraint con JOIN pg_class rel ON rel.oid = con.conrelid
			JOIN pg_namespace n ON n.oid = rel.relnamespace
			WHERE con.contype = 'c' AND rel.relname = $1 AND n.nspname = $2 AND con.conname = $3`,
			table, schema, name).Scan(&def); err != nil {
			t.Fatalf("re-read the widened constraintdef: %v", err)
		}
		if !strings.Contains(def, sneak) {
			t.Fatalf("the widened constraint does not carry %q (%s); the counterfactual did not take", sneak, def)
		}
		got := liveCheckValues(t, def, column)
		if !got[sneak] {
			t.Fatalf("liveCheckValues(%q) = %v, and it did not see %q — this guard would report a widened live "+
				"constraint as clean, which is the finding it exists to close", def, got, sneak)
		}
		// And the comparison itself must reject it. Run against a recording T so
		// the counterfactual proves the failure without failing this test.
		var rec recordingErrorf
		compareClosedEnumTo(&rec, closedEnumCheckFor(t, table, column), got, "counterfactual")
		if rec.n == 0 {
			t.Fatalf("compareClosedEnum accepted %q as a defined Go constant; the parity guard cannot fail and is "+
				"therefore proving nothing", sneak)
		}
	})
}

// closedEnumCheckFor returns the case table's entry for one column.
func closedEnumCheckFor(t *testing.T, table, column string) closedEnumCheck {
	t.Helper()
	for _, c := range closedEnumChecks() {
		if c.table == table && c.column == column {
			return c
		}
	}
	t.Fatalf("no closedEnumChecks entry for %s.%s", table, column)
	return closedEnumCheck{}
}

// recordingErrorf counts the failures compareClosedEnumTo would have reported,
// so the counterfactual above can assert that the guard DOES fail without
// failing the test that is proving it.
type recordingErrorf struct{ n int }

func (r *recordingErrorf) Errorf(string, ...any) { r.n++ }
func (r *recordingErrorf) Helper()               {}
