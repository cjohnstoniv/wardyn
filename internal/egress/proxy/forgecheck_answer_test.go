// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
	"github.com/cjohnstoniv/wardyn/internal/reqcheck"
)

// plantForgeDecisions makes the two gates read the given decisions for the
// rest of the test, and counts how often each was asked.
func plantForgeDecisions(t *testing.T, ado reqcheck.Decision[adoscope.Verdict], gh reqcheck.Decision[ghscope.Verdict]) (adoCalls, ghCalls *int) {
	t.Helper()
	oldADO, oldGH := decideADO, decideGitHub
	t.Cleanup(func() { decideADO, decideGitHub = oldADO, oldGH })
	adoCalls, ghCalls = new(int), new(int)
	decideADO = func(reqcheck.Checker[adoscope.Verdict], reqcheck.Request) reqcheck.Decision[adoscope.Verdict] {
		*adoCalls++
		return ado
	}
	decideGitHub = func(reqcheck.Checker[ghscope.Verdict], reqcheck.Request) reqcheck.Decision[ghscope.Verdict] {
		*ghCalls++
		return gh
	}
	return adoCalls, ghCalls
}

// forgeDecisionCase is one planted decision per forge and the sentence each
// door's refusal must carry.
type forgeDecisionCase struct {
	name    string
	ado     reqcheck.Decision[adoscope.Verdict]
	gh      reqcheck.Decision[ghscope.Verdict]
	adoWant string
	ghWant  string
}

// assertForgeDecisionsRefuse sends, for each case, a request each run's grant
// covers through gateADO and serveGitHubAPI, and requires a refusal carrying
// the case's sentence, with nothing minted and nothing forwarded.
func assertForgeDecisionsRefuse(t *testing.T, cases []forgeDecisionCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name+"/azure devops", func(t *testing.T) {
			h := newADOHarness(t, adoscope.CapProjectRead)
			calls, _ := plantForgeDecisions(t, c.ado, c.gh)
			rec := h.do(t, http.MethodGet, "/acme/_apis/projects?api-version=7.1", "", map[string]string{"Authorization": "Bearer sandbox-placeholder"})
			if *calls != 1 {
				t.Fatalf("the gate asked for %d decisions, want 1", *calls)
			}
			h.mustRefuse(t, rec, c.adoWant)
		})
		t.Run(c.name+"/github", func(t *testing.T) {
			up := newGHUpstream(t)
			p, _ := newGHProxy(t, up)
			_, calls := plantForgeDecisions(t, c.ado, c.gh)
			rec := httptest.NewRecorder()
			p.serveMITMRequest(rec, ghTunnelRequest(t, http.MethodGet, "/repos/acme/app/issues?state=open"), ghscope.APIHost, 443)
			if *calls != 1 {
				t.Fatalf("the gate asked for %d decisions, want 1", *calls)
			}
			ghRefused(t, rec)
			var body map[string]string
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if body["message"] != c.ghWant {
				t.Errorf("refusal says %q, want %q", body["message"], c.ghWant)
			}
			if mints, seen := up.counts(); mints != 0 || len(seen) != 0 {
				t.Fatalf("a refused request reached upstream: mints %d seen %q", mints, seen)
			}
		})
	}
}

// A verdict each run holds: only the decision's Effect refuses these requests.
var (
	forgeHeldADOVerdict = adoscope.Verdict{Capability: adoscope.CapProjectRead}
	forgeHeldGHVerdict  = ghscope.Verdict{Capability: ghscope.CapIssuesRead, Owner: "acme", Repo: "app"}
)

func TestForgeCheckZeroDecisionRefuses(t *testing.T) {
	assertForgeDecisionsRefuse(t, []forgeDecisionCase{{
		name: "zero decision", adoWant: adoCouldNotTell, ghWant: gitHubCouldNotRead,
	}})
}

func TestForgeCheckEmptySentenceRefuses(t *testing.T) {
	assertForgeDecisionsRefuse(t, []forgeDecisionCase{
		{
			name:    "refuse without a sentence",
			ado:     reqcheck.Decision[adoscope.Verdict]{Effect: reqcheck.Refuse, Verdict: forgeHeldADOVerdict},
			gh:      reqcheck.Decision[ghscope.Verdict]{Effect: reqcheck.Refuse, Verdict: forgeHeldGHVerdict},
			adoWant: adoCouldNotTell, ghWant: gitHubCouldNotRead,
		},
		{
			name:    "hold without a sentence",
			ado:     reqcheck.Decision[adoscope.Verdict]{Effect: reqcheck.Hold, Verdict: adoscope.Verdict{Capability: adoscope.CapDeniedTokens}},
			gh:      reqcheck.Decision[ghscope.Verdict]{Effect: reqcheck.Hold, Verdict: forgeHeldGHVerdict},
			adoWant: adoCouldNotTell, ghWant: gitHubCouldNotRead,
		},
	})
}

