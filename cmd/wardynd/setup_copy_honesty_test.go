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

// TestMaskRegistryFailOpenDocsNameSingleReplicaRestart guards against
// docs/OPERATIONS.md and threatmodel/THREAT-MODEL.md bounding the
// secretmask.Registry fail-open (buildMaskingBody / liveMaskWriter passing an
// upload through unmasked when Snapshot(runID) is empty) to a multi-replica
// risk only. The registry is wiped by ANY process restart, so a single
// `replicas: 1` wardynd that restarts mid-run (upgrade, crash) reproduces the
// identical empty-snapshot fail-open — the docs must say so, not imply
// `replicas: 1` alone makes the gap inert. Regression for W21-S1-2.
func TestMaskRegistryFailOpenDocsNameSingleReplicaRestart(t *testing.T) {
	root := repoRoot(t)

	for _, tc := range []struct {
		rel  string
		want string
	}{
		{filepath.Join("docs", "OPERATIONS.md"), "not bounded to two replicas"},
		{filepath.Join("threatmodel", "THREAT-MODEL.md"), "single-process case is not inert"},
	} {
		body, err := os.ReadFile(filepath.Join(root, tc.rel))
		if err != nil {
			t.Fatalf("read %s: %v", tc.rel, err)
		}
		if !strings.Contains(string(body), tc.want) {
			t.Errorf("%s: mask-registry fail-open discussion must name the single-replica "+
				"restart case (a wardynd restart wipes the in-memory registry the same way "+
				"a second replica does), not bound the risk to replicas>1 — missing %q", tc.rel, tc.want)
		}
	}
}
