// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var (
	scanComment  = regexp.MustCompile(`--[^\n]*`)
	scanCreate   = regexp.MustCompile(`(?is)create table (?:if not exists )?([a-z_0-9]+)\s*\(`)
	scanRename   = regexp.MustCompile(`(?is)alter table (?:if exists )?(?:only )?([a-z_0-9]+)\s+rename to ([a-z_0-9]+)`)
	scanAlter    = regexp.MustCompile(`(?is)alter table (?:if exists )?(?:only )?([a-z_0-9]+)\s+([^;]*);`)
	scanAddCol   = regexp.MustCompile(`(?i)add column (?:if not exists )?([a-z_0-9]+)\s+([A-Za-z\[\]_0-9]+)`)
	scanDropCol  = regexp.MustCompile(`(?i)drop column (?:if exists )?([a-z_0-9]+)`)
	scanColumn   = regexp.MustCompile(`^\s*([a-z_0-9]+)\s+([A-Za-z\[\]_0-9]+)`)
	personColumn = regexp.MustCompile(`^(principal|created_by|owner.*|owned_by|sponsor|holder|helper|person|sub|subject|decided_by|proposed_by|registered_by)$`)
	notAColumn   = map[string]bool{"PRIMARY": true, "UNIQUE": true, "CHECK": true, "FOREIGN": true, "CONSTRAINT": true, "EXCLUDE": true, "LIKE": true}
)

type schemaEvent struct {
	pos        int
	kind, a, b string
}

// migrationColumns replays the migration files in order and returns table -> column -> upper-case type name,
// following CREATE TABLE bodies, ADD COLUMN, DROP COLUMN and the two table renames.
func migrationColumns(t *testing.T) map[string]map[string]string {
	t.Helper()
	files, err := filepath.Glob("../db/migrations/[0-9]*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations found: %v", err)
	}
	sort.Strings(files)
	state := map[string]map[string]string{}
	table := func(name string) map[string]string {
		if state[name] == nil {
			state[name] = map[string]string{}
		}
		return state[name]
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		s := scanComment.ReplaceAllString(string(raw), "")
		var events []schemaEvent
		for _, m := range scanCreate.FindAllStringSubmatchIndex(s, -1) {
			i, depth := m[1], 1
			for depth > 0 && i < len(s) {
				switch s[i] {
				case '(':
					depth++
				case ')':
					depth--
				}
				i++
			}
			events = append(events, schemaEvent{m[0], "create", s[m[2]:m[3]], s[m[1] : i-1]})
		}
		for _, m := range scanRename.FindAllStringSubmatchIndex(s, -1) {
			events = append(events, schemaEvent{m[0], "rename", s[m[2]:m[3]], s[m[4]:m[5]]})
		}
		for _, m := range scanAlter.FindAllStringSubmatchIndex(s, -1) {
			events = append(events, schemaEvent{m[0], "alter", s[m[2]:m[3]], s[m[4]:m[5]]})
		}
		sort.SliceStable(events, func(i, j int) bool { return events[i].pos < events[j].pos })
		for _, e := range events {
			switch e.kind {
			case "create":
				cols := table(e.a)
				depth, cur := 0, strings.Builder{}
				var parts []string
				for _, ch := range e.b {
					switch ch {
					case '(':
						depth++
					case ')':
						depth--
					}
					if ch == ',' && depth == 0 {
						parts = append(parts, cur.String())
						cur.Reset()
						continue
					}
					cur.WriteRune(ch)
				}
				parts = append(parts, cur.String())
				for _, p := range parts {
					if m := scanColumn.FindStringSubmatch(p); m != nil && !notAColumn[strings.ToUpper(m[1])] {
						cols[m[1]] = strings.ToUpper(m[2])
					}
				}
			case "rename":
				if cols, ok := state[e.a]; ok {
					delete(state, e.a)
					state[e.b] = cols
				}
			case "alter":
				cols := table(e.a)
				for _, m := range scanAddCol.FindAllStringSubmatch(e.b, -1) {
					cols[m[1]] = strings.ToUpper(m[2])
				}
				for _, m := range scanDropCol.FindAllStringSubmatch(e.b, -1) {
					delete(cols, m[1])
				}
			}
		}
	}
	return state
}

// Every JSONB column, and every TEXT column named like a person, in the migrations is either read by the
// census or exempt with a reason; every entry of either list is a column the migrations still define, with
// the kind the census reads it as. So a migration that adds a column which can hold a person's principal
// fails here until the census or the exemption list names it.
func TestNamespaceCensus_MigrationScanGuard(t *testing.T) {
	schema := migrationColumns(t)
	scanned := map[string]string{} // "table.column" -> "json" | "text"
	for tbl, cols := range schema {
		for col, typ := range cols {
			switch {
			case typ == "JSONB":
				scanned[tbl+"."+col] = "json"
			case typ == "TEXT" && personColumn.MatchString(col):
				scanned[tbl+"."+col] = "text"
			}
		}
	}
	if len(scanned) < 100 {
		t.Fatalf("the scan found only %d candidate columns; the migration reader is broken", len(scanned))
	}

	listed := map[string]string{}
	for _, c := range namespaceCensusColumns {
		name := c.Table + "." + c.Column
		kind := "text"
		if c.JSON {
			kind = "json"
		}
		if _, dup := listed[name]; dup {
			t.Errorf("%s is listed twice in namespaceCensusColumns", name)
		}
		listed[name] = kind
	}
	for name, reason := range namespaceCensusExempt {
		if _, both := listed[name]; both {
			t.Errorf("%s is both read by the census and exempt", name)
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("the exemption of %s has no reason", name)
		}
	}
	var missing, stale, wrongKind []string
	for name, kind := range scanned {
		_, read := listed[name]
		_, exempt := namespaceCensusExempt[name]
		if !read && !exempt {
			missing = append(missing, name)
		}
		if read && listed[name] != kind {
			wrongKind = append(wrongKind, name)
		}
	}
	for name := range listed {
		if _, ok := scanned[name]; !ok {
			stale = append(stale, name)
		}
	}
	for name := range namespaceCensusExempt {
		if _, ok := scanned[name]; !ok {
			stale = append(stale, name)
		}
	}
	for _, l := range [][]string{missing, stale, wrongKind} {
		sort.Strings(l)
	}
	if len(missing) > 0 {
		t.Errorf("columns no census entry or exemption names: %v", missing)
	}
	if len(stale) > 0 {
		t.Errorf("census or exemption entries for columns the migrations do not define as such: %v", stale)
	}
	if len(wrongKind) > 0 {
		t.Errorf("census entries whose JSON flag disagrees with the column's type: %v", wrongKind)
	}
}
