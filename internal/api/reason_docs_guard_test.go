// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// adoEntraFailureEnumValues is ADOEntraFailure's own closed enum
// (internal/api/ado_entra_store.go), carried through unchanged when the Azure
// DevOps redemption classifies a renewal failure — docs/sdk.md documents it as
// this "carried through unchanged" exception in its own words, and this guard
// hard-codes the same five values so a real gap in reasons.go's coverage
// cannot hide behind that exception.
var adoEntraFailureEnumValues = []string{
	"not_captured", "dead_credential", "consent_required", "interaction_required", "unavailable",
}

// documentedDuplicateReasonValues lists wire values TWO OR MORE reasons.go
// consts are deliberately declared to share (#656 slice 2 review round F4):
// #656's own rule is "one reason per cause", so a shared value is only
// legitimate when it is the SAME cause reached two ways — never a name
// added here to silence the guard. Empty today: no such pair exists yet.
var documentedDuplicateReasonValues = map[string]bool{}

// TestReasonDocsMatchReasonsGo (#656 M2) parses docs/sdk.md's Reason table and
// internal/api/reasons.go's const block and requires their sets of wire
// values to be equal, modulo ADOEntraFailure's own documented exception. This
// is the guard the FINAL review on PR #1299 asked for: without it, renaming a
// PUBLISHED wire value in reasons.go (a breaking change for any caller
// matching on it) passes every other test in the package.
func TestReasonDocsMatchReasonsGo(t *testing.T) {
	docBytes, err := os.ReadFile("../../docs/sdk.md")
	if err != nil {
		t.Fatalf("read docs/sdk.md: %v", err)
	}
	doc := string(docBytes)
	start := strings.Index(doc, "| Reason | Meaning |")
	if start < 0 {
		t.Fatal(`docs/sdk.md has no "| Reason | Meaning |" table header — this guard reads the Reason table by that anchor`)
	}
	end := strings.Index(doc[start:], "\n## ")
	if end < 0 {
		t.Fatal("could not find the end of the Reason table (next \"## \" heading)")
	}
	table := doc[start : start+end]

	code := regexp.MustCompile("`([a-z][a-z0-9_]*)`")
	docReasons := map[string]bool{}
	for _, line := range strings.Split(table, "\n") {
		if !strings.HasPrefix(line, "|") || strings.HasPrefix(line, "|---") || strings.HasPrefix(line, "| Reason") {
			continue
		}
		// The first pipe-delimited cell is the reason-code column; the second
		// (the meaning/prose) can itself contain backtick-quoted Go/route
		// identifiers that are NOT reason codes, so only the first cell counts.
		cells := strings.SplitN(line, "|", 3)
		if len(cells) < 2 {
			continue
		}
		for _, m := range code.FindAllStringSubmatch(cells[1], -1) {
			docReasons[m[1]] = true
		}
	}
	if len(docReasons) < 50 {
		t.Fatalf("parsed only %d reason(s) from docs/sdk.md — the table anchor or parser broke, not reasons.go", len(docReasons))
	}

	// Any string-literal const in this file counts, not only ones named
	// reasonXxx: driveRefusal*/driveUnavailable* (#656 slice 2 review round)
	// moved here BECAUSE this guard only reads this one file, and a naming
	// prefix is not what makes a value wire-visible. go/parser, not a regex
	// anchored on "^\s*name = ..." (#656 slice 3 review round): that regex
	// assumed every spec sat on its own line inside a `const ( ... )` block,
	// so a single-line `const x = "…"` — legal Go, invisible to a line-anchored
	// pattern that never expects the "const" keyword before the name — escaped
	// both this check and the duplicate-value one below. driveRefusalConstants
	// (user_drives_run_test.go) already parses this same file this way.
	goReasons, namesByValue := reasonsGoConstValues(t)
	if len(goReasons) < 50 {
		t.Fatalf("parsed only %d reason(s) from reasons.go — the parser broke, not the const block", len(goReasons))
	}

	// #656 slice 2 review round F4: no two consts may share a wire value
	// unless the pair is explicitly documented above — #656's own rule is
	// one reason per cause, so an undocumented shared value is either a
	// copy-paste accident or two causes silently collapsed into one.
	var undocumentedDuplicates []string
	for value, names := range namesByValue {
		if len(names) > 1 && !documentedDuplicateReasonValues[value] {
			sort.Strings(names)
			undocumentedDuplicates = append(undocumentedDuplicates, value+" "+strings.Join(names, "="))
		}
	}
	sort.Strings(undocumentedDuplicates)
	if len(undocumentedDuplicates) > 0 {
		t.Errorf("reasons.go declares the same wire value under more than one const name, with no "+
			"documentedDuplicateReasonValues entry: %v", undocumentedDuplicates)
	}

	// docReasons must equal goReasons ∪ the ADO enum exception, exactly.
	want := map[string]bool{}
	for k := range goReasons {
		want[k] = true
	}
	for _, v := range adoEntraFailureEnumValues {
		want[v] = true
	}

	var missingFromDocs, missingFromGo []string
	for k := range goReasons {
		if !docReasons[k] {
			missingFromDocs = append(missingFromDocs, k)
		}
	}
	for k := range docReasons {
		if !want[k] {
			missingFromGo = append(missingFromGo, k)
		}
	}
	sort.Strings(missingFromDocs)
	sort.Strings(missingFromGo)
	if len(missingFromDocs) > 0 {
		t.Errorf("reasons.go values with no docs/sdk.md row: %v", missingFromDocs)
	}
	if len(missingFromGo) > 0 {
		t.Errorf("docs/sdk.md values with no reasons.go const (and not the ADOEntraFailure exception): %v", missingFromGo)
	}
}

// reasonWireValueShape is a wire reason's own shape: lowercase snake_case,
// the same shape docs/sdk.md's table anchors on. reasons.go declares other
// string consts too (HTTP header names, capability kinds); this filters to
// the ones that look like a reason without needing every const to say so by
// name.
var reasonWireValueShape = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// reasonsGoFiles is the closed set's own two files (reasons.go's header
// comment explains the split: #656 slice 3 pushed reasons.go past
// scripts/check-file-size.sh's 1000-line gate). Every reader of the set —
// this guard, driveRefusalConstants (user_drives_run_test.go) — must read
// both, or a value declared in the second file is invisible to it.
var reasonsGoFiles = []string{"reasons.go", "reasons_routes.go"}

// reasonsGoConstValues parses every top-level const's string-literal value in
// reasonsGoFiles via go/parser rather than a line-anchored regex, so a
// single-line `const x = "…"` (legal Go outside a `const ( ... )` block) is
// seen exactly like one declared inside one. Returns the set of values, and
// every const name declared under each value (so the duplicate-value check
// above can name the collision).
func reasonsGoConstValues(t *testing.T) (values map[string]bool, namesByValue map[string][]string) {
	t.Helper()
	values, namesByValue = map[string]bool{}, map[string][]string{}
	for _, filename := range reasonsGoFiles {
		f, err := parser.ParseFile(token.NewFileSet(), filename, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", filename, err)
		}
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					value := strings.Trim(lit.Value, `"`)
					if !reasonWireValueShape.MatchString(value) {
						continue
					}
					values[value] = true
					namesByValue[value] = append(namesByValue[value], name.Name)
				}
			}
		}
	}
	return values, namesByValue
}
