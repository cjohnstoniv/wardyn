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

// An empty host-proxy detection means two very different things depending on
// where the detector ran. A containerized wardynd with no host-side seed is
// structurally blind to every tier (shell profiles, git, tool configs, OS/PAC),
// so it must say "couldn't look there" — asserting "nothing is there" is the
// false negative that undermines the corporate-network checklist.
func TestHostProxyCheck_BlindContainerIsHonest(t *testing.T) {
	const hostCopy = "No host-side proxy configuration detected"

	onHost := hostProxyCheck(setup.HostProxyDetection{}, false)
	blind := hostProxyCheck(setup.HostProxyDetection{}, true)

	for _, chk := range []SetupCheck{onHost, blind} {
		if chk.ID != "host_proxy" {
			t.Fatalf("ID = %q, want host_proxy", chk.ID)
		}
		// Detection never gates setup, in either posture.
		if chk.Status != "info" {
			t.Errorf("status = %q, want info", chk.Status)
		}
	}

	if !strings.Contains(onHost.Detail, hostCopy) {
		t.Errorf("host-mode detail = %q, want the plain %q", onHost.Detail, hostCopy)
	}
	if strings.Contains(blind.Detail, hostCopy) {
		t.Errorf("containerized detail asserts a false negative: %q", blind.Detail)
	}
	if !strings.Contains(blind.Detail, "container") {
		t.Errorf("containerized detail should name where it looked: %q", blind.Detail)
	}
	if blind.Fix == "" {
		t.Error("containerized check must carry a Fix — a blind result needs a next step")
	}
}

// A non-empty detection is posture-independent: once something was actually
// found, the "couldn't look there" caveat would be wrong.
func TestHostProxyCheck_FoundIsPostureIndependent(t *testing.T) {
	det := setup.HostProxyDetection{
		HTTPProxy: &setup.HostProxySetting{Value: "http://corp.proxy:8080", Source: setup.ProxySourceEnv},
	}
	onHost := hostProxyCheck(det, false)
	seeded := hostProxyCheck(det, true)

	if onHost.Detail != seeded.Detail {
		t.Errorf("detail differs when something WAS found:\n host: %q\n blind: %q", onHost.Detail, seeded.Detail)
	}
	if !strings.Contains(onHost.Detail, "Detected") {
		t.Errorf("detail = %q, want it to report the detection", onHost.Detail)
	}
}
