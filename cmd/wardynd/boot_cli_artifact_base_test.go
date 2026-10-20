// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseServeKnobsCLIArtifactBase: boot refuses each malformed
// WARDYN_CLI_ARTIFACT_BASE naming the setting, and carries a good one through.
func TestParseServeKnobsCLIArtifactBase(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"http", "http://mirror.example"},
		{"user name", "https://user@mirror.example"},
		{"user name and password", "https://user:pass@mirror.example"},
		{"query", "https://mirror.example?x=1"},
		{"fragment", "https://mirror.example#x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseServeKnobs(serveKnobFlags(t, tc.value))
			if err == nil || !strings.Contains(err.Error(), "WARDYN_CLI_ARTIFACT_BASE") {
				t.Fatalf("parseServeKnobs(%q) = %v, want a refusal naming WARDYN_CLI_ARTIFACT_BASE", tc.value, err)
			}
		})
	}
	k, err := parseServeKnobs(serveKnobFlags(t, "https://mirror.example/wardyn/"))
	if err != nil || k.cliArtifactBase != "https://mirror.example/wardyn" {
		t.Fatalf("good value: got (%q, %v)", k.cliArtifactBase, err)
	}
}

func serveKnobFlags(t *testing.T, cliArtifactBase string) *bootFlags {
	t.Helper()
	policy := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(policy, []byte(`{"min_confinement_class":"CC1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	empty, no := "", false
	return &bootFlags{
		policyPath: &policy, siteConfigSeedFile: &empty, demoVideoBaseURL: &empty,
		trustedProxies: &empty, governAdminRunsExempt: &empty, governAdminRuns: &no,
		cliArtifactBase: &cliArtifactBase,
	}
}
