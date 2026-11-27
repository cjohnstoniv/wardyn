// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"slices"
	"testing"
)

// localEligibilityDoors is every non-test call of placement.LocalEligibility, as "file function": the doors
// that judge a plan. Each says whether its plan carries runners.machine_kind_denials (a new run is judged
// against them) or not (a revive keeps its runner). A new door reds the census until it is listed here, so
// whoever adds it decides which it is.
var localEligibilityDoors = map[string]string{
	"run_placement_eligibility.go localPlacementRefusal":   "new run: the plan from localAdmittedPlan carries the denials",
	"runner_trust.go judgeAtTrust":                         "new run: judges the caller's plan, which carries them",
	"run_placement_spec.go classifyLocalDispatch":          "new run: sets them after localResolvedPlan",
	"run_placement_revive.go refreshLocalDeploymentConfig": "revive: none; a revive keeps its runner",
}

// localEligibilityCallsIn is each function that calls placement.LocalEligibility, as "file function".
func localEligibilityCallsIn(files map[string]*ast.File) []string {
	seen := map[string]bool{}
	for name, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "LocalEligibility" {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "placement" {
					seen[name+" "+fn.Name.Name] = true
				}
				return true
			})
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// P9: every caller of placement.LocalEligibility is classified for the machine-kind denials.
func TestLocalEligibilityDoors_ClassifiedForMachineKindDenials(t *testing.T) {
	_, files := parsePackageSources(t, ".")
	got, want := localEligibilityCallsIn(files), slices.Sorted(maps.Keys(localEligibilityDoors))
	if !slices.Equal(got, want) {
		t.Errorf("placement.LocalEligibility callers = %v, want localEligibilityDoors %v: classify a new door as judging new runs or reviving", got, want)
	}
}

// A fifth call site reds the census.
func TestLocalEligibilityDoorsCensusFlagsAPlantedTwin(t *testing.T) {
	_, files := parsePackageSources(t, ".")
	f, err := parser.ParseFile(token.NewFileSet(), "planted.go", `package api
func (s *Server) destinationPlanTwin() { placement.LocalEligibility(placement.LocalPlan{}) }`, 0)
	if err != nil {
		t.Fatal(err)
	}
	files["planted.go"] = f
	got := localEligibilityCallsIn(files)
	if !slices.Contains(got, "planted.go destinationPlanTwin") || slices.Equal(got, slices.Sorted(maps.Keys(localEligibilityDoors))) {
		t.Errorf("a planted LocalEligibility door went unseen: %v", got)
	}
}
