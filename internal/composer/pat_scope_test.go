// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// patScope builds a git_pat scope from the fields a case sets. An empty string
// is an omitted field, so every row states its omissions in the literal.
func patScope(fields string) json.RawMessage {
	if fields == "" {
		return json.RawMessage(`{"host":"git.example.com","secret_name":"pat"}`)
	}
	return json.RawMessage(`{"host":"git.example.com","secret_name":"pat",` + fields + `}`)
}

func patGrant(fields string) types.GrantSpec {
	return types.GrantSpec{Kind: types.GrantGitPAT, Scope: patScope(fields)}
}

func patReposOf(t *testing.T, raw json.RawMessage) (repos []string, set bool) {
	t.Helper()
	sc, err := types.DecodeGitPATScope(raw)
	if err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if sc.Repos == nil {
		return nil, false
	}
	return *sc.Repos, true
}

// Every expected verdict below is written by hand from the contract in
// docs/design/0.8/0.8.6-gpat.md D4, never produced by the comparator.
func TestPATScopeWithin(t *testing.T) {
	for _, tc := range []struct {
		name     string
		proposal string // scope fields, "" = omitted
		ceiling  string
		want     string // "" = within, else a substring of the refusal
	}{
		// repos
		{"both omit everything", ``, ``, ""},
		{"ceiling omits repos, proposal narrows", `"repos":["team/app"]`, ``, ""},
		{"ceiling omits repos, proposal is empty", `"repos":[]`, ``, ""},
		{"same list", `"repos":["team/app"]`, `"repos":["team/app"]`, ""},
		{"subset", `"repos":["team/app"]`, `"repos":["team/app","team/lib"]`, ""},
		{"substitution", `"repos":["team/other"]`, `"repos":["team/app"]`, "repos entry"},
		{"superset", `"repos":["team/app","team/lib"]`, `"repos":["team/app"]`, "repos entry"},
		{"proposal omits repos under a narrowed ceiling", ``, `"repos":["team/app"]`, "repos is omitted"},
		{"proposal omits repos under an empty ceiling list", ``, `"repos":[]`, "repos is omitted"},
		{"empty proposal list under a narrowed ceiling", `"repos":[]`, `"repos":["team/app"]`, ""},
		{"empty proposal list under an empty ceiling list", `"repos":[]`, `"repos":[]`, ""},
		{"entry under an empty ceiling list", `"repos":["team/app"]`, `"repos":[]`, "repos entry"},
		{"wildcard covers a name", `"repos":["group/app"]`, `"repos":["group/*"]`, ""},
		{"wildcard does not cover a nested name", `"repos":["group/sub/app"]`, `"repos":["group/*"]`, "repos entry"},
		{"wildcard covered by the same wildcard", `"repos":["group/*"]`, `"repos":["group/*"]`, ""},
		{"wildcard not covered by a name", `"repos":["group/*"]`, `"repos":["group/app"]`, "repos entry"},
		{"wildcard not covered by a list of names", `"repos":["group/*"]`, `"repos":["group/a","group/b"]`, "repos entry"},
		{"wildcard covered by an omitted ceiling list", `"repos":["group/*"]`, ``, ""},
		{"case differs on generic", `"repos":["Team/App"]`, `"repos":["team/app"]`, "repos entry"},
		{"dot git differs on generic", `"repos":["team/app.git"]`, `"repos":["team/app"]`, "repos entry"},
		{"prefix is not coverage", `"repos":["team/app-x"]`, `"repos":["team/app"]`, "repos entry"},
		{"malformed proposal entry is covered by nothing", `"repos":["a/../b"]`, `"repos":["a/../b"]`, "repos entry"},
		// access
		{"write under write", `"access":"write"`, `"access":"write"`, ""},
		{"omitted under omitted", ``, ``, ""},
		{"read under write", `"access":"read"`, `"access":"write"`, ""},
		{"read under read", `"access":"read"`, `"access":"read"`, ""},
		{"read under omitted", `"access":"read"`, ``, ""},
		{"write under read", `"access":"write"`, `"access":"read"`, "access"},
		{"omitted proposal is write, under read", ``, `"access":"read"`, "access"},
		// api
		{"api false under false", `"api":false`, `"api":false`, ""},
		{"api false under true", `"forge":"gitlab"`, `"api":true,"forge":"gitlab"`, ""},
		{"api false under true, forge omitted", ``, `"api":true,"forge":"gitlab"`, "forge"},
		{"api true under true", `"api":true,"forge":"gitlab"`, `"api":true,"forge":"gitlab"`, ""},
		{"api true under false", `"api":true,"forge":"gitlab"`, `"api":false,"forge":"gitlab"`, "api"},
		{"api true under omitted", `"api":true,"forge":"gitlab"`, ``, "api"},
		// forge
		{"same forge", `"forge":"gitlab","repos":["team/app"]`, `"forge":"gitlab","repos":["team/app"]`, ""},
		{"forge change under a repos ceiling", `"forge":"gitea","repos":["team/app"]`, `"forge":"gitlab","repos":["team/app"]`, "forge"},
		{"forge change under an api ceiling", `"forge":"gitea"`, `"forge":"gitlab","api":true`, "forge"},
		{"forge omitted under a gitlab repos ceiling", `"repos":["team/app"]`, `"forge":"gitlab","repos":["team/app"]`, "forge"},
		{"forge gitlab under a pre-0.8.6 repos ceiling (generic)", `"forge":"gitlab","repos":["team/app"]`, `"repos":["team/app"]`, "forge"},
		{"forge is free under an unnarrowed ceiling", `"forge":"gitlab","repos":["team/app"],"access":"read"`, ``, ""},
		{"forge is free under an access-only ceiling", `"forge":"gitea"`, `"access":"read","forge":"gitlab"`, "access"},
		{"forge is free under a write ceiling that names a forge", `"forge":"gitea"`, `"forge":"gitlab"`, ""},
		// several axes at once
		{"all axes inside", `"repos":["team/app"],"access":"read"`, `"repos":["team/app","team/lib"],"access":"write"`, ""},
		{"repos fine, access widened", `"repos":["team/app"],"access":"write"`, `"repos":["team/app"],"access":"read"`, "access"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := PATScopeWithin(patScope(tc.proposal), patScope(tc.ceiling))
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.want != "" && err == nil:
				t.Fatalf("accepted, want a refusal naming %q", tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("refusal %q does not name %q", err, tc.want)
			}
		})
	}
}

