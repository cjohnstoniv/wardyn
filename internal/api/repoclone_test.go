// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
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

	// gitBrokerKey itself must reject an ssh:// GitHub URL, not just the slug
	// wrapper. url.Parse reads ssh://git@github.com/o/r as Hostname()=="github.com",
	// so a hostname-only check brokers it — and a brokered run denies the forge AND
	// withholds the ssh_key (single-lane), while agent-run's insteadOf rewrite only
	// matches bare slugs. The run would end up with no working clone lane at all.
	for url, want := range map[string]string{
		"https://github.com/Octo/Repo.git": "octo/repo",
		"http://github.com/o/r":            "o/r",
		"ssh://git@github.com/o/r":         "",
		"ssh://git@github.com/o/r.git":     "",
		"ssh://github.com/o/r":             "",
	} {
		if k := gitBrokerKey(url); k != want {
			t.Errorf("gitBrokerKey(%q) = %q, want %q", url, k, want)
		}
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

// TestConfineGitBrokerEgress: a BROKERED run's effective egress carries no
// broker-managed GitHub host and no SSH lane to a brokered forge — dispatch
// subtracts both from the allowlist (exact, wildcard, and :port spellings) and
// denies them, so /wardyn/gh/ is the only path to those names and the receive-pack
// branch-namespace parser cannot be routed around by dialing one of them, nor by
// pushing over SSH. A NON-brokered run is untouched, SSH lane included.
func TestConfineGitBrokerEgress(t *testing.T) {
	grants := map[string]uuid.UUID{"octocat/hello-world": uuid.New()}

	brokered := types.RunPolicySpec{AllowedDomains: []string{
		"api.anthropic.com", "GitHub.com", "api.github.com:443",
		"raw.githubusercontent.com", "*.githubusercontent.com", "codeload.github.com.",
	}}
	if dropped := confineGitBrokerEgress(&brokered, grants); len(dropped) == 0 {
		t.Fatal("confineGitBrokerEgress reported nothing dropped for a brokered run")
	}
	for _, d := range brokered.AllowedDomains {
		if gitBrokerManaged(d) {
			t.Errorf("broker-managed host %q survived in the effective allowlist: %v", d, brokered.AllowedDomains)
		}
	}
	if len(brokered.AllowedDomains) != 1 || brokered.AllowedDomains[0] != "api.anthropic.com" {
		t.Errorf("unrelated egress must survive verbatim; got %v", brokered.AllowedDomains)
	}
	// The deny is the load-bearing half: it beats allow_all_egress too.
	for _, want := range gitBrokerManagedHosts {
		if !slices.Contains(brokered.DeniedDomains, want) {
			t.Errorf("denied_domains = %v, want it to contain %q", brokered.DeniedDomains, want)
		}
	}

	// BROKERED MEANS SINGLE-LANE — this assertion is the INVERSE of what it pinned
	// before: it used to require ssh.github.com:443 to SURVIVE the confinement, on
	// the reasoning that an ssh_key grant is operator-supplied and operator-bounded.
	// That left a brokered run with github.com:443 denied and ssh.github.com:443
	// allowed — a second push path the receive-pack branch-namespace parser cannot
	// read, because SSH is opaque to it. The lane is now closed here (and refused at
	// policy-write by validateGrantLaneExclusivity), so the brokered route is the
	// only route to the forge BY NAME — a name-based deny never binds an IP
	// literal, the standing caveat the four HTTPS denies already carry. The deny
	// here is the BARE host, which covers every port. If this
	// assertion ever flips BACK, docs/POLICIES.md and threatmodel/THREAT-MODEL.md
	// are wrong and must change with it.
	sshLane := types.RunPolicySpec{AllowedDomains: []string{"github.com", "ssh.github.com:443"}}
	confineGitBrokerEgress(&sshLane, grants)
	if slices.Contains(sshLane.AllowedDomains, "ssh.github.com:443") {
		t.Errorf("ssh.github.com:443 must NOT survive the confinement, got allowed=%v", sshLane.AllowedDomains)
	}
	if !slices.Contains(sshLane.DeniedDomains, "ssh.github.com") {
		t.Errorf("ssh.github.com must be denied by the git-broker confinement, got denied=%v", sshLane.DeniedDomains)
	}

	// THE REGRESSION THAT MATTERS: the SSH confinement fires ONLY for a brokered
	// run. An ssh_key run with no broker map keeps its lane untouched — nothing is
	// subtracted and nothing is denied.
	sshOnly := types.RunPolicySpec{AllowedDomains: []string{"ssh.github.com:443", "api.anthropic.com"}}
	if dropped := confineGitBrokerEgress(&sshOnly, nil); dropped != nil {
		t.Errorf("non-brokered ssh_key run must be untouched, dropped %v", dropped)
	}
	if !slices.Contains(sshOnly.AllowedDomains, "ssh.github.com:443") || len(sshOnly.DeniedDomains) != 0 {
		t.Errorf("non-brokered ssh_key run lost its SSH lane: allowed=%v denied=%v", sshOnly.AllowedDomains, sshOnly.DeniedDomains)
	}

	// NON-brokered run: no git grants, so github egress stays the operator's call.
	plain := types.RunPolicySpec{AllowedDomains: []string{"github.com", "api.anthropic.com"}}
	if dropped := confineGitBrokerEgress(&plain, nil); dropped != nil {
		t.Errorf("non-brokered run must be untouched, dropped %v", dropped)
	}
	if !slices.Equal(plain.AllowedDomains, []string{"github.com", "api.anthropic.com"}) || len(plain.DeniedDomains) != 0 {
		t.Errorf("non-brokered run mutated: allowed=%v denied=%v", plain.AllowedDomains, plain.DeniedDomains)
	}

	// THE DEDUP KEY IS THE RAW ENTRY, and this case is the only thing that says so.
	// A policy that already denies "github.com:443" must STILL get the bare-host
	// deny: keying the dedup map on egressEntryHost(d) instead looks like tidy-up
	// now that the helper exists, passes every other test in the repo, and leaves
	// github.com:22 reachable on a brokered run under allow_all_egress — where the
	// allowlist subtraction is worth nothing and the deny is the only half left.
	// Probed through the REAL evaluator, since the bug is a verdict, not a spelling.
	prePorted := types.RunPolicySpec{AllowAllEgress: true, DeniedDomains: []string{"github.com:443"}}
	confineGitBrokerEgress(&prePorted, grants)
	for _, port := range []int{22, 443} {
		v, err := proxy.NewBuiltinEvaluator(prePorted).EvaluateHost(context.Background(), egress.Request{Host: "github.com", Port: port})
		if err != nil {
			t.Fatalf("evaluate github.com:%d: %v", port, err)
		}
		if v != egress.VerdictDeny {
			t.Errorf("github.com:%d = %v, want deny (denied=%v)", port, v, prePorted.DeniedDomains)
		}
	}

	// Idempotent + no duplicate deny rows on a policy that already denies a host.
	twice := types.RunPolicySpec{AllowedDomains: []string{"github.com"}, DeniedDomains: []string{"github.com"}}
	confineGitBrokerEgress(&twice, grants)
	confineGitBrokerEgress(&twice, grants)
	seen := map[string]int{}
	for _, d := range twice.DeniedDomains {
		seen[d]++
	}
	for d, n := range seen {
		if n != 1 {
			t.Errorf("denied_domains has %q %d times, want 1: %v", d, n, twice.DeniedDomains)
		}
	}

	// The caller's backing array is never aliased: the source policy a second run
	// composes from must not see the first run's narrowing.
	shared := []string{"github.com", "api.anthropic.com"}
	src := types.RunPolicySpec{AllowedDomains: shared}
	confineGitBrokerEgress(&src, grants)
	if !slices.Equal(shared, []string{"github.com", "api.anthropic.com"}) {
		t.Errorf("caller's AllowedDomains array was mutated in place: %v", shared)
	}
}

// TestBuildRepoRecordsCanonicalisesGitHubURLs pins the D2 half the Sec review
// found incomplete. agent-run registers url.<broker>.insteadOf against
// "https://github.com/<org>/<repo>" and git PREFIX-matches the record's clone URL
// against it, so a full github URL must reach the sandbox in exactly that shape.
// A trailing slash used to survive into the slug (the shell's bare-slug regex
// then dropped the record entirely) and an http:// clone URL never prefix-matched
// the https insteadOf — both left the clone dialing github.com directly, a route
// a brokered run no longer has. A BARE slug is already canonical and must come
// through byte-identical, casing included.
func TestBuildRepoRecordsCanonicalisesGitHubURLs(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// bare slug: untouched, original casing preserved (git's insteadOf match is
		// case-SENSITIVE even though github itself is not). Trailing \t: the 4th
		// (ref) field, empty for the legacy no-ref single-repo path (W9-S1-3).
		{"octocat/Hello-World", "https://github.com/octocat/Hello-World.git\t/home/agent/work/Hello-World\toctocat/Hello-World\t"},
		// the two spellings the review found mismatching gitBrokerKeyFromSlug:
		{"https://github.com/octocat/hello-world/", "https://github.com/octocat/hello-world.git\t/home/agent/work/hello-world\toctocat/hello-world\t"},
		{"http://github.com/octocat/hello-world", "https://github.com/octocat/hello-world.git\t/home/agent/work/hello-world\toctocat/hello-world\t"},
		// already-canonical URL forms collapse to the same record.
		{"https://github.com/octocat/hello-world.git", "https://github.com/octocat/hello-world.git\t/home/agent/work/hello-world\toctocat/hello-world\t"},
		// A MIXED-CASE full URL lowercases, dest included (~/work/hello-world, not
		// ~/work/Hello-World). Deliberate — see the side-effect note in
		// buildRepoRecords: it is what lets the dest dedup below see two spellings
		// of one repo as one repo. Bare slugs, the common form, keep their case.
		{"https://github.com/Octocat/Hello-World", "https://github.com/octocat/hello-world.git\t/home/agent/work/hello-world\toctocat/hello-world\t"},
		// NOT github: left completely alone (its own lane, its own host allowlist).
		{"https://gitlab.com/o/r.git", "https://gitlab.com/o/r.git\t/home/agent/work/r\thttps://gitlab.com/o/r.git\t"},
	} {
		if got := buildRepoRecords(tc.in, nil); got != tc.want {
			t.Errorf("buildRepoRecords(%q) =\n  %q\nwant\n  %q", tc.in, got, tc.want)
		}
	}

	// A trailing-slash URL and its bare slug are the SAME repo: canonicalisation
	// makes the dest dedup see that, instead of cloning it twice into
	// ~/work/repo and ~/work/hello-world.
	both := buildRepoRecords("octocat/hello-world", []types.WorkspaceRepo{{Repo: "https://github.com/octocat/hello-world/"}})
	if strings.Contains(both, "\n") {
		t.Errorf("buildRepoRecords: the same repo in two spellings produced two records:\n%s", both)
	}
}

