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

// adoScopeRow matches a scope table row in docs/adoption/azure-devops-entra.md:
// a first cell holding one backticked vso.* scope.
var adoScopeRow = regexp.MustCompile("(?m)^\\| `(vso\\.[a-z_]+)` \\|")

// TestADOEntraDocListsEveryRequestedScope fails when a scope Wardyn can request
// for the Azure DevOps Entra lane is missing from the app-registration tables.
// An admin adds exactly the scopes that page lists, so a missing one is a
// capability that 403s at Azure DevOps on every install that followed the doc.
func TestADOEntraDocListsEveryRequestedScope(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "adoption", "azure-devops-entra.md"))
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
			t.Errorf("docs/adoption/azure-devops-entra.md has no scope table row for %s, which adoscope.ScopesFor can request", s)
		}
	}
}
