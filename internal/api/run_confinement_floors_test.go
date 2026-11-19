// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// TestConfinementTraitsFireOnTheirSignal: each trait the confinement floors read is true on its own signal and
// false without it.
func TestConfinementTraitsFireOnTheirSignal(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(f *runFold)
		want composer.ConfinementTraits
	}{
		{"no trait", func(*runFold) {}, composer.ConfinementTraits{}},
		{"the request's image", func(f *runFold) { f.req.Image = "registry.example/team/tool:1" }, composer.ConfinementTraits{OwnImage: true}},
		{"a policy-carried workspace's image", func(f *runFold) { f.carriedImage = "registry.example/team/base:2" }, composer.ConfinementTraits{OwnImage: true}},
		{"a devcontainer build", func(f *runFold) { f.req.DevcontainerRepo = "https://git.example/team/repo" }, composer.ConfinementTraits{OwnImage: true}},
		{"a workspace built from its repository's own devcontainer", func(f *runFold) { f.wsRefs = []types.Workspace{devcontainerRepoWorkspace(t, true)} }, composer.ConfinementTraits{OwnImage: true}},
		{"a workspace whose image Wardyn generates", func(f *runFold) { f.wsRefs = []types.Workspace{devcontainerRepoWorkspace(t, false)} }, composer.ConfinementTraits{}},
		{"a devcontainer workspace beside one naming a base image", func(f *runFold) {
			// The base image decides the image (resolveCreateRunImage), and the fold carries it as carriedImage.
			based := devcontainerRepoWorkspace(t, false)
			based.BaseImage = &types.WorkspaceBaseImage{Kind: "registry", Image: "registry.example/team/base:2"}
			f.wsRefs = []types.Workspace{devcontainerRepoWorkspace(t, true), based}
		}, composer.ConfinementTraits{}},
		{"a component the launcher defined", func(f *runFold) { f.comps.selfDefined = 1 }, composer.ConfinementTraits{SelfDefinedComponent: true}},
		{"data_out unset", func(f *runFold) { f.spec.DataOut = "" }, composer.ConfinementTraits{DataOutOpen: true}},
		{"data_out allowed", func(f *runFold) { f.spec.DataOut = types.TransferAllowed }, composer.ConfinementTraits{DataOutOpen: true}},
		{"data_out review", func(f *runFold) { f.spec.DataOut = types.TransferReview }, composer.ConfinementTraits{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := runFold{req: &createRunRequest{}, spec: types.RunPolicySpec{DataOut: types.TransferBlocked}}
			tc.set(&f)
			if got := confinementTraits(*f.req, f.carriedImage, f.wsRefs, f.comps, f.spec); got != tc.want {
				t.Errorf("traits = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// devcontainerRepoWorkspace is a scanned workspace whose primary source is an HTTPS repository, carrying its own
// devcontainer when devcontainer is true.
func devcontainerRepoWorkspace(t *testing.T, devcontainer bool) types.Workspace {
	t.Helper()
	profile, err := json.Marshal(workspacescan.WorkspaceProfile{Languages: []string{"Go"}, HasDevcontainer: devcontainer})
	if err != nil {
		t.Fatal(err)
	}
	return types.Workspace{
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: "https://git.example/team/repo", Ref: "main"}},
		Profile: profile,
	}
}

// TestConfinementFloorHoldsTheRunToTheTraitFloor: an own_image floor of CC3 raises the floor spec over a CC1
// policy without changing the run's spec, so a requested CC2 is refused as weaker than the policy minimum and a
// defaulted class is CC3 though the runner offers only CC1 and CC2; with no floors the spec's class stands.
func TestConfinementFloorHoldsTheRunToTheTraitFloor(t *testing.T) {
	f := runFold{
		req:     &createRunRequest{Image: "registry.example/team/tool:1"},
		spec:    types.RunPolicySpec{MinConfinementClass: types.CC1, DataOut: types.TransferBlocked},
		ceiling: governanceCeiling{Limits: types.GovernanceLimits{ConfinementFloors: &types.ConfinementFloors{OwnImage: types.CC3}}},
	}
	floorOf := func() types.RunPolicySpec {
		return confinementFloor(*f.req, f.carriedImage, f.wsRefs, f.comps, f.spec, f.ceiling)
	}
	raisedBy := func() []string {
		return confinementRaisedBy(*f.req, f.carriedImage, f.wsRefs, f.comps, f.spec, f.ceiling)
	}
	floor := floorOf()
	if floor.MinConfinementClass != types.CC3 || f.spec.MinConfinementClass != types.CC1 {
		t.Fatalf("floor %q over spec %q; want CC3 over an unchanged CC1", floor.MinConfinementClass, f.spec.MinConfinementClass)
	}
	if _, _, err := enforcedConfinement(floor, types.CC2, nil, "", composer.Baseline{}); err == nil || !strings.Contains(err.Error(), "weaker than the policy minimum CC3") {
		t.Errorf("a requested CC2 under an own_image floor of CC3: err = %v, want the policy-minimum refusal", err)
	}
	if got, _, err := enforcedConfinement(floor, "", []types.ConfinementClass{types.CC1, types.CC2}, "", composer.Baseline{}); err != nil || got != types.CC3 {
		t.Errorf("a defaulted class = %q, %v; want CC3", got, err)
	}
	if by := raisedBy(); !slices.Equal(by, []string{"own_image"}) {
		t.Errorf("raised by %v, want [own_image]", by)
	}
	f.ceiling.Limits.ConfinementFloors = nil
	if got := floorOf().MinConfinementClass; got != types.CC1 || raisedBy() != nil {
		t.Errorf("with no floors the class is %q raised by %v, want the spec's CC1 raised by none", got, raisedBy())
	}
}

// TestConfinementFloorIsOneAtEveryDoor: only runFold.confinementFloor builds a floor spec, and stepPlacement and
// stepConfinement each read it once, in a top-level assignment rather than under a mode test. Both steps run at
// every door (wantRunFoldSteps in TestPreflightMirrorsLaunchGates), and the queued start replays the fold, so
// create, Review, the preview and the queue's start hold a run to the same raised floor.
func TestConfinementFloorIsOneAtEveryDoor(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	specCalls := map[string]int{}     // declaring function -> its calls of confinementFloorSpec
	floorCalls := map[string][]bool{} // declaring function -> for each confinementFloor call, in a top-level assignment
	foldArgs := map[string]string{}   // declaring function -> the arguments of its confinementFloor, traitFloor or confinementRaisedBy call
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			for _, stmt := range fn.Body.List {
				_, assign := stmt.(*ast.AssignStmt)
				ast.Inspect(stmt, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					id, ok := call.Fun.(*ast.Ident)
					if !ok {
						return true
					}
					switch id.Name {
					case "confinementFloorSpec":
						specCalls[fn.Name.Name]++
					case "confinementFloor":
						floorCalls[fn.Name.Name] = append(floorCalls[fn.Name.Name], assign)
					}
					if name != "run_confinement_floors.go" && slices.Contains([]string{"confinementFloor", "traitFloor", "confinementRaisedBy"}, id.Name) {
						var args []string
						for _, a := range call.Args {
							var b strings.Builder
							if err := printer.Fprint(&b, fset, a); err != nil {
								t.Fatal(err)
							}
							args = append(args, b.String())
						}
						foldArgs[fn.Name.Name] = strings.Join(args, ", ")
					}
					return true
				})
			}
		}
	}
	if want := map[string]int{"confinementFloor": 1}; !maps.Equal(specCalls, want) {
		t.Errorf("confinementFloorSpec is called by %v; want only by runFold.confinementFloor, so no floor skips the profile's confinement floors", specCalls)
	}
	for _, step := range []string{"stepPlacement", "stepConfinement"} {
		if got := floorCalls[step]; !slices.Equal(got, []bool{true}) {
			t.Errorf("%s reads confinementFloor %d times (in a top-level assignment: %v); want once, in a top-level assignment", step, len(got), got)
		}
	}
	// Every reader hands over the fold's own fields: its resolved policy, whose minimum no floor has raised, and
	// its own ceiling. A copy of the spec a door has since changed would name the wrong traits, or none. A waiting
	// launch reads its own request, q.req, which handleCreateRun copies from the request the fold read once the
	// fold is done: the same value, and a queuedCreate need not carry the fold's request pointer.
	const own = "*f.req, f.carriedImage, f.wsRefs, f.comps, f.spec, f.ceiling"
	want := map[string]string{"stepPlacement": own, "stepConfinement": own, "runCreateAuditData": own, "handlePreflightRun": own,
		"auditQueuedCreate": strings.Replace(strings.ReplaceAll(own, "f.", "q.f."), "*q.f.req", "q.req", 1)}
	if !maps.Equal(foldArgs, want) {
		t.Errorf("the confinement floor is read with %v; want %v", foldArgs, want)
	}
}

