// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"
)

// The census for the spec read tier. Every call that redacts a policy spec for a reader takes a
// specReadTier, and that tier must be the reader's (s.specReadTierFor, directly or through a variable or
// parameter that only ever holds it), or a constant this table names with the reason it is fixed. A
// site written later that passes a tier of its own choosing fails here.

// tierTakers is each tier-taking function and the index of its tier argument.
var tierTakers = map[string]int{
	"redactSpecForRead": 1, "redactPolicyForRead": 1, "redactPoliciesForRead": 1, "redactPresetOverlay": 1,
	"redactOverlayValue": 2, "redactOverlayForRead": 1, "newUnitView": 3, "newUnitDiffView": 2,
}

// fixedTiers are the sites whose tier is a constant, file:func:constant, each with its reason.
var fixedTiers = map[string]string{
	"policy_preview_facts.go:policyPreviewFacts:specReadMember":         "the preview is shown to the run's author, whatever their tier",
	"governance_change_kinds.go:newMembershipDiffView:specReadSecurity": "an assignment sets no host path, so a security admin decides it",
	"governance_change_kinds.go:holdUnitDelete:specReadSecurity":        "a delete sets no host path, so a security admin decides it",
	"governance_change_kinds.go:holdUnitWrite:specReadSecurity":         "a change is rendered for the lowest tier that may decide it",
	"governance_change_kinds.go:holdUnitWrite:specReadFull":             "a change that sets a host path is decided by super admins only",
	"run_policy_view.go:handleGetRunPolicy:specReadFull":                "the baseline Redacted compares the reader's view with",
}

// tierCensus is every tier-taking call in files whose tier is not the reader's or a listed constant.
func tierCensus(files []apiSource) []string {
	var bad []string
	for _, f := range files {
		for _, d := range f.file.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			params := map[string]bool{}
			for _, fl := range fd.Type.Params.List {
				if id, ok := fl.Type.(*ast.Ident); ok && id.Name == "specReadTier" {
					for _, n := range fl.Names {
						params[n.Name] = true
					}
				}
			}
			assigns := map[string][]ast.Expr{}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if as, ok := n.(*ast.AssignStmt); ok && len(as.Lhs) == len(as.Rhs) {
					for i, l := range as.Lhs {
						if id, ok := l.(*ast.Ident); ok {
							assigns[id.Name] = append(assigns[id.Name], as.Rhs[i])
						}
					}
				}
				return true
			})
			var readers func(e ast.Expr, seen map[string]bool) bool
			readers = func(e ast.Expr, seen map[string]bool) bool {
				switch x := e.(type) {
				case *ast.CallExpr:
					sel, ok := x.Fun.(*ast.SelectorExpr)
					return ok && sel.Sel.Name == "specReadTierFor"
				case *ast.Ident:
					if _, listed := fixedTiers[f.name+":"+fd.Name.Name+":"+x.Name]; listed {
						return true
					}
					if params[x.Name] {
						return true
					}
					if seen[x.Name] || len(assigns[x.Name]) == 0 {
						return false
					}
					seen[x.Name] = true
					for _, a := range assigns[x.Name] {
						if !readers(a, seen) {
							return false
						}
					}
					return true
				}
				return false
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				id, ok := call.Fun.(*ast.Ident)
				if !ok {
					return true
				}
				i, takes := tierTakers[id.Name]
				if !takes || i >= len(call.Args) {
					return true
				}
				if !readers(call.Args[i], map[string]bool{}) {
					bad = append(bad, f.name+":"+fd.Name.Name+": "+id.Name)
				}
				return true
			})
		}
	}
	slices.Sort(bad)
	return bad
}

func TestPolMount_CensusAST(t *testing.T) {
	files := parseAPISources(t)
	if bad := tierCensus(files); len(bad) > 0 {
		t.Errorf("CENSUS: these spec reads pass a tier that is not the reader's and not listed in fixedTiers: %v", bad)
	}
	// Every listed constant is still used: a stale row would admit a future site silently.
	for key := range fixedTiers {
		file, fn, _ := strings.Cut(key, ":")
		fn, constant, _ := strings.Cut(fn, ":")
		i := slices.IndexFunc(files, func(s apiSource) bool { return s.name == file })
		if i < 0 || !strings.Contains(funcSource(files[i].file, fn), constant) {
			t.Errorf("fixedTiers lists %s, which no longer exists", key)
		}
	}
}

// funcSource is the identifiers in fn, space-joined: enough to say whether it names a constant.
func funcSource(f *ast.File, fn string) string {
	var ids []string
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == fn {
			ast.Inspect(fd, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok {
					ids = append(ids, id.Name)
				}
				return true
			})
		}
	}
	return strings.Join(ids, " ")
}

// TestPolMount_CensusAST_PlantedViolation is the census's twin: a planted file with one site of each
// kind the census must refuse, and one it must accept.
func TestPolMount_CensusAST_PlantedViolation(t *testing.T) {
	const planted = `package api
func (s *Server) constant(spec types.RunPolicySpec) { _ = redactSpecForRead(spec, specReadFull) }
func (s *Server) chosen(r *http.Request, spec types.RunPolicySpec) {
	tier := specReadMember
	if s.isSecurityOperator(r.Context()) {
		tier = specReadFull
	}
	_ = newUnitView(types.GovernanceUnit{}, nil, nil, tier)
}
func (s *Server) mixed(r *http.Request, spec types.RunPolicySpec) {
	tier := s.specReadTierFor(r.Context())
	tier = specReadFull
	_ = redactSpecForRead(spec, tier)
}
func (s *Server) fine(r *http.Request, spec types.RunPolicySpec) {
	tier := s.specReadTierFor(r.Context())
	_ = redactSpecForRead(spec, tier)
	_ = redactPolicyForRead(types.RunPolicy{}, s.specReadTierFor(r.Context()))
}
func param(spec types.RunPolicySpec, tier specReadTier) { _ = redactSpecForRead(spec, tier) }
`
	f, err := parser.ParseFile(token.NewFileSet(), "planted.go", planted, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := tierCensus([]apiSource{{"planted.go", f}})
	want := []string{"planted.go:chosen: newUnitView", "planted.go:constant: redactSpecForRead", "planted.go:mixed: redactSpecForRead"}
	if !slices.Equal(got, want) {
		t.Errorf("the census reported %v, want %v", got, want)
	}
}
