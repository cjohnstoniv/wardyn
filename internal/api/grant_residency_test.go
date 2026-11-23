// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// grantResidencyKindRows lists, by the kind's string value, the answer for the
// default arguments (broker off, no brokered repos, no proxy signing).
var grantResidencyKindRows = map[string]struct{ inside, known bool }{
	"api_key":      {false, true},
	"ssh_key":      {false, true},
	"git_pat":      {true, true},
	"github_token": {true, true},
	"cloud_sts":    {true, true},
	"env_secret":   {true, true},
	"file_secret":  {true, true},
}

// grantResidencyKindConsts returns the string value of every GrantKind const
// declared in internal/types/types.go.
func grantResidencyKindConsts(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "../types/types.go", nil, 0)
	if err != nil {
		t.Fatalf("parse types.go: %v", err)
	}
	var out []string
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, s := range gd.Specs {
			vs := s.(*ast.ValueSpec)
			id, ok := vs.Type.(*ast.Ident)
			if !ok || id.Name != "GrantKind" {
				continue
			}
			for _, v := range vs.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("GrantKind const %v is not a string literal", vs.Names)
				}
				val, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", lit.Value, err)
				}
				out = append(out, val)
			}
		}
	}
	return out
}

func TestGrantResidencyCoversEveryKind(t *testing.T) {
	kinds := grantResidencyKindConsts(t)
	if len(kinds) == 0 {
		t.Fatal("no GrantKind consts found in types.go")
	}
	for _, k := range kinds {
		want, ok := grantResidencyKindRows[k]
		if !ok {
			t.Errorf("GrantKind %q has no residency row", k)
			continue
		}
		inside, known := grantResidency(types.GrantSpec{Kind: types.GrantKind(k)}, false, nil, false)
		if inside != want.inside || known != want.known {
			t.Errorf("kind %q = (%v, %v), want (%v, %v)", k, inside, known, want.inside, want.known)
		}
	}
}

func grantResidencyGitHubGrant(repos ...string) types.GrantSpec {
	b, _ := json.Marshal(map[string]any{"repos": repos})
	return types.GrantSpec{Kind: types.GrantGitHubToken, Scope: b}
}

func TestGrantResidencyGitHubToken(t *testing.T) {
	cases := []struct {
		name     string
		grant    types.GrantSpec
		brokerOn bool
		brokered []string
		inside   bool
	}{
		{"all repos brokered", grantResidencyGitHubGrant("a/x", "a/y"), true, []string{"a/x", "a/y", "a/z"}, false},
		{"broker off", grantResidencyGitHubGrant("a/x"), false, []string{"a/x"}, true},
		{"one repo not brokered", grantResidencyGitHubGrant("a/x", "a/y"), true, []string{"a/x"}, true},
		{"no brokered repos", grantResidencyGitHubGrant("a/x"), true, nil, true},
		{"scope names no repos", grantResidencyGitHubGrant(), true, []string{"a/x"}, true},
		{"scope unreadable", types.GrantSpec{Kind: types.GrantGitHubToken, Scope: json.RawMessage(`not json`)}, true, []string{"a/x"}, true},
		{"scope absent", types.GrantSpec{Kind: types.GrantGitHubToken}, true, []string{"a/x"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			inside, known := grantResidency(c.grant, c.brokerOn, c.brokered, false)
			if inside != c.inside || !known {
				t.Fatalf("got (%v, %v), want (%v, true)", inside, known, c.inside)
			}
		})
	}
}

func TestGrantResidencyCloudSTS(t *testing.T) {
	g := types.GrantSpec{Kind: types.GrantCloudSTS}
	if inside, known := grantResidency(g, false, nil, true); inside || !known {
		t.Fatalf("signed at the proxy: got (%v, %v), want (false, true)", inside, known)
	}
	if inside, known := grantResidency(g, true, nil, false); !inside || !known {
		t.Fatalf("not signed at the proxy: got (%v, %v), want (true, true)", inside, known)
	}
}

func TestGrantResidencyFlagsOnlyMoveTheirOwnKinds(t *testing.T) {
	for _, k := range []types.GrantKind{types.GrantAPIKey, types.GrantSSHKey, types.GrantGitPAT, types.GrantEnvSecret, types.GrantFileSecret} {
		want := grantResidencyKindRows[string(k)]
		inside, known := grantResidency(types.GrantSpec{Kind: k}, true, []string{"a/x"}, true)
		if inside != want.inside || known != want.known {
			t.Errorf("kind %q with flags on = (%v, %v), want (%v, %v)", k, inside, known, want.inside, want.known)
		}
	}
}

func TestGrantResidencyUnknownKind(t *testing.T) {
	for _, k := range []types.GrantKind{"", "no_such_kind"} {
		if inside, known := grantResidency(types.GrantSpec{Kind: k}, true, []string{"a/x"}, true); inside || known {
			t.Errorf("kind %q = (%v, %v), want (false, false)", k, inside, known)
		}
	}
}
