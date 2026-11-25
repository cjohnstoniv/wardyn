// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// uiSpecSite classifies one function that calls a validator reaching validateUIApps. ruledBy names the
// functions that must call a UI-app caller ruling (ruleUIAppCaller*, ruleUnitUIAppCallers) for a spec that
// site validates; exempt gives the reason none is needed.
type uiSpecSite struct {
	ruledBy []string
	exempt  string
}

// uiSpecSites is the table of validation sites. A site that is not here, or that is here and
// no longer calls a validator reaching validateUIApps, fails the guard, so a new or moved site is classified.
var uiSpecSites = map[string]uiSpecSite{
	"decodePolicyRequest":         {ruledBy: []string{"handleCreatePolicy", "handleUpdatePolicy"}},
	"buildGovernanceUnit":         {ruledBy: []string{"writeGovernanceUnit"}},
	"checkOverlay":                {ruledBy: []string{"writeGovernanceUnit"}},
	"finishGovernanceUnitRequest": {ruledBy: []string{"writeGovernanceUnit"}},
	"handlePutPreset":             {ruledBy: []string{"validatePresetRequest"}},
	"inlinePolicyRefusal":         {ruledBy: []string{"inlinePolicyRefusal"}},
	"applyPolicyOverlay":          {ruledBy: []string{"applyPolicyOverlay"}},
	"LoadPolicySpec":              {exempt: "the policy file is a configured source, read by the census, not a stored spec"},
	"applyRestrictionChange":      {exempt: "a RestrictionSpec has no ui_apps field"},
	"restrictionReviewFor":        {exempt: "a RestrictionSpec has no ui_apps field"},
	"writeRestriction":            {exempt: "a RestrictionSpec has no ui_apps field"},
	"boundaryReplayRefusal":       {exempt: "a BoundarySpec has no ui_apps field"},
	"handleBoundaryDryRun":        {exempt: "a BoundarySpec has no ui_apps field"},
	"handlePutBoundary":           {exempt: "a BoundarySpec has no ui_apps field"},
	"handleSynthesizeProfile":     {exempt: "answers the caller and stores nothing"},
	"handleGradePolicy":           {exempt: "answers the caller and stores nothing"},
}

// uiSpecCallGraph is the function-name call graph of the non-test files in dir (a method and a function of one
// name share a node, which only widens what a guard flags).
func uiSpecCallGraph(t *testing.T, dir string) map[string]map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	calls := map[string]map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			c := calls[fd.Name.Name]
			if c == nil {
				c = map[string]bool{}
				calls[fd.Name.Name] = c
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if ce, ok := n.(*ast.CallExpr); ok {
					switch fn := ce.Fun.(type) {
					case *ast.Ident:
						c[fn.Name] = true
					case *ast.SelectorExpr:
						c[fn.Sel.Name] = true
					}
				}
				return true
			})
		}
	}
	return calls
}

// Every function that calls a validate* function reaching validateUIApps is classified in the table: ruled by a
// named function that calls a UI-app caller ruling, or exempt with a reason.
func TestPortalSubjectRuling_EveryUISpecValidationRuled(t *testing.T) {
	calls := uiSpecCallGraph(t, ".")
	reach := map[string]bool{"validateUIApps": true}
	for changed := true; changed; {
		changed = false
		for fn, c := range calls {
			if reach[fn] || !strings.HasPrefix(fn, "validate") {
				continue
			}
			for callee := range c {
				if reach[callee] {
					reach[fn], changed = true, true
					break
				}
			}
		}
	}
	found := map[string]bool{}
	for fn, c := range calls {
		if reach[fn] {
			continue
		}
		for callee := range c {
			if reach[callee] {
				found[fn] = true
			}
		}
	}
	var names []string
	for fn := range found {
		names = append(names, fn)
	}
	sort.Strings(names)
	for _, fn := range names {
		if _, ok := uiSpecSites[fn]; !ok {
			t.Errorf("%s calls a validator that reaches validateUIApps and is not in the table: rule its callers (ruleUIAppCallers) or exempt it with a reason", fn)
		}
	}
	for fn, site := range uiSpecSites {
		if !found[fn] {
			t.Errorf("uiSpecSites names %s, which no longer calls a validator that reaches validateUIApps", fn)
		}
		if (len(site.ruledBy) == 0) == (site.exempt == "") {
			t.Errorf("%s: give exactly one of ruledBy and exempt", fn)
		}
		for _, holder := range site.ruledBy {
			ok := false
			for callee := range calls[holder] {
				if strings.HasPrefix(callee, "ruleUIAppCaller") || callee == "ruleUnitUIAppCallers" {
					ok = true
				}
			}
			if !ok {
				t.Errorf("%s validates a spec that is stored or starts a run, and %s must call a UI-app caller ruling beside it, and does not", fn, holder)
			}
		}
	}
	if !slices.Contains(names, "decodePolicyRequest") {
		t.Error("the guard found no site at decodePolicyRequest: its call graph is broken")
	}
}