// TestBuildRepoRecordsCarriesRef is the W9-S1-3 regression: a WorkspaceRepo's
// Ref must ride as the record's 4th tab-separated field — clone_one
// (agent-run-lib.sh) is the only remaining consumer that can actually check it
// out, but it can't if buildRepoRecords never emits it in the first place.
func TestBuildRepoRecordsCarriesRef(t *testing.T) {
	got := buildRepoRecords("", []types.WorkspaceRepo{{Repo: "acme/payments", Ref: "release-2.0"}})
	want := "https://github.com/acme/payments.git\t/home/agent/work/payments\tacme/payments\trelease-2.0"
	if got != want {
		t.Errorf("buildRepoRecords with Ref =\n  %q\nwant\n  %q", got, want)
	}

	// No ref declared: the field is still present (empty) — the format is
	// fixed-arity so agent-run-lib.sh's `read` splits it the same way either way.
	got = buildRepoRecords("", []types.WorkspaceRepo{{Repo: "acme/payments"}})
	if strings.Count(got, "\t") != 3 {
		t.Errorf("buildRepoRecords with no Ref = %q, want exactly 3 tabs (4 fields, ref empty)", got)
	}

	// A ref containing a control character/whitespace must be rejected the same
	// way an unsafe slug or dest already is — never smuggled past the tab framing.
	got = buildRepoRecords("", []types.WorkspaceRepo{{Repo: "acme/payments", Ref: "evil\tref"}})
	if got != "" {
		t.Errorf("buildRepoRecords with an unsafe ref = %q, want \"\" (record dropped)", got)
	}
}