func TestForgeCheckUnknownEffectRefuses(t *testing.T) {
	const s = "planted refusal sentence"
	var cases []forgeDecisionCase
	for _, e := range []reqcheck.Effect{reqcheck.Allow + 1, 255} {
		cases = append(cases, forgeDecisionCase{
			name:    fmt.Sprintf("effect %d", e),
			ado:     reqcheck.Decision[adoscope.Verdict]{Effect: e, Verdict: forgeHeldADOVerdict, Sentence: s},
			gh:      reqcheck.Decision[ghscope.Verdict]{Effect: e, Verdict: forgeHeldGHVerdict, Sentence: s},
			adoWant: s, ghWant: s,
		})
	}
	// A hold on a capability no run can hold is refused, never put to a
	// person: the door answers at once rather than wait for an approval.
	cases = append(cases, forgeDecisionCase{
		name:    "hold on a capability that is not grantable",
		ado:     reqcheck.Decision[adoscope.Verdict]{Effect: reqcheck.Hold, Verdict: adoscope.Verdict{Capability: adoscope.CapDeniedTokens}, Sentence: s},
		gh:      reqcheck.Decision[ghscope.Verdict]{Effect: reqcheck.Hold, Verdict: ghscope.Verdict{Capability: ghscope.CapDeniedGraphQL}, Sentence: s},
		adoWant: s, ghWant: s,
	})
	assertForgeDecisionsRefuse(t, cases)
}

// TestForgeCheckHoldOnlyLiftsAGrantableCapability: adoAnswer offers a held
// verdict for a person to grant only when a run can hold the capability. The
// doors refuse either way when no approver is wired, so this reads the answer
// itself.
func TestForgeCheckHoldOnlyLiftsAGrantableCapability(t *testing.T) {
	const s = "planted refusal sentence"
	if got, held := adoAnswer(reqcheck.Decision[adoscope.Verdict]{Effect: reqcheck.Hold, Verdict: adoscope.Verdict{Capability: adoscope.CapDeniedTokens}, Sentence: s}); held != nil || got != s {
		t.Fatalf("hold on a capability that is not grantable: got (%q, %v), want (%q, nil)", got, held, s)
	}
	got, held := adoAnswer(reqcheck.Decision[adoscope.Verdict]{Effect: reqcheck.Hold, Verdict: forgeHeldADOVerdict, Sentence: s})
	if held == nil || held.Capability != forgeHeldADOVerdict.Capability || got != s {
		t.Fatalf("hold on a grantable capability: got (%q, %v), want (%q, %v)", got, held, s, forgeHeldADOVerdict)
	}
}

// forgeDecideSeams are the two variables only a test may assign.
var forgeDecideSeams = map[string]bool{"decideADO": true, "decideGitHub": true}

// forgeSeamWrites is each place in f that assigns a seam or takes its
// address, which is the only way to change it from outside the assignment.
func forgeSeamWrites(fset *token.FileSet, f *ast.File) []string {
	isSeam := func(e ast.Expr) bool {
		for {
			p, ok := e.(*ast.ParenExpr)
			if !ok {
				break
			}
			e = p.X
		}
		id, ok := e.(*ast.Ident)
		return ok && forgeDecideSeams[id.Name]
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			for _, l := range n.Lhs {
				if isSeam(l) {
					out = append(out, fmt.Sprintf("%s: assigns a forge decision seam", fset.Position(l.Pos())))
				}
			}
		case *ast.UnaryExpr:
			if n.Op == token.AND && isSeam(n.X) {
				out = append(out, fmt.Sprintf("%s: takes a forge decision seam's address", fset.Position(n.Pos())))
			}
		}
		return true
	})
	return out
}

// TestForgeDecideSeamsAssignedOnlyInTests: no file the binary is built from
// assigns decideADO or decideGitHub, so the gates always decide through
// reqcheck.Decide.
func TestForgeDecideSeamsAssignedOnlyInTests(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var findings []string
	declared := map[string]bool{}
	files := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files++
		findings = append(findings, forgeSeamWrites(fset, f)...)
		for _, d := range f.Decls {
			if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.VAR {
				for _, s := range g.Specs {
					for _, id := range s.(*ast.ValueSpec).Names {
						if forgeDecideSeams[id.Name] {
							declared[id.Name] = true
						}
					}
				}
			}
		}
	}
	if files == 0 || len(declared) != len(forgeDecideSeams) {
		t.Fatalf("scanned %d files and found the declarations of %v, want both seams declared in this package", files, declared)
	}
	if len(findings) > 0 {
		t.Fatalf("a non-test file changes a forge decision seam:\n%s", strings.Join(findings, "\n"))
	}
}

// TestForgeDecideSeamsAssignedOnlyInTests_PlantedViolation: the check finds an
// assignment and an address taken, and passes over the declaration.
func TestForgeDecideSeamsAssignedOnlyInTests_PlantedViolation(t *testing.T) {
	for _, c := range []struct {
		src  string
		want int
	}{
		{"package proxy\n\nfunc f() { decideADO = f2 }\n", 1},
		{"package proxy\n\nfunc f() { p := &decideGitHub; _ = p }\n", 1},
		{"package proxy\n\nvar decideADO = reqcheck.Decide[adoscope.Verdict]\n", 0},
	} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "planted.go", c.src, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := forgeSeamWrites(fset, f); len(got) != c.want {
			t.Errorf("%q: %d findings %v, want %d", c.src, len(got), got, c.want)
		}
	}
}
