// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

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
		"octocat/Repo\u0085name", // NEL (C1 control)
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
		{"http://example.com/x/y.git", "http://example.com/x/y.git"},           // http(s) passes through
		{"https://user@github.com/o/r.git", "https://user@github.com/o/r.git"}, // http(s) with user: unchanged
		// SSH to a SUPPORTED SSH-over-443 provider passes through VERBATIM (github + ADO).
		{"ssh://git@github.com/octocat/Hello-World.git", "ssh://git@github.com/octocat/Hello-World.git"},
		{"git@github.com:octocat/Hello-World.git", "git@github.com:octocat/Hello-World.git"}, // scp-form
		// ADO SSH: a 4-segment v3/org/proj/repo path — passthrough proves twoSegments is
		// NOT on the path (it would reject this).
		{"git@ssh.dev.azure.com:v3/org/proj/repo", "git@ssh.dev.azure.com:v3/org/proj/repo"},
		{"ssh://git@ssh.github.com/o/r.git", "ssh://git@ssh.github.com/o/r.git"}, // ssh.<host> form ok
		// SSH fail-closed boundary:
		{"ssh://git@github.com:22/o/r.git", ""}, // explicit non-443 port defeats the 443 lane: fail closed
		{"ssh://git@gitlab.com/o/r.git", ""},    // unsupported provider: fail closed
		{"git@evil.com:o/r", ""},                // scp-form to an unsupported host: fail closed
		{"ssh://git@host/x/y", ""},              // ssh to an unsupported host: fail closed
		{"git+ssh://host/x/y", ""},              // non-http(s)/ssh scheme: fail closed
		{"file:///etc/passwd", ""},              // file: fail closed
		{"ext::sh -c whoami", ""},               // git helper transport: no "://", not scp/bare
		{"justname", ""},                        // no separator
		{"a/b/c", ""},                           // three segments, not a bare slug
		{"/leading", ""},                        // empty first segment
		{"trailing/", ""},                       // empty second segment
		{"", ""},                                // empty
	}
	for _, c := range cases {
		if got := repoCloneURL(c.in); got != c.want {
			t.Errorf("repoCloneURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestScanEgressDomains_SSH locks in that an SSH clone URL adds the PORT-QUALIFIED
// SSH-over-443 endpoint (matches ONLY :443) and never the bare SSH host (which
// would match any port) — the egress half of the SSH lane.
func TestScanEgressDomains_SSH(t *testing.T) {
	gh := scanEgressDomains("ssh://git@github.com/o/r.git")
	if !slices.Contains(gh, "ssh.github.com:443") {
		t.Errorf("github ssh: missing ssh.github.com:443, got %v", gh)
	}
	if slices.Contains(gh, "ssh.github.com") {
		t.Errorf("github ssh: bare ssh.github.com must NOT be allowlisted (any-port), got %v", gh)
	}

	// An SSH clone needs ONLY the :443 endpoint (the dev.azure.com REST bundle is
	// the git_pat HTTPS lane, not this one) — least privilege, matches the direct-run
	// sshEgress lane.
	ado := scanEgressDomains("git@ssh.dev.azure.com:v3/org/proj/repo")
	if !slices.Contains(ado, "ssh.dev.azure.com:443") {
		t.Errorf("ado ssh: missing ssh.dev.azure.com:443, got %v", ado)
	}
	if slices.Contains(ado, "ssh.dev.azure.com") {
		t.Errorf("ado ssh: bare ssh.dev.azure.com must NOT be allowlisted (any-port), got %v", ado)
	}

	// A plain https URL still just adds its bare host (unchanged behavior).
	https := scanEgressDomains("https://example.com/x/y.git")
	if !slices.Contains(https, "example.com") {
		t.Errorf("https: missing example.com, got %v", https)
	}

	// Option C: a GitHub HTTPS clone is routed through the git-broker, so github.com
	// (and the rest of the bundle) is NOT in the scan/verify egress allowlist.
	ghHTTPS := scanEgressDomains("https://github.com/octocat/Hello-World.git")
	for _, banned := range []string{"github.com", "api.github.com", "codeload.github.com", "*.githubusercontent.com"} {
		if slices.Contains(ghHTTPS, banned) {
			t.Errorf("github https: %q must NOT be allowlisted (broker-managed), got %v", banned, ghHTTPS)
		}
	}
}

// TestGitBrokerWiring locks in the pure logic that maps a run's github grants +
// declared clone set into the proxy's per-repo git-broker allowlist.
func TestGitBrokerWiring(t *testing.T) {
	// githubScopeRepos: only well-formed "<org>/<repo>" survive.
	got := githubScopeRepos([]byte(`{"repos":["a/b","junk","c/d/e",""]}`))
	if !slices.Equal(got, []string{"a/b"}) {
		t.Errorf("githubScopeRepos = %v, want [a/b] (drop malformed/deep entries)", got)
	}

	// gitBrokerKeyFromSlug: bare slug + https github -> lowercased key; ssh/non-github -> "".
	for slug, want := range map[string]string{
		"octocat/Hello-World":              "octocat/hello-world",
		"https://github.com/Octo/Repo.git": "octo/repo",
		"git@github.com:o/r.git":           "",
		"ssh://git@github.com/o/r":         "",
		"https://gitlab.com/o/r.git":       "",
		"not-a-slug":                       "",
	} {
		if k := gitBrokerKeyFromSlug(slug); k != want {
			t.Errorf("gitBrokerKeyFromSlug(%q) = %q, want %q", slug, k, want)
		}
	}

	// augmentGitBrokerGrants: the declared clone set maps to the github grant, but
	// only when a grant exists; explicit scope entries are not overwritten.
	gid := uuid.New()
	gw := grantWiring{gitGrants: map[string]uuid.UUID{}, firstGitHubGrantID: &gid}
	gw.augmentGitBrokerGrants("octocat/Hello-World", []types.WorkspaceRepo{{Repo: "org/tool"}})
	if gw.gitGrants["octocat/hello-world"] != gid || gw.gitGrants["org/tool"] != gid {
		t.Errorf("augment: clone set not mapped to the grant: %v", gw.gitGrants)
	}
	// No github grant -> no augmentation (uncovered github repos stay denied).
	none := grantWiring{gitGrants: map[string]uuid.UUID{}}
	none.augmentGitBrokerGrants("octocat/Hello-World", nil)
	if len(none.gitGrants) != 0 {
		t.Errorf("augment with no grant should be a no-op, got %v", none.gitGrants)
	}
}
