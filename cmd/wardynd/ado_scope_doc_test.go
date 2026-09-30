// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
)

// adoScopeRow matches a scope table row in docs/AZURE-DEVOPS.md:
// a first cell holding one backticked vso.* scope.
var adoScopeRow = regexp.MustCompile("(?m)^\\| `(vso\\.[a-z_]+)` \\|")

// TestADOEntraDocListsEveryRequestedScope fails when a scope Wardyn can request
// for the Azure DevOps Entra lane is missing from the app-registration tables.
// An admin adds exactly the scopes that page lists, so a missing one is a
// capability that 403s at Azure DevOps on every install that followed the doc.
func TestADOEntraDocListsEveryRequestedScope(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "AZURE-DEVOPS.md"))
	if err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	for _, m := range adoScopeRow.FindAllStringSubmatch(string(b), -1) {
		documented[m[1]] = true
	}
	scopes, err := adoscope.ScopesFor(adoscope.GrantableCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range scopes {
		s := strings.TrimPrefix(q, adoscope.ResourceID+"/")
		if !documented[s] {
			t.Errorf("docs/AZURE-DEVOPS.md has no scope table row for %s, which adoscope.ScopesFor can request", s)
		}
	}
}

// adoConsoleLine matches one capability's grey Azure DevOps line in the
// console copy: its key, then the scopes the line names before the dash.
var adoConsoleLine = regexp.MustCompile(`(?s)\n  ([a-z_]+): \{\n[^}]*?ado: "ADO: ([^"]*?) — `)

// TestADOConsoleLineNamesTheRequestedScopes holds the console's grey Azure
// DevOps line for every capability to the scopes adoscope.ScopesFor requests
// for it, so the scope a person is shown is the scope a token carries.
func TestADOConsoleLineNamesTheRequestedScopes(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "ui", "src", "app", "lib", "workspace-providers-copy.ts"))
	if err != nil {
		t.Fatal(err)
	}
	shown := map[string]string{}
	for _, m := range adoConsoleLine.FindAllStringSubmatch(string(b), -1) {
		shown[m[1]] = m[2]
	}
	for _, c := range adoscope.GrantableCapabilities() {
		scopes, err := adoscope.ScopesFor([]adoscope.Capability{c})
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]bool{}
		for _, q := range scopes {
			want[strings.TrimPrefix(q, adoscope.ResourceID+"/")] = true
		}
		line, ok := shown[string(c)]
		if !ok {
			t.Errorf("the console copy has no Azure DevOps line for %s", c)
			continue
		}
		got := map[string]bool{}
		for _, s := range strings.Split(line, ", ") {
			got[s] = true
		}
		if len(got) != len(want) {
			t.Errorf("%s: the console shows %q, adoscope.ScopesFor requests %v", c, line, scopes)
			continue
		}
		for s := range want {
			if !got[s] {
				t.Errorf("%s: the console shows %q, adoscope.ScopesFor requests %v", c, line, scopes)
			}
		}
	}
}
