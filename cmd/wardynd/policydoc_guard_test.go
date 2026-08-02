// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Policy-doc parity guard, the RunPolicySpec twin of envdoc_guard_test.go. It
// lives beside that guard (and not in internal/types) purely to reuse repoRoot.
//
// policyDocRow matches one FIELD row: the json name as the first cell of a
// four-column row. Anchoring is the whole point: policy json names are bare
// snake_case (`resources`, `mode`, `source`, `target`), so ENV.md's
// strings.Contains check — safe only because WARDYN_* is a collision-free
// namespace — would go green off incidental prose here. That is the false-green
// class v0.4.3 removed; do not reintroduce it.
//
// The column count is what separates a field row from the doc's VALUE tables
// (the three first_use_approval modes, the five grant kinds), whose first cell
// is also a backticked identifier but which are two and three columns wide. A
// new table shaped like a field table will red this guard — that is the loud
// failure, not a silent pass.
var policyDocRow = regexp.MustCompile("(?m)^\\| `([a-z0-9_]+)` \\|[^|\n]*\\|[^|\n]*\\|[^|\n]*\\|[ \t]*$")

// policyJSONNames collects every json tag reachable from t, recursing through
// pointers, slices and nested structs. The nested spec types (LLMInspectionSpec
// alone has 15 fields) are two thirds of the documented surface, so a
// top-level-only walk would police less than a third of the doc.
func policyJSONNames(t reflect.Type, out, seen map[string]bool) {
	for t.Kind() == reflect.Ptr || t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || seen[t.String()] {
		return
	}
	seen[t.String()] = true
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		out[name] = true
		policyJSONNames(f.Type, out, seen)
	}
}

func policyDocNames(t *testing.T) (fields map[string]bool, rows map[string]bool) {
	t.Helper()
	doc, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "POLICIES.md"))
	if err != nil {
		t.Fatalf("read docs/POLICIES.md: %v", err)
	}
	fields, seen := map[string]bool{}, map[string]bool{}
	policyJSONNames(reflect.TypeOf(types.RunPolicySpec{}), fields, seen)
	rows = map[string]bool{}
	for _, m := range policyDocRow.FindAllStringSubmatch(string(doc), -1) {
		rows[m[1]] = true
	}
	return fields, rows
}

// TestPolicyDoc_EveryFieldHasRow: adding a RunPolicySpec field (at any depth)
// without a docs/POLICIES.md row fails.
func TestPolicyDoc_EveryFieldHasRow(t *testing.T) {
	fields, rows := policyDocNames(t)
	for f := range fields {
		if !rows[f] {
			t.Errorf("%q is a RunPolicySpec json field with no docs/POLICIES.md row (add `| \x60%s\x60 | … |`)", f, f)
		}
	}
}

// TestPolicyDoc_EveryRowHasField: the other direction — a row for a field that
// no longer exists (renamed or deleted) fails, so the doc cannot rot.
func TestPolicyDoc_EveryRowHasField(t *testing.T) {
	fields, rows := policyDocNames(t)
	for r := range rows {
		if !fields[r] {
			t.Errorf("docs/POLICIES.md has a row for %q, which is not a json field reachable from RunPolicySpec — delete the stale row", r)
		}
	}
}
