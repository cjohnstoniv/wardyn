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
// RBAC + SSO run on the same compose control plane (see docs/OPERATIONS.md
// §Multi-user + deploy/compose/README.md) — the copy must say so instead of
// implying multi-user access control isn't available at all. Both front-door
// files are checked.
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

// TestMaskRegistryDocsNameTheResiduals guards docs/OPERATIONS.md and
// threatmodel/THREAT-MODEL.md against describing the shared masking registry as
// closing more than it does. Since 0.8.6 a registry miss fails closed for runs
// the manifest covers; what stays open is a run that predates 0.8.6 (refused
// after a restart, not masked) and the SSH paths that were never masked, and
// both documents must say so.
func TestMaskRegistryDocsNameTheResiduals(t *testing.T) {
	root := repoRoot(t)

	for _, tc := range []struct {
		rel  string
		want []string
	}{
		{filepath.Join("docs", "OPERATIONS.md"), []string{"Runs that predate 0.8.6 have no masking manifest", "SSH exec, SFTP and direct-tcpip were never masked"}},
		{filepath.Join("threatmodel", "THREAT-MODEL.md"), []string{"Runs that predate 0.8.6 have no manifest", "SSH exec, SFTP and direct-tcpip were never masked"}},
	} {
		body, err := os.ReadFile(filepath.Join(root, tc.rel))
		if err != nil {
			t.Fatalf("read %s: %v", tc.rel, err)
		}
		for _, want := range tc.want {
			if !strings.Contains(string(body), want) {
				t.Errorf("%s: the masking residuals must name every case the shared registry leaves open: missing %q", tc.rel, want)
			}
		}
	}
}
