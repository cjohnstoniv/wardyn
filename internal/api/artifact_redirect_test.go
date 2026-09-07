// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//lint:file-ignore SA1019 TestFoldCompat_ArtifactOverridesGoldenBehavior
// deliberately constructs and reads the deprecated SiteConfig.ArtifactOverrides
// to prove foldLegacyArtifactOverrides produces byte-identical behavior to the
// pre-refactor shape — see internal/api/site_config.go's own file-scope ignore
// for why the field still exists.

package api

import (
	"reflect"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/hostrules"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestSubstituteArtifactEgress_CorpReplacesPublic: for a CONFIGURED ecosystem the
// public-registry hosts are dropped and the corp host is added; UNCONFIGURED
// langs are untouched; unrelated hosts survive; a network-only redirect drops
// exactly its own From host (no per-ecosystem table to consult).
func TestSubstituteArtifactEgress_CorpReplacesPublic(t *testing.T) {
	// npm + pip redirected; go left public. github.com is unrelated (kept).
	// Plus a network-only redirect (no Ecosystem) for an unrelated host pair.
	sc := types.SiteConfig{EgressRedirects: []types.EgressRedirect{
		{From: "https://registry.npmjs.org/", To: "https://artifactory.corp/api/npm/npm-remote/", Ecosystem: "npm"},
		{From: "https://pypi.org/simple/", To: "https://artifactory.corp/api/pypi/pypi-remote/simple", Ecosystem: "pip"},
		{From: "ghcr.io", To: "registry.corp.internal"},
	}}
	in := []string{
		"registry.npmjs.org",     // npm public -> dropped
		"pypi.org",               // pip public -> dropped
		"files.pythonhosted.org", // pip public -> dropped (whole ecosystem table, not just From)
		"proxy.golang.org",       // go public -> KEPT (unconfigured)
		"sum.golang.org",         // go public -> KEPT
		"github.com",             // unrelated -> KEPT
		"ghcr.io",                // network-only From -> dropped
	}
	got := substituteArtifactEgress(in, sc)
	gotSet := map[string]bool{}
	for _, d := range got {
		gotSet[d] = true
	}
	for _, dropped := range []string{"registry.npmjs.org", "pypi.org", "files.pythonhosted.org", "ghcr.io"} {
		if gotSet[dropped] {
			t.Errorf("host %q should have been dropped; got %v", dropped, got)
		}
	}
	for _, kept := range []string{"proxy.golang.org", "sum.golang.org", "github.com"} {
		if !gotSet[kept] {
			t.Errorf("host %q (unconfigured/unrelated) should have been kept; got %v", kept, got)
		}
	}
	if !gotSet["artifactory.corp"] {
		t.Errorf("corp host artifactory.corp should have been added; got %v", got)
	}
	if !gotSet["registry.corp.internal"] {
		t.Errorf("network-only To host registry.corp.internal should have been added; got %v", got)
	}
	// One corp host even though two ecosystems share it.
	corpCount := 0
	for _, d := range got {
		if d == "artifactory.corp" {
			corpCount++
		}
	}
	if corpCount != 1 {
		t.Errorf("corp host should appear exactly once (deduped), got %d", corpCount)
	}
}

// TestSubstituteArtifactEgress_NoOpAndFreshSlice: no redirects returns the input
// unchanged; a malformed From/To leaves that redirect's public host(s) in place
// (fail-safe); the returned slice never mutates the caller's backing array.
func TestSubstituteArtifactEgress_NoOpAndFreshSlice(t *testing.T) {
	in := []string{"registry.npmjs.org", "github.com"}
	if got := substituteArtifactEgress(in, types.SiteConfig{}); !reflect.DeepEqual(got, in) {
		t.Errorf("no redirects must be a no-op; got %v", got)
	}

	// Malformed To (no host): fail-safe, keep the public host, add nothing.
	sc := types.SiteConfig{EgressRedirects: []types.EgressRedirect{
		{From: "https://registry.npmjs.org/", To: "not a url", Ecosystem: "npm"},
	}}
	got := substituteArtifactEgress(in, sc)
	found := false
	for _, d := range got {
		if d == "registry.npmjs.org" {
			found = true
		}
	}
	if !found {
		t.Errorf("malformed to should leave npm public host in place; got %v", got)
	}

	// Malformed From on a network-only redirect: fail-safe, drop nothing.
	scNetwork := types.SiteConfig{EgressRedirects: []types.EgressRedirect{
		{From: "not a url", To: "https://corp.example/relay"},
	}}
	got2 := substituteArtifactEgress(in, scNetwork)
	if !reflect.DeepEqual(got2, in) {
		t.Errorf("malformed network-only from should drop nothing; got %v, want %v", got2, in)
	}

	// Backing-array safety: a real substitution must not clobber `in`.
	orig := append([]string(nil), in...)
	_ = substituteArtifactEgress(in, types.SiteConfig{EgressRedirects: []types.EgressRedirect{
		{From: "https://registry.npmjs.org/", To: "https://corp.example/npm/", Ecosystem: "npm"},
	}})
	if !reflect.DeepEqual(in, orig) {
		t.Errorf("substitution mutated the caller's slice: %v (was %v)", in, orig)
	}
}

// TestSubstituteArtifactEgress_ScopedToMatchingRun pins GAP-EGRESS-2: a redirect
// is applied ONLY when the run's own egress reaches one of the public hosts it
// fronts. An unrelated sealed run (naming neither the From host nor the ecosystem)
// is left entirely untouched — it does NOT gain the corp To host.
func TestSubstituteArtifactEgress_ScopedToMatchingRun(t *testing.T) {
	sc := types.SiteConfig{EgressRedirects: []types.EgressRedirect{
		{From: "https://registry.npmjs.org/", To: "https://artifactory.corp/api/npm/", Ecosystem: "npm"},
		{From: "ghcr.io", To: "registry.corp.internal"},
	}}
	// A run that names NONE of the fronted hosts: untouched, corp host NOT added.
	in := []string{"api.anthropic.com"}
	got := substituteArtifactEgress(in, sc)
	if !reflect.DeepEqual(got, in) {
		t.Errorf("a run naming none of a redirect's hosts must be untouched; got %v", got)
	}
	// A run that DOES name the ecosystem public host: redirect applies.
	got = substituteArtifactEgress([]string{"registry.npmjs.org"}, sc)
	set := map[string]bool{}
	for _, d := range got {
		set[d] = true
	}
	if set["registry.npmjs.org"] || !set["artifactory.corp"] {
		t.Errorf("a matching run should drop the public host and gain the corp host; got %v", got)
	}
}

// TestSubstituteArtifactEgress_WildcardAndPortDrop pins GAP-EGRESS-6: a wildcard
// or :port-qualified allowlist entry covering an ecosystem public host is dropped
// like a bare host, so public reach never survives beside the corp mirror.
func TestSubstituteArtifactEgress_WildcardAndPortDrop(t *testing.T) {
	sc := types.SiteConfig{EgressRedirects: []types.EgressRedirect{
		{From: "https://pypi.org/simple/", To: "https://pypi.corp.example/pypi/", Ecosystem: "pip"},
	}}
	in := []string{"*.pythonhosted.org", "pypi.org:443", "github.com"}
	got := substituteArtifactEgress(in, sc)
	for _, d := range got {
		if d == "*.pythonhosted.org" || d == "pypi.org:443" {
			t.Errorf("wildcard/port entry %q should have been dropped; got %v", d, got)
		}
	}
	set := map[string]bool{}
	for _, d := range got {
		set[d] = true
	}
	if !set["github.com"] || !set["pypi.corp.example"] {
		t.Errorf("unrelated host kept + corp added expected; got %v", got)
	}
}

// TestAppendNetworkRedirectDenials pins GAP-EGRESS-4: a network-only redirect's
// From host is added to DeniedDomains (deny beats allow-all), while an ecosystem
// row contributes no deny.
func TestAppendNetworkRedirectDenials(t *testing.T) {
	sc := types.SiteConfig{EgressRedirects: []types.EgressRedirect{
		{From: "ghcr.io", To: "registry.corp.internal"},                                  // network-only -> deny ghcr.io
		{From: "https://registry.npmjs.org/", To: "https://corp/npm/", Ecosystem: "npm"}, // ecosystem -> no deny
	}}
	got := appendNetworkRedirectDenials([]string{"existing.example"}, sc)
	set := map[string]bool{}
	for _, d := range got {
		set[d] = true
	}
	if !set["ghcr.io"] {
		t.Errorf("network-only From host ghcr.io must be denied; got %v", got)
	}
	if set["registry.npmjs.org"] {
		t.Errorf("an ecosystem row must not add a deny; got %v", got)
	}
	if !set["existing.example"] {
		t.Errorf("existing deny entries must be preserved; got %v", got)
	}
}

// TestRedirectPort is the W13-S1-5 producer-side unit test: planArtifactRedirect
// must carry a redirect's REAL port into plan.mitmHosts (net.JoinHostPort(host,
// redirectPort(r.To))) rather than always assuming 443, so the proxy's MITM
// dial (mitm.go) lands on the mirror the operator actually configured.
func TestRedirectPort(t *testing.T) {
	cases := []struct {
		name string
		to   string
		want int
	}{
		{"explicit non-443 port", "https://artifactory.corp:5000/api/npm/npm-remote/", 5000},
		{"no port defaults 443", "https://artifactory.corp/api/npm/npm-remote/", 443},
		{"bare host:port, no scheme or path", "artifactory.corp:8081", 8081},
		{"bare host, no scheme or path", "artifactory.corp", 443},
		{"explicit default port stays 443", "https://artifactory.corp:443/api/", 443},
	}
	for _, c := range cases {
		if got := redirectPort(c.to); got != c.want {
			t.Errorf("%s: redirectPort(%q) = %d, want %d", c.name, c.to, got, c.want)
		}
	}
}

// TestArtifactBaseURLs extracts ecosystem->base (URL-only) from the
// Ecosystem-tier subset of EgressRedirects, drops tokens, and SKIPS any
// network-only row (Ecosystem "").
func TestArtifactBaseURLs(t *testing.T) {
	sc := types.SiteConfig{EgressRedirects: []types.EgressRedirect{
		{From: "https://registry.npmjs.org/", To: "https://corp/npm/", TokenSecretRef: "npm-token", Ecosystem: "npm"},
		{From: "https://index.crates.io/", To: "https://corp/cargo/", Ecosystem: "cargo"},
		{From: "ghcr.io", To: "registry.corp.internal"}, // network-only: must not appear
	}}
	got := artifactBaseURLs(sc)
	want := map[string]string{"npm": "https://corp/npm/", "cargo": "https://corp/cargo/"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("artifactBaseURLs\n got: %v\nwant: %v", got, want)
	}
	if artifactBaseURLs(types.SiteConfig{}) != nil {
		t.Errorf("no redirects must yield nil")
	}
	onlyNetwork := types.SiteConfig{EgressRedirects: []types.EgressRedirect{{From: "ghcr.io", To: "registry.corp.internal"}}}
	if got := artifactBaseURLs(onlyNetwork); got != nil {
		t.Errorf("all-network-only redirects must yield nil, got %v", got)
	}
}

// TestArtifactRepoCheck reports info status + configured ecosystems.
func TestArtifactRepoCheck(t *testing.T) {
	// Unconfigured: info + fix hint.
	c := artifactRepoCheck(types.SiteConfig{})
	if c.ID != "artifact_repo" || c.Status != "info" || c.Fix == "" {
		t.Errorf("unconfigured artifact_repo check unexpected: %+v", c)
	}
	// Configured with one token: lists ecosystems + notes the token.
	c = artifactRepoCheck(types.SiteConfig{EgressRedirects: []types.EgressRedirect{
		{From: "https://registry.npmjs.org/", To: "https://corp/npm/", TokenSecretRef: "npm-token", Ecosystem: "npm"},
		{From: "https://repo.maven.apache.org/maven2/", To: "https://corp/maven", Ecosystem: "maven"},
	}})
	if c.Status != "info" {
		t.Errorf("configured artifact_repo status = %q, want info", c.Status)
	}
	// deterministic ecosystem list in detail
	for _, eco := range []string{"maven", "npm"} {
		if !strings.Contains(c.Detail, eco) {
			t.Errorf("detail should mention %q; got %q", eco, c.Detail)
		}
	}
}

// TestFoldCompat_ArtifactOverridesGoldenBehavior is the fold-compat golden the
// Wave A gate requires: an existing ecosystem-keyed config, folded into
// EgressRedirects (foldLegacyArtifactOverrides — the same fold PUT /site-config
// applies to a legacy body) must produce dispatch-relevant output IDENTICAL to
// the documented pre-refactor ArtifactOverrides behavior: the same emitted
// config files/env, the same egress substitution (including an ecosystem —
// go, pointed at ITS OWN host — that shares no host with anything else, so its
// drop/add is independently verifiable), and the same token injection for a
// HOST SHARED across ecosystems (artifactory.corp, fronting both npm+pip) —
// proving the "first sighted wins" dedup determinism survives the fold (npm
// sorts before pip, so npm's token wins either way).
func TestFoldCompat_ArtifactOverridesGoldenBehavior(t *testing.T) {
	legacy := types.SiteConfig{ArtifactOverrides: map[string]types.ArtifactOverride{
		"npm": {BaseURL: "https://artifactory.corp/api/npm/npm-remote/", TokenSecretRef: "npm-token"},
		"pip": {BaseURL: "https://artifactory.corp/api/pip/pip-remote/"},
		"go":  {BaseURL: "https://go-mirror.corp/repo"},
	}}

	folded := legacy
	if err := foldLegacyArtifactOverrides(&folded); err != nil {
		t.Fatalf("fold: %v", err)
	}
	if len(folded.ArtifactOverrides) != 0 {
		t.Fatalf("fold left ArtifactOverrides non-empty: %+v", folded.ArtifactOverrides)
	}
	if len(folded.EgressRedirects) != 3 {
		t.Fatalf("fold produced %d redirects, want 3: %+v", len(folded.EgressRedirects), folded.EgressRedirects)
	}

	// Same emitted config files/env the old BaseURL-keyed map produced —
	// EmitArtifactConfig itself is unchanged, only its input's SOURCE moved
	// from ArtifactOverrides to the Ecosystem-tier of EgressRedirects.
	wantBases := map[string]string{
		"npm": "https://artifactory.corp/api/npm/npm-remote/",
		"pip": "https://artifactory.corp/api/pip/pip-remote/",
		"go":  "https://go-mirror.corp/repo",
	}
	wantFiles, wantEnv := hostrules.EmitArtifactConfig(wantBases)
	gotFiles, gotEnv := hostrules.EmitArtifactConfig(artifactBaseURLs(folded))
	if !reflect.DeepEqual(wantFiles, gotFiles) {
		t.Errorf("emitted config files diverged after fold:\n want: %v\n got:  %v", wantFiles, gotFiles)
	}
	if !reflect.DeepEqual(wantEnv, gotEnv) {
		t.Errorf("emitted env diverged after fold:\n want: %v\n got:  %v", wantEnv, gotEnv)
	}

	// Same egress substitution: npm+pip+go are ALL configured, so ALL their
	// public hosts drop (the WHOLE per-ecosystem table, e.g. pip's file-CDN
	// host too); github.com is unrelated and stays; artifactory.corp is added
	// exactly once despite npm+pip sharing it, and go-mirror.corp once for go.
	in := []string{"registry.npmjs.org", "pypi.org", "files.pythonhosted.org", "proxy.golang.org", "sum.golang.org", "github.com"}
	got := substituteArtifactEgress(in, folded)
	gotSet := map[string]bool{}
	for _, d := range got {
		gotSet[d] = true
	}
	for _, dropped := range []string{"registry.npmjs.org", "pypi.org", "files.pythonhosted.org", "proxy.golang.org", "sum.golang.org"} {
		if gotSet[dropped] {
			t.Errorf("post-fold substitution: %q should be dropped (its ecosystem is configured); got %v", dropped, got)
		}
	}
	if !gotSet["github.com"] {
		t.Errorf("post-fold substitution: unrelated host github.com should be kept; got %v", got)
	}
	if !gotSet["go-mirror.corp"] {
		t.Errorf("post-fold substitution: go-mirror.corp should have been added; got %v", got)
	}
	corpCount := 0
	for _, d := range got {
		if d == "artifactory.corp" {
			corpCount++
		}
	}
	if corpCount != 1 {
		t.Errorf("post-fold substitution: artifactory.corp should appear exactly once, got %d in %v", corpCount, got)
	}

	// Same token injection for the shared host: planArtifactRedirect dedupes by
	// TO-host in EgressRedirects' STORED order (its seenHost loop), and the fold
	// emits ecosystems in key order — so npm (sorting before pip) is the row
	// whose token artifactory.corp injects, exactly the pre-refactor
	// map-iteration code's "first alphabetically" winner.
	first := ""
	for _, red := range folded.EgressRedirects {
		if strings.EqualFold(hostrules.HostOf(red.To), "artifactory.corp") {
			first = red.TokenSecretRef
			break
		}
	}
	if first != "npm-token" {
		t.Errorf("post-fold shared-host token = %q, want npm-token (first ecosystem alphabetically)", first)
	}
}

// TestPublicRegistryHostsCoverage: every ecosystem the substitution supports has
// public hosts to drop; junk yields nil.
func TestPublicRegistryHostsCoverage(t *testing.T) {
	for _, e := range []string{"npm", "pip", "go", "cargo", "maven", "nuget"} {
		if len(hostrules.PublicRegistryHosts(e)) == 0 {
			t.Errorf("ecosystem %q has no public hosts", e)
		}
	}
	if hostrules.PublicRegistryHosts("bogus") != nil {
		t.Errorf("unknown ecosystem must return nil")
	}
}

// TestRedirectPort_AuthorityAndSchemeShapes pins the two things TestRedirectPort
// above does not exercise, both of which reached the MITM/token-injection set
// and the redirect probe from ordinary operator input that validateSiteConfig
// accepts:
//
//   - where the AUTHORITY ends. A query or fragment terminates it exactly as
//     '/' does; cutting only at '/' handed strconv "8443?repo=npm" and fell
//     back to 443, so plan.mitmHosts carried host:443 for a mirror on 8443 —
//     the proxy's mitmPorts gate then withheld the operator's token on the port
//     the redirect names and presented it on one the operator never configured.
//   - which DEFAULT the scheme names. 443 is the default of https and of a bare
//     host (both are reached through a CONNECT the proxy TLS-terminates); an
//     explicit http:// names 80, and calling it 443 made the probe dial a port
//     the plain-http mirror does not serve.
//
// Expected ports are written literally, never derived from redirectPort.
func TestRedirectPort_AuthorityAndSchemeShapes(t *testing.T) {
	cases := []struct {
		name string
		to   string
		want int
	}{
		{"query ends the authority", "https://mirror.corp.example:8443?repo=npm-remote", 8443},
		{"fragment ends the authority", "https://mirror.corp.example:8443#frag", 8443},
		{"query on a literal-IP To", "https://10.40.2.11:8443?x=1", 8443},
		{"query with no port keeps the https default", "https://mirror.corp.example?repo=npm", 443},
		{"an explicit http:// with no port is 80", "http://10.40.1.5", 80},
		{"an explicit http:// with a port keeps it", "http://10.40.1.5:8080", 8080},
		{"https with no port stays 443", "https://mirror.corp.example", 443},
		{"a bare host stays 443", "mirror.corp.example", 443},
		{"a bare host:port keeps its port", "mirror.corp.example:8443", 8443},
	}
	for _, c := range cases {
		if got := redirectPort(c.to); got != c.want {
			t.Errorf("%s: redirectPort(%q) = %d, want %d", c.name, c.to, got, c.want)
		}
	}
}

// TestRedirectEndpointPort_SpelledButUnusable is the write-time half of the same
// parser: a port that IS spelled and is not a decimal 1-65535 must be reported
// as unusable, so validateSiteConfig can refuse it instead of letting a
// downstream reader coerce it to 443.
func TestRedirectEndpointPort_SpelledButUnusable(t *testing.T) {
	for _, raw := range []string{"10.40.2.11:0", "10.40.2.11:99999", "10.40.2.11:-1",
		"mirror.corp:8443:9", "mirror.corp:80x"} {
		if _, spelled, ok := redirectEndpointPort(raw); ok || !spelled {
			t.Errorf("redirectEndpointPort(%q) = (spelled=%v, ok=%v), want a spelled-but-unusable port", raw, spelled, ok)
		}
	}
	for _, raw := range []string{"mirror.corp", "https://mirror.corp/path", "https://mirror.corp?q=1"} {
		if _, spelled, ok := redirectEndpointPort(raw); !ok || spelled {
			t.Errorf("redirectEndpointPort(%q) = (spelled=%v, ok=%v), want no port spelled", raw, spelled, ok)
		}
	}
}
