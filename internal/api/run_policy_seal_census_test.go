// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

// The census guard for the final seal (run_policy_seal.go). It reads syntax only
// and fails when
//
//   - a late host addition appears, or one goes, that lateAddCensus does not
//     list (a union helper call, an append to an allowlist, an allowlist
//     assigned from a call, allow_all_egress set to anything but false or nil);
//   - a sealed path (dispatchRun, reviveRunProxy) does not call the seal once,
//     after its ceiling re-assertion, or after the seal adds a host itself or
//     hands the sealed value to anything that can reach a late addition (the
//     walk stops at a sealed path, which seals its own run, and at a detached
//     flow, which carries another run);
//   - a function that hands a policy to a proxy or a runner (proxySinks) is
//     reachable other than from after a sealed path's seal.
//
// Calls resolve by name, so a method sharing a widening function's name counts
// as one (the safe error), and a function passed as a value is not seen as a
// call: a path that wants that shape has to be listed by hand, which is the
// point of making it fail first.

// lateAddCensus is every late host addition in the package, as
// "file function" and how many it holds. A new one is only listed once it runs
// before the seal on every path that dispatches or revives a run.
var lateAddCensus = map[string]int{
	"components_run.go expand":                                  1,
	"integrations_run.go applyIntegrationRequirement":           1,
	"policy_preview.go unionPreviewWorkspaceEgress":             1,
	"provider_azure.go authorAzureInjection":                    1,
	"provider_subscription.go authorOAuthSentinelGrant":         1,
	"run_derived_egress.go deriveRunEgress":                     3,
	"run_overrides_apply.go applyHostOverrides":                 1,
	"run_overrides_apply.go applyRunEgressOverrides":            2,
	"run_overrides_apply.go applyRunHostOverrides":              1,
	"run_overrides_apply.go boundOverrideAdds":                  1,
	"run_policy_provenance.go canonicalKnobs":                   1,
	"runs_autonomy.go autonomyPostureSpec":                      2,
	"runs_autonomy.go unionADOEntraLane":                        1,
	"runs_cli_github.go confineBrokerEgress":                    1,
	"runs_create_requirements.go applyWorkspaceRequirementsFor": 1,
	"runs_dispatch.go dispatchRun":                              1,
	"runs_dispatch_ado_inject.go authorADOEntraInjection":       2,
	"runs_dispatch_cli_aws.go authorAWSCLI":                     1,
	"runs_dispatch_cli_azure.go authorAzureCLI":                 1,
	"runs_dispatch_llm.go applyBedrockTransport":                1,
	"runs_dispatch_provider.go resolveProviderLane":             1,
	"workspace_egress.go confinedEgressDomains":                 1,
	"workspace_egress.go unionSiteConfigScmHosts":               1,
	"workspace_egress.go unionWorkspaceEgress":                  3,
	"workspace_egress_github.go unionDirectGitHubEgress":        1,
}

// unionHelpers are the allowlist and denylist unions; their bodies, and the seal's own, are not sites.
var unionHelpers = []string{"unionAllowedDomains", "unionDeniedDomains", "unionDomains"}

// sealedPaths is each path that dispatches or revives a run: the ceiling
// re-assertion its seal must follow, and the variable holding what it seals.
var sealedPaths = map[string]struct{ after, sealed string }{
	"dispatchRun":    {"reassertCeilingDenies", "policy"},
	"reviveRunProxy": {"reassertProxyCeiling", "cfg"},
}

// proxySinks are the calls that hand a policy to a proxy or a runner.
var proxySinks = []string{"keepRunProxyConfig", "storeRunProxyConfig", "CreateSandbox"}

type lateAdd struct {
	file, fn string
	line     int
	pos      token.Pos
	union    bool
}

func mentionsField(e ast.Expr, field string) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == field {
			found = true
		}
		return !found
	})
	return found
}

// handsOver reports whether call is given ident, as an argument or as its receiver.
func handsOver(call *ast.CallExpr, ident string) bool {
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		found := false
		ast.Inspect(sel.X, func(n ast.Node) bool {
			found = found || isBare(asExpr(n), ident)
			return !found
		})
		if found {
			return true
		}
	}
	return callMentions(call, ident)
}

func asExpr(n ast.Node) ast.Expr {
	e, _ := n.(ast.Expr)
	return e
}

func isField(e ast.Expr, field string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == field
}