func TestPATScopeWithinUndecodableFailsClosed(t *testing.T) {
	good := patScope(``)
	for _, bad := range []json.RawMessage{nil, json.RawMessage(`{}`), json.RawMessage(`{"host":"h"}`), json.RawMessage(`[1]`)} {
		if PATScopeWithin(bad, good) == nil {
			t.Errorf("an undecodable proposal %s was within the ceiling", bad)
		}
		if PATScopeWithin(good, bad) == nil {
			t.Errorf("an undecodable ceiling %s dominated a proposal", bad)
		}
	}
}

func TestPATScopeMeet(t *testing.T) {
	for _, tc := range []struct {
		name       string
		proposal   string
		bounds     []string
		keep       bool
		wantRepos  []string // nil with wantSet false = absent
		wantSet    bool
		wantAccess string
		wantAPI    bool
		wantForge  string
	}{
		{name: "no bounds leaves it", proposal: `"repos":["b/x","a/y"]`, keep: true,
			wantRepos: []string{"b/x", "a/y"}, wantSet: true, wantAccess: "write", wantForge: "generic"},
		{name: "unnarrowed bound leaves it", proposal: `"repos":["a/y"]`, bounds: []string{``}, keep: true,
			wantRepos: []string{"a/y"}, wantSet: true, wantAccess: "write", wantForge: "generic"},
		{name: "proposal omits repos, bound narrows: takes the bound", proposal: ``, bounds: []string{`"repos":["team/app"]`}, keep: true,
			wantRepos: []string{"team/app"}, wantSet: true, wantAccess: "write", wantForge: "generic"},
		{name: "intersection", proposal: `"repos":["a/1","a/2","a/3"]`, bounds: []string{`"repos":["a/2","a/3","a/4"]`}, keep: true,
			wantRepos: []string{"a/2", "a/3"}, wantSet: true, wantAccess: "write", wantForge: "generic"},
		{name: "wildcard narrowed by a name", proposal: `"repos":["g/*"]`, bounds: []string{`"repos":["g/app"]`}, keep: true,
			wantRepos: []string{"g/app"}, wantSet: true, wantAccess: "write", wantForge: "generic"},
		{name: "name narrowed by a wildcard keeps the name", proposal: `"repos":["g/app","h/x"]`, bounds: []string{`"repos":["g/*"]`}, keep: true,
			wantRepos: []string{"g/app"}, wantSet: true, wantAccess: "write", wantForge: "generic"},
		{name: "empty intersection drops the grant", proposal: `"repos":["a/1"]`, bounds: []string{`"repos":["b/1"]`}, keep: false},
		{name: "proposal omits repos, bound is the empty list: dropped", proposal: ``, bounds: []string{`"repos":[]`}, keep: false},
		{name: "authored empty list is kept as none", proposal: `"repos":[]`, bounds: []string{`"repos":["a/1"]`}, keep: true,
			wantRepos: []string{}, wantSet: true, wantAccess: "write", wantForge: "generic"},
		{name: "access takes the lower", proposal: ``, bounds: []string{`"access":"read"`}, keep: true,
			wantAccess: "read", wantForge: "generic"},
		{name: "access never raised", proposal: `"access":"read"`, bounds: []string{`"access":"write"`}, keep: true,
			wantAccess: "read", wantForge: "generic"},
		{name: "api cleared by a bound that has it off", proposal: `"api":true,"forge":"gitlab"`, bounds: []string{``}, keep: true,
			wantAccess: "write", wantAPI: false, wantForge: "gitlab"},
		{name: "api kept under a bound that has it on", proposal: `"api":true,"forge":"gitlab"`, bounds: []string{`"api":true,"forge":"gitlab"`}, keep: true,
			wantAccess: "write", wantAPI: true, wantForge: "gitlab"},
		{name: "forge disagreement with a repos bound drops", proposal: `"forge":"gitea"`, bounds: []string{`"forge":"gitlab","repos":["a/b"]`}, keep: false},
		{name: "forge disagreement with an api bound drops", proposal: `"forge":"gitea"`, bounds: []string{`"forge":"gitlab","api":true`}, keep: false},
		{name: "forge kept against a bound with no repos and api false", proposal: `"forge":"gitea","repos":["a/b"]`, bounds: []string{`"forge":"gitlab","access":"read"`}, keep: true,
			wantRepos: []string{"a/b"}, wantSet: true, wantAccess: "read", wantForge: "gitea"},
		{name: "every bound is applied", proposal: `"repos":["a/1","a/2","a/3"]`, bounds: []string{`"repos":["a/1","a/2"]`, `"repos":["a/2","a/3"]`}, keep: true,
			wantRepos: []string{"a/2"}, wantSet: true, wantAccess: "write", wantForge: "generic"},
		{name: "two bounds with a disjoint intersection drop", proposal: `"repos":["a/1","a/2"]`, bounds: []string{`"repos":["a/1"]`, `"repos":["a/2"]`}, keep: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bounds := make([]json.RawMessage, len(tc.bounds))
			for i, b := range tc.bounds {
				bounds[i] = patScope(b)
			}
			var warns []string
			got, keep := PATScopeMeet(patScope(tc.proposal), bounds, &warns)
			if keep != tc.keep {
				t.Fatalf("keep = %v, want %v (warnings %q)", keep, tc.keep, warns)
			}
			if !keep {
				if len(warns) == 0 {
					t.Error("a dropped grant must say why")
				}
				return
			}
			sc, err := types.DecodeGitPATScope(got)
			if err != nil {
				t.Fatalf("result %s: %v", got, err)
			}
			repos, set := patReposOf(t, got)
			if set != tc.wantSet || !slices.Equal(repos, tc.wantRepos) {
				t.Errorf("repos = %v (set=%v), want %v (set=%v)", repos, set, tc.wantRepos, tc.wantSet)
			}
			if sc.Access != tc.wantAccess || sc.API != tc.wantAPI || sc.Forge != tc.wantForge {
				t.Errorf("access/api/forge = %s/%v/%s, want %s/%v/%s", sc.Access, sc.API, sc.Forge, tc.wantAccess, tc.wantAPI, tc.wantForge)
			}
		})
	}
}

