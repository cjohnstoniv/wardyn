// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package envbuild

import (
	"strings"
	"testing"
)

// Regression for the envbuild SSRF/RCE finding: caller-supplied git URLs/refs
// flow into envbuilder (ENVBUILDER_GIT_URL/REF). Only https://git:// remote
// clones are allowed; file://, ssh://, scp-like, and ext:: transports — and any
// control chars / leading-dash refs — must be rejected.
func TestValidateBuildInput(t *testing.T) {
	cases := []struct {
		name    string
		spec    BuildSpec
		wantErr bool
	}{
		{"https ok", BuildSpec{RepoURL: "https://github.com/example/repo", OutputImageTag: "x"}, false},
		{"git ok", BuildSpec{RepoURL: "git://example.com/repo.git", OutputImageTag: "x"}, false},
		{"https with ref ok", BuildSpec{RepoURL: "https://github.com/example/repo", Ref: "main", OutputImageTag: "x"}, false},
		{"empty url", BuildSpec{RepoURL: "", OutputImageTag: "x"}, true},
		{"file scheme", BuildSpec{RepoURL: "file:///etc/passwd", OutputImageTag: "x"}, true},
		{"ssh scheme", BuildSpec{RepoURL: "ssh://git@example.com/repo", OutputImageTag: "x"}, true},
		{"scp-like ssh", BuildSpec{RepoURL: "git@example.com:repo.git", OutputImageTag: "x"}, true},
		{"ext transport helper", BuildSpec{RepoURL: "ext::sh -c touch${IFS}/tmp/pwn", OutputImageTag: "x"}, true},
		{"local path", BuildSpec{RepoURL: "/srv/repo", OutputImageTag: "x"}, true},
		{"url with newline", BuildSpec{RepoURL: "https://github.com/example/repo\nfoo", OutputImageTag: "x"}, true},
		{"leading-dash ref", BuildSpec{RepoURL: "https://github.com/example/repo", Ref: "--upload-pack=evil", OutputImageTag: "x"}, true},
		{"ref with control char", BuildSpec{RepoURL: "https://github.com/example/repo", Ref: "ma\x00in", OutputImageTag: "x"}, true},

		// Devcontainer path must stay inside the cloned repo.
		{"devcontainer path ok", BuildSpec{RepoURL: "https://github.com/example/repo", DevcontainerPath: ".devcontainer/devcontainer.json", OutputImageTag: "x"}, false},
		{"devcontainer traversal", BuildSpec{RepoURL: "https://github.com/example/repo", DevcontainerPath: "../../etc/passwd", OutputImageTag: "x"}, true},
		{"devcontainer absolute", BuildSpec{RepoURL: "https://github.com/example/repo", DevcontainerPath: "/etc/passwd", OutputImageTag: "x"}, true},
		{"devcontainer dotdot segment", BuildSpec{RepoURL: "https://github.com/example/repo", DevcontainerPath: "a/../../b", OutputImageTag: "x"}, true},
		{"devcontainer backslash", BuildSpec{RepoURL: "https://github.com/example/repo", DevcontainerPath: "a\\b", OutputImageTag: "x"}, true},
		{"devcontainer leading dash", BuildSpec{RepoURL: "https://github.com/example/repo", DevcontainerPath: "-rf", OutputImageTag: "x"}, true},

		// Input-length bound.
		{"over-long repo url", BuildSpec{RepoURL: "https://example.com/" + strings.Repeat("a", maxBuildInputLen), OutputImageTag: "x"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBuildInput(tc.spec)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for %q (ref %q), got nil", tc.spec.RepoURL, tc.spec.Ref)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.spec.RepoURL, err)
			}
		})
	}
}