// lateAddsIn is every late host addition in f.
func lateAddsIn(fset *token.FileSet, name string, f *ast.File) []lateAdd {
	var out []lateAdd
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil || slices.Contains(unionHelpers, fn.Name.Name) || fn.Name.Name == "sealPolicy" {
			continue
		}
		at := func(n ast.Node, union bool) {
			out = append(out, lateAdd{name, fn.Name.Name, fset.Position(n.Pos()).Line, n.Pos(), union})
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				if id, ok := n.Fun.(*ast.Ident); ok && slices.Contains(unionHelpers, id.Name) {
					at(n, true)
				}
				if isBare(n.Fun, "append") && len(n.Args) > 0 && mentionsField(n.Args[0], "AllowedDomains") {
					at(n, false)
				}
			case *ast.AssignStmt:
				if len(n.Lhs) != len(n.Rhs) {
					return true
				}
				for i, lhs := range n.Lhs {
					call, isCall := n.Rhs[i].(*ast.CallExpr)
					if isField(lhs, "AllowedDomains") && isCall && !isBare(call.Fun, "append") &&
						!isQualified(call.Fun, "slices", "DeleteFunc") && !isQualified(call.Fun, "slices", "Clone") {
						at(n, false)
					}
					if isField(lhs, "AllowAllEgress") && !isBare(n.Rhs[i], "false") && !isBare(n.Rhs[i], "nil") {
						at(n, false)
					}
				}
			}
			return true
		})
	}
	return out
}

// sealCensus is the guard over a parsed package: the late additions it found
// and every violation, as "file:line: what".
type sealCensus struct {
	adds       []lateAdd
	violations []string
}

type callSite struct {
	file, fn string
	pos      token.Pos
}

func runSealCensus(fset *token.FileSet, files map[string]*ast.File) sealCensus {
	var c sealCensus
	fail := func(file string, pos token.Pos, format string, args ...any) {
		c.violations = append(c.violations, fmt.Sprintf("%s:%d: %s", file, fset.Position(pos).Line, fmt.Sprintf(format, args...)))
	}
	decls := map[string][]*ast.FuncDecl{}
	declFile := map[*ast.FuncDecl]string{}
	calls := map[string]map[string]bool{}
	callers := map[string][]callSite{}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		f := files[name]
		c.adds = append(c.adds, lateAddsIn(fset, name, f)...)
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			decls[fn.Name.Name] = append(decls[fn.Name.Name], fn)
			declFile[fn] = name
			if calls[fn.Name.Name] == nil {
				calls[fn.Name.Name] = map[string]bool{}
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					callers[calleeName(call)] = append(callers[calleeName(call)], callSite{name, fn.Name.Name, call.Pos()})
				}
				return true
			})
			// A detached flow (a go statement, goBackground) carries another run, not this policy.
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if _, ok := n.(*ast.GoStmt); ok {
					return false
				}
				if call, ok := n.(*ast.CallExpr); ok {
					if calleeName(call) == "goBackground" {
						return false
					}
					calls[fn.Name.Name][calleeName(call)] = true
				}
				return true
			})
		}
	}

	widens := map[string]bool{}
	for _, h := range unionHelpers {
		widens[h] = true
	}
	for _, a := range c.adds {
		if _, ok := sealedPaths[a.fn]; !ok { // a sealed path's own sites are placed by position, below
			widens[a.fn] = true
		}
	}
	// widenChain is the shortest call chain from name to a late addition, or nil.
	widenChain := func(name string) []string {
		from := map[string]string{name: ""}
		for queue := []string{name}; len(queue) > 0; queue = queue[1:] {
			at := queue[0]
			if widens[at] {
				var chain []string
				for ; at != ""; at = from[at] {
					chain = append([]string{at}, chain...)
				}
				return chain
			}
			for _, callee := range slices.Sorted(maps.Keys(calls[at])) {
				if _, sealed := sealedPaths[callee]; sealed {
					continue // it seals what it dispatches
				}
				if _, ok := from[callee]; !ok {
					from[callee] = at
					queue = append(queue, callee)
				}
			}
		}
		return nil
	}

	sealAt := map[string]token.Pos{}
	for _, path := range slices.Sorted(maps.Keys(sealedPaths)) {
		after, sealed := sealedPaths[path].after, sealedPaths[path].sealed
		if len(decls[path]) != 1 {
			c.violations = append(c.violations, fmt.Sprintf("sealed path %s: %d declarations, want 1", path, len(decls[path])))
			continue
		}
		fn := decls[path][0]
		file := declFile[fn]
		var seals, reasserts []token.Pos
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				switch calleeName(call) {
				case "sealRunPolicy":
					seals = append(seals, call.Pos())
				case after:
					reasserts = append(reasserts, call.Pos())
				}
			}
			return true
		})
		if len(seals) != 1 {
			fail(file, fn.Pos(), "%s calls sealRunPolicy %d times, want once", path, len(seals))
			continue
		}
		seal := seals[0]
		sealAt[path] = seal
		if len(reasserts) == 0 || slices.Max(reasserts) > seal {
			fail(file, seal, "%s seals before (or without) %s", path, after)
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			// Only a call handed the sealed value can change it.
			if call, ok := n.(*ast.CallExpr); ok && call.Pos() > seal && handsOver(call, sealed) {
				if chain := widenChain(calleeName(call)); chain != nil {
					fail(file, call.Pos(), "%s can add a host after the seal: %s", path, strings.Join(chain, " -> "))
				}
			}
			return true
		})
		for _, a := range c.adds {
			if a.fn == path && a.pos > seal {
				fail(a.file, a.pos, "%s adds a host after the seal", path)
			}
		}
	}

	// sealedBelow reports whether every call into name comes from after a sealed
	// path's seal, directly or through callers that do.
	var sealedBelow func(string, map[string]bool) bool
	sealedBelow = func(name string, seen map[string]bool) bool {
		if seen[name] {
			return true
		}
		seen[name] = true
		sites := callers[name]
		if len(sites) == 0 {
			return false
		}
		for _, cs := range sites {
			if seal, ok := sealAt[cs.fn]; ok {
				if cs.pos < seal {
					return false
				}
				continue
			}
			if !sealedBelow(cs.fn, seen) {
				return false
			}
		}
		return true
	}
	for _, sink := range proxySinks {
		for _, cs := range callers[sink] {
			if _, ok := sealAt[cs.fn]; ok && cs.pos > sealAt[cs.fn] {
				continue
			}
			if slices.Contains(proxySinks, cs.fn) {
				continue // a sink wrapping another; its own callers are checked as a sink
			}
			if !sealedBelow(cs.fn, map[string]bool{}) {
				fail(cs.file, cs.pos, "%s reaches %s on a path the final seal does not cover", cs.fn, sink)
			}
		}
	}
	return c
}