// A meet that changed nothing hands back the proposal byte for byte, so a
// dominated grant is never rewritten (or warned about) on its way through Clamp.
func TestPATScopeMeetUnchangedKeepsTheRawScope(t *testing.T) {
	proposal := json.RawMessage(`{"host":"git.example.com","secret_name":"pat","stray":1,"repos":["g/*","g/x"]}`)
	var warns []string
	got, keep := PATScopeMeet(proposal, []json.RawMessage{patScope(`"repos":["g/*"]`)}, &warns)
	if !keep || string(got) != string(proposal) || len(warns) != 0 {
		t.Fatalf("got %s keep=%v warns=%q, want the proposal untouched", got, keep, warns)
	}
}

// The meet does not depend on the order its bounds arrive in.
func TestPATScopeMeetIsOrderIndependent(t *testing.T) {
	bounds := []string{
		`"repos":["a/1","a/2","g/*"],"access":"write"`,
		`"repos":["a/2","g/x","g/y"],"access":"read"`,
		`"access":"write"`,
		`"repos":["a/2","g/x","z/9"]`,
	}
	proposal := patScope(`"repos":["a/2","g/x","g/q"],"api":false`)
	var first json.RawMessage
	for _, perm := range permutations(len(bounds)) {
		ordered := make([]json.RawMessage, len(bounds))
		for i, p := range perm {
			ordered[i] = patScope(bounds[p])
		}
		var warns []string
		got, keep := PATScopeMeet(proposal, ordered, &warns)
		if !keep {
			t.Fatalf("order %v dropped the grant: %q", perm, warns)
		}
		if first == nil {
			first = got
		} else if string(first) != string(got) {
			t.Fatalf("order %v gave %s, another order gave %s", perm, got, first)
		}
	}
	repos, _ := patReposOf(t, first)
	if !slices.Equal(repos, []string{"a/2", "g/x"}) {
		t.Fatalf("repos = %v, want [a/2 g/x]", repos)
	}
}