// TestStampConfinementRaisedBy: run.create carries confinement_raised_by only when a floor raised the class, and
// then exactly the traits that did.
func TestStampConfinementRaisedBy(t *testing.T) {
	data := map[string]any{}
	stampConfinementRaisedBy(data, nil)
	if _, ok := data["confinement_raised_by"]; ok {
		t.Errorf("a run no floor raised carries confinement_raised_by: %v", data)
	}
	stampConfinementRaisedBy(data, []string{"own_image", "data_out_open"})
	if got, _ := data["confinement_raised_by"].([]string); !slices.Equal(got, []string{"own_image", "data_out_open"}) {
		t.Errorf("confinement_raised_by = %v, want [own_image data_out_open]", data["confinement_raised_by"])
	}
}

// TestGovernanceLimitsRefusesAnUnknownConfinementFloor: a standalone profile's write refuses a floor that is not
// CC1, CC2 or CC3, naming it under limits.confinement_floors, and takes a valid one.
func TestGovernanceLimitsRefusesAnUnknownConfinementFloor(t *testing.T) {
	bad := governanceLimitsRefusal(types.GovernanceLimits{ConfinementFloors: &types.ConfinementFloors{DataOutOpen: "CC4"}})
	if !strings.HasPrefix(bad, "limits.confinement_floors.data_out_open: ") {
		t.Errorf("refusal = %q, want one naming limits.confinement_floors.data_out_open", bad)
	}
	if got := governanceLimitsRefusal(types.GovernanceLimits{ConfinementFloors: &types.ConfinementFloors{DataOutOpen: types.CC2}}); got != "" {
		t.Errorf("a valid floor is refused: %q", got)
	}
}

// TestConfinementRaisedWarning: Review's sentence names the raised class and every trait that raised it, and a
// run no floor raised gets none.
func TestConfinementRaisedWarning(t *testing.T) {
	if got := confinementRaisedWarning(types.CC3, nil); got != nil {
		t.Errorf("no trait raised: warnings = %q, want none", got)
	}
	got := confinementRaisedWarning(types.CC3, []string{"own_image", "data_out_open"})
	if len(got) != 1 || !strings.Contains(got[0], "CC3") || !strings.Contains(got[0], "own_image, data_out_open") {
		t.Errorf("warnings = %q, want one sentence naming CC3 and own_image, data_out_open", got)
	}
}
