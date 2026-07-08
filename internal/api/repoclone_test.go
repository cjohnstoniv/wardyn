// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "testing"

// TestRepoFieldSafe locks in the sanitization for the attacker-influenceable
// run.Repo before it can flow into the in-sandbox `git clone`. Anything with a
// control character or whitespace must be rejected (fail closed).
func TestRepoFieldSafe(t *testing.T) {
	safe := []string{
		"octocat/Hello-World",
		"https://github.com/octocat/Hello-World.git",
		"https://example.com/group/sub/proj.git",
		"a/b",
	}
	for _, s := range safe {
		if !repoFieldSafe(s) {
			t.Errorf("repoFieldSafe(%q) = false, want true", s)
		}
	}

	unsafe := []string{
		"octocat/Hello World",    // ASCII space
		"octocat/Hello\tWorld",   // tab
		"octocat/Hello\nWorld",   // newline
		"octocat/Hello\rWorld",   // carriage return
		"octocat/Hello\x00World", // NUL
		"octocat/Repo name",      // non-breaking space (Unicode whitespace)
		"octocat/Reponame",      // NEL (C1 control)
		"octocat/Repo\x1bname",   // ESC (C0 control)
		"octocat/Repo name",      // ASCII space (would break the git argv)
	}
	for _, s := range unsafe {
		if repoFieldSafe(s) {
			t.Errorf("repoFieldSafe(%q) = true, want false", s)
		}
	}
}

// TestRepoCloneURL verifies the v0.1 GitHub-only derivation: a bare org/name
// becomes an https GitHub clone URL, an explicit URL passes through unchanged,
// and anything else yields "" (slug-only, no clone).
func TestRepoCloneURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"octocat/Hello-World", "https://github.com/octocat/Hello-World.git"},
		{"a/b", "https://github.com/a/b.git"},
		{"https://github.com/octocat/Hello-World.git", "https://github.com/octocat/Hello-World.git"},
		{"http://example.com/x/y.git", "http://example.com/x/y.git"}, // http(s) passes through
		{"git+ssh://host/x/y", ""},                                   // non-http(s) scheme: fail closed
		{"ssh://git@host/x/y", ""},                                   // ssh: fail closed
		{"file:///etc/passwd", ""},                                   // file: fail closed
		{"ext::sh -c whoami", ""},                                    // git helper transport: no "://", not a bare slug
		{"justname", ""},                                             // no separator
		{"a/b/c", ""},                                                // three segments, not a bare slug
		{"/leading", ""},                                             // empty first segment
		{"trailing/", ""},                                            // empty second segment
		{"", ""},                                                     // empty
	}
	for _, c := range cases {
		if got := repoCloneURL(c.in); got != c.want {
			t.Errorf("repoCloneURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
