// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// overrideNarrowingGolden is the console's copy of the table. The Go table is
// the source: regenerate it with WARDYN_UPDATE_GOLDEN=1 go test ./internal/types
// -run TestOverrideNarrowingGolden.
var overrideNarrowingGolden = filepath.Join("..", "..", "ui", "src", "app", "lib", "override-narrowing.json")

func TestOverrideNarrowingGolden(t *testing.T) {
	want, err := json.MarshalIndent(OverrideNarrowingTable(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	if os.Getenv("WARDYN_UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(overrideNarrowingGolden, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(overrideNarrowingGolden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is not the narrowing table; run WARDYN_UPDATE_GOLDEN=1 go test ./internal/types -run TestOverrideNarrowingGolden", overrideNarrowingGolden)
	}
}

// TestOverrideNarrowingPrinciple pins OD-1's rule in the table itself: tightening
// is always allowed, widening is never plainly allowed, and one pair has one row.
func TestOverrideNarrowingPrinciple(t *testing.T) {
	type pair struct {
		k OverrideKind
		o OverrideOp
	}
	var seen []pair
	for _, row := range OverrideNarrowingTable() {
		p := pair{row.Kind, row.Op}
		if slices.Contains(seen, p) {
			t.Errorf("%s %s has two rows", row.Kind, row.Op)
		}
		seen = append(seen, p)
		switch row.Direction {
		case OverrideTighten:
			if row.Rule != OverrideAllowed {
				t.Errorf("%s %s tightens and is %s", row.Kind, row.Op, row.Rule)
			}
		case OverrideWiden:
			if row.Rule == OverrideAllowed {
				t.Errorf("%s %s widens and is plainly allowed", row.Kind, row.Op)
			}
		default:
			t.Errorf("%s %s has direction %q", row.Kind, row.Op, row.Direction)
		}
	}
	for _, kind := range []OverrideKind{OverrideAgentHost, OverrideAgentHostWildcard, OverrideAgentSecret, OverrideToolRuleRestrict,
		OverrideToolRuleAllow, OverrideADOCapability, OverrideGitPATScope, OverridePushDeny, OverridePushReview} {
		if !slices.ContainsFunc(seen, func(p pair) bool { return p.k == kind }) {
			t.Errorf("%s has no row", kind)
		}
	}
	// A push rule is monotone: it can be added to and never given back.
	for _, kind := range []OverrideKind{OverridePushDeny, OverridePushReview} {
		if add, _ := OverrideRuleFor(kind, OverrideAdd); add.Rule != OverrideAllowed {
			t.Errorf("%s add = %s", kind, add.Rule)
		}
		if rm, _ := OverrideRuleFor(kind, OverrideRemove); rm.Rule != OverrideRefused {
			t.Errorf("%s remove = %s", kind, rm.Rule)
		}
	}
}

func TestComponentRefBuiltinShape(t *testing.T) {
	ok := func(string) error { return nil }
	for _, tc := range []struct {
		name string
		ref  ComponentRef
		bad  bool
	}{
		{"github", ComponentRef{Builtin: "github", Org: "acme", Repos: []string{"acme/api"}}, false},
		{"github needs repos", ComponentRef{Builtin: "github", Org: "acme"}, true},
		{"ado may list none", ComponentRef{Builtin: "azure_devops", Org: "acme"}, false},
		{"ado with repos and write", ComponentRef{Builtin: "azure_devops", Org: "acme", Repos: []string{"proj/repo"}, Access: "write"}, false},
		{"unknown builtin", ComponentRef{Builtin: "gitlab", Org: "acme"}, true},
		{"no org", ComponentRef{Builtin: "github", Repos: []string{"a/b"}}, true},
		{"bad access", ComponentRef{Builtin: "github", Org: "acme", Repos: []string{"a/b"}, Access: "admin"}, true},
		{"repo shape", ComponentRef{Builtin: "github", Org: "acme", Repos: []string{"nope"}}, true},
		{"repo twice", ComponentRef{Builtin: "github", Org: "acme", Repos: []string{"a/b", "a/b"}}, true},
		{"with a name", ComponentRef{Builtin: "github", Org: "acme", Repos: []string{"a/b"}, Name: "x"}, true},
		{"scope without builtin", ComponentRef{Org: "acme"}, true},
	} {
		if err := tc.ref.Validate(ok); (err != nil) != tc.bad {
			t.Errorf("%s: err = %v, want bad=%v", tc.name, err, tc.bad)
		}
	}
}
