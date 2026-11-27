// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package ghscope

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"testing"
)

// declaredCapabilities reads every constant of type Capability out of
// capability.go, so the test holds Catalogue to the file and not to a list the
// test keeps.
func declaredCapabilities(t *testing.T) []Capability {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "capability.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []Capability
	for _, d := range file.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, s := range gd.Specs {
			vs := s.(*ast.ValueSpec)
			if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "Capability" {
				continue
			}
			for _, v := range vs.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("capability constant %v is not a string literal", vs.Names)
				}
				name, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				out = append(out, Capability(name))
			}
		}
	}
	return out
}

// TestCatalogueIsValidSet: Catalogue holds exactly the declared capabilities
// that Valid admits, once each, in order, and nothing Valid refuses.
func TestCatalogueIsValidSet(t *testing.T) {
	declared := declaredCapabilities(t)
	if len(declared) < 25 {
		t.Fatalf("parsed %d capability constants from capability.go, want at least 25 (the parse found too little)", len(declared))
	}
	var want []Capability
	for _, c := range declared {
		if c.Valid() {
			want = append(want, c)
		}
	}
	slices.Sort(want)
	got := Catalogue()
	if len(want) == 0 || !slices.Equal(got, want) {
		t.Fatalf("Catalogue() = %v, want the Valid declared constants %v", got, want)
	}
	if !slices.IsSorted(got) || len(slices.Compact(slices.Clone(got))) != len(got) {
		t.Errorf("Catalogue() = %v, want sorted with no repeats", got)
	}
	for _, c := range got {
		if !c.Valid() {
			t.Errorf("Catalogue() holds %q, which Valid refuses", c)
		}
	}
	for _, c := range GrantableCapabilities() {
		if !slices.Contains(got, c) {
			t.Errorf("Catalogue() lacks the grantable %q", c)
		}
	}
	got[0] = "changed"
	if Catalogue()[0] == "changed" {
		t.Error("Catalogue() returned a shared slice")
	}
}