func permutations(n int) [][]int {
	if n == 0 {
		return [][]int{{}}
	}
	var out [][]int
	for _, p := range permutations(n - 1) {
		for i := 0; i <= len(p); i++ {
			q := slices.Insert(slices.Clone(p), i, n-1)
			out = append(out, q)
		}
	}
	return out
}

// Clamp is the create/preflight seam. Each widening is narrowed or dropped by
// it, and a proposal the ceiling dominates passes through unchanged.
func TestClampGitPATAxes(t *testing.T) {
	ceiling := func(fields ...string) types.RunPolicySpec {
		var gs []types.GrantSpec
		for _, f := range fields {
			gs = append(gs, patGrant(f))
		}
		return types.RunPolicySpec{EligibleGrants: gs}
	}
	narrowed := `"repos":["team/app"],"access":"read"`

	for _, tc := range []struct {
		name     string
		ceiling  types.RunPolicySpec
		proposal string
		kept     bool
		contains string // the kept scope must contain this
		absent   string // ... and must not contain this
	}{
		{name: "repository substitution is dropped", ceiling: ceiling(narrowed), proposal: `"repos":["team/other"],"access":"read"`},
		{name: "read to write is clamped to read", ceiling: ceiling(narrowed), proposal: `"repos":["team/app"],"access":"write"`,
			kept: true, contains: `"access":"read"`},
		{name: "api false to true is cleared", ceiling: ceiling(`"repos":["team/app"],"forge":"gitlab"`),
			proposal: `"repos":["team/app"],"forge":"gitlab","api":true`, kept: true, absent: `"api"`},
		{name: "omitted repos take the ceiling's", ceiling: ceiling(narrowed), proposal: `"access":"read"`,
			kept: true, contains: `"repos":["team/app"]`},
		{name: "omitted access is clamped", ceiling: ceiling(narrowed), proposal: `"repos":["team/app"]`,
			kept: true, contains: `"access":"read"`},
		{name: "forge change against a repos ceiling is dropped", ceiling: ceiling(`"repos":["team/app"],"forge":"gitlab"`),
			proposal: `"repos":["team/app"],"forge":"gitea"`},
		{name: "dominated proposal passes unchanged", ceiling: ceiling(`"repos":["team/app","team/lib"]`),
			proposal: `"repos":["team/app"],"access":"read"`, kept: true},
		{name: "forge is free under an unnarrowed ceiling", ceiling: ceiling(``),
			proposal: `"forge":"gitlab","repos":["team/app"],"access":"read"`, kept: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := patGrant(tc.proposal)
			out, warns := Clamp(types.RunPolicySpec{EligibleGrants: []types.GrantSpec{p}}, tc.ceiling, types.GovernanceLimits{})
			if !tc.kept {
				if len(out.EligibleGrants) != 0 {
					t.Fatalf("grant survived as %s (warnings %q)", out.EligibleGrants[0].Scope, warns)
				}
				return
			}
			if len(out.EligibleGrants) != 1 {
				t.Fatalf("grant dropped (warnings %q)", warns)
			}
			got := string(out.EligibleGrants[0].Scope)
			if tc.contains == "" && tc.absent == "" && got != string(p.Scope) {
				t.Fatalf("scope changed to %s, want it unchanged: %s", got, p.Scope)
			}
			if tc.contains != "" && !strings.Contains(got, tc.contains) {
				t.Fatalf("scope %s does not contain %s", got, tc.contains)
			}
			if tc.absent != "" && strings.Contains(got, tc.absent) {
				t.Fatalf("scope %s still contains %s", got, tc.absent)
			}
		})
	}
}