// plantedSources replaces or adds source files the census reads. It is empty in
// the tree: a revert proof fills it from a file go test -overlay adds, since the
// census reads the disk and an overlay only reaches the compiler.
var plantedSources = map[string]string{}

func parsePackageSources(t *testing.T, dir string) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if _, ok := plantedSources[name]; ok {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = f
	}
	for name, src := range plantedSources {
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = f
	}
	if len(files) == 0 {
		t.Fatal("parsed no source files")
	}
	return fset, files
}

func censusOf(adds []lateAdd) map[string]int {
	out := map[string]int{}
	for _, a := range adds {
		out[a.file+" "+a.fn]++
	}
	return out
}

// TestEveryLateHostAdditionIsSealed is the census as a test.
func TestEveryLateHostAdditionIsSealed(t *testing.T) {
	fset, files := parsePackageSources(t, ".")
	c := runSealCensus(fset, files)
	for _, v := range c.violations {
		t.Error(v)
	}
	got := censusOf(c.adds)
	if !maps.Equal(got, lateAddCensus) {
		var lines []string
		for _, a := range c.adds {
			lines = append(lines, fmt.Sprintf("%s:%d %s", a.file, a.line, a.fn))
		}
		t.Errorf("late host additions = %v, want lateAddCensus %v\nsites:\n%s", got, lateAddCensus, strings.Join(lines, "\n"))
	}
	// The union helpers are most of the census. Fewer seen means the guard went
	// blind to them, not that they went away.
	unions := 0
	for _, a := range c.adds {
		if a.union {
			unions++
		}
	}
	if unions < 21 {
		t.Errorf("the census sees %d union helper calls, want at least the 21 it was written against", unions)
	}
}

