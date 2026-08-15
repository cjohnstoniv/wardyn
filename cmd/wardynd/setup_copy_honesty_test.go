// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSetupScriptTeamModeCopyIsHonest guards against scripts/setup.sh and
// Makefile claiming team mode (multi-user) "does not exist" outright. A
// packaged one-command team installer really doesn't exist, but admin/member
// RBAC + SSO shipped in v0.5 on the same compose control plane (see
// docs/OPERATIONS.md §Multi-user + deploy/compose/README.md) — the copy must
// say so instead of implying multi-user access control isn't available at
// all. Regression for W1-S1-1 (the honesty pass at bdee7d7 missed these two
// front-door files).
func TestSetupScriptTeamModeCopyIsHonest(t *testing.T) {
	root := repoRoot(t)

	setupSh, err := os.ReadFile(filepath.Join(root, "scripts", "setup.sh"))
	if err != nil {
		t.Fatalf("read scripts/setup.sh: %v", err)
	}
	makefile, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}

	for _, tc := range []struct {
		name string
		body string
	}{
		{"scripts/setup.sh", string(setupSh)},
		{"Makefile", string(makefile)},
	} {
		if strings.Contains(tc.body, "does not exist yet and is not scheduled") {
			t.Errorf("%s: still claims team mode outright does not exist / is not scheduled — "+
				"admin/member RBAC + SSO shipped in v0.5, only a packaged one-command team "+
				"installer is missing", tc.name)
		}
		if !strings.Contains(tc.body, "admin/member") {
			t.Errorf("%s: missing the admin/member RBAC callout for team mode", tc.name)
		}
		if !strings.Contains(tc.body, "v0.5") {
			t.Errorf("%s: missing the 'shipped in v0.5' callout for admin/member RBAC + SSO", tc.name)
		}
	}
}
