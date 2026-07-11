// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/setup"
)

// scmProviderCheck grades against the safest-path ladder and must NEVER gate:
// ok only for the App (nothing safer to recommend), warn only for standing
// resident SSH keys, info everywhere else.
func TestScmProviderCheck_Grading(t *testing.T) {
	cases := []struct {
		name       string
		githubApp  bool
		secrets    []string
		posture    setup.SCMPosture
		wantStatus string
		wantInFix  string // "" = Fix must be empty or is unchecked
	}{
		{"app is ok", true, []string{"git-pat-github-com"}, setup.SCMPosture{}, "ok", ""},
		{"ssh key warns with upgrade fix", false, []string{"ssh-key-github-com"}, setup.SCMPosture{}, "warn", "deploy key"},
		{"pat only stays info", false, []string{"git-pat-github-com"}, setup.SCMPosture{}, "info", ""},
		{"loose posture info carries fix", false, nil, setup.SCMPosture{GhCLI: true}, "info", "fine-grained"},
		{"credential.helper store counts as loose", false, nil, setup.SCMPosture{CredentialHelper: "store --file=/tmp/x"}, "info", "fine-grained"},
		{"pristine host stays info", false, nil, setup.SCMPosture{}, "info", "git-pat-github-com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chk := scmProviderCheck(tc.githubApp, tc.secrets, tc.posture)
			if chk.ID != "scm_provider" {
				t.Fatalf("ID = %q", chk.ID)
			}
			if chk.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q (detail: %s)", chk.Status, tc.wantStatus, chk.Detail)
			}
			if tc.wantInFix != "" && !strings.Contains(chk.Fix, tc.wantInFix) {
				t.Errorf("Fix %q missing %q", chk.Fix, tc.wantInFix)
			}
		})
	}
}