// TestSealCensusFiresOnAPlantedSite shows the guard is not satisfied by being
// silent: each planted package is one a late addition could be written as.
func TestSealCensusFiresOnAPlantedSite(t *testing.T) {
	const paths = `package api
func (s *Server) dispatchRun() { s.reassertCeilingDenies(); s.sealRunPolicy(); s.keepRunProxyConfig() }
func (s *Server) reviveRunProxy() { reassertProxyCeiling(); s.sealRunPolicy(); s.storeRunProxyConfig() }
`
	for _, tc := range []struct {
		name, src  string
		violations int
		adds       int
	}{
		{"sealed paths alone pass", ``, 0, 0},
		{"a late addition before the seal passes", `
func (s *Server) build(p *types.RunPolicySpec) { unionAllowedDomains(p, nil) }
func (s *Server) create() { s.build(nil); go s.dispatchRun() }`, 0, 1},
		{"a union on no sealed path's tail is a listed site only", `
func later(p *types.RunPolicySpec) { unionAllowedDomains(p, []string{"evil.example"}) }
func (s *Server) postSeal() { later(nil) }`, 0, 1},
		{"an append to the allowlist is a site", `
func f(p *types.RunPolicySpec) { p.AllowedDomains = append(p.AllowedDomains, "x") }`, 0, 1},
		{"an allowlist from a call is a site, a filter is not", `
func f(p *types.RunPolicySpec) { p.AllowedDomains = widen(p.AllowedDomains); p.AllowedDomains = slices.DeleteFunc(p.AllowedDomains, nil) }`, 0, 1},
		{"allow_all_egress turned on is a site, off is not", `
func f(p *types.RunPolicySpec) { p.AllowAllEgress = true; p.AllowAllEgress = false }`, 0, 1},
		{"an unsealed proxy sink", `
func (s *Server) sideDoor() { s.keepRunProxyConfig() }
func (s *Server) handle() { s.sideDoor() }`, 1, 0},
		{"a sink no one calls is not sealed", `
func (s *Server) orphan() { s.cfg.Runner.CreateSandbox() }`, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			files := map[string]*ast.File{}
			for name, src := range map[string]string{"paths.go": paths, "planted.go": "package api\n" + tc.src} {
				f, err := parser.ParseFile(fset, name, src, 0)
				if err != nil {
					t.Fatal(err)
				}
				files[name] = f
			}
			c := runSealCensus(fset, files)
			if len(c.violations) != tc.violations || len(c.adds) != tc.adds {
				t.Errorf("violations = %v, adds = %d; want %d violations, %d adds", c.violations, len(c.adds), tc.violations, tc.adds)
			}
		})
	}
	for _, tc := range []struct{ name, dispatch string }{
		{"a union after the seal in a sealed path", `func (s *Server) dispatchRun(policy types.RunPolicySpec) { s.reassertCeilingDenies(); s.sealRunPolicy(); unionAllowedDomains(&policy, nil) }`},
		{"a method on the sealed config that reaches a union after the seal", `func (s *Server) dispatchRun() { s.reassertCeilingDenies(); s.sealRunPolicy() }
func (s *Server) reviveRunProxy(cfg *proxy.Config) { reassertProxyCeiling(); s.sealRunPolicy(); cfg.widen() }
func (c *config) widen() { c.Policy.AllowedDomains = append(c.Policy.AllowedDomains, "x") }`},
		{"a call handed the policy that reaches a union after the seal", `func (s *Server) dispatchRun(policy types.RunPolicySpec) { s.reassertCeilingDenies(); s.sealRunPolicy(); s.widenLate(&policy) }
func (s *Server) widenLate(p *types.RunPolicySpec) { p.AllowedDomains = append(p.AllowedDomains, "x") }`},
		{"a seal before the re-assertion", `func (s *Server) dispatchRun() { s.sealRunPolicy(); s.reassertCeilingDenies() }`},
		{"no seal", `func (s *Server) dispatchRun() { s.reassertCeilingDenies() }`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			src := "package api\n" + tc.dispatch + "\n"
			if !strings.Contains(src, "reviveRunProxy") {
				src += "func (s *Server) reviveRunProxy() { reassertProxyCeiling(); s.sealRunPolicy() }\n"
			}
			f, err := parser.ParseFile(fset, "planted.go", src, 0)
			if err != nil {
				t.Fatal(err)
			}
			if c := runSealCensus(fset, map[string]*ast.File{"planted.go": f}); len(c.violations) == 0 {
				t.Error("the guard reported nothing")
			}
		})
	}
}
