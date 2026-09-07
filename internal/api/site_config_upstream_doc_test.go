// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"os"
	"strings"
	"testing"
)

// TestOperationsUpstreamProxyCheckNamesTheLaneThatIsCheckedWhere (F003,
// adversarial fix-up round 3) pins the runbook's upstream-proxy paragraph to
// WHERE each of the two lanes is actually checked.
//
// The paragraph the round-1 fix added opened "Either way, the URL is checked at
// the write" — directly under the `wardyn secret set upstream-proxy-url` recipe,
// so "either way" claimed the write-time check covers the secret lane too. It
// does not, and the same diff's own code comment says so
// (loadableUpstreamProxyURL: "a URL that arrived through the SECRET lane, which
// no write-time validator can see inside"). validateSiteConfig only ever sees
// cfg.UpstreamProxyURL; PUT stores the secret's NAME and never its value, so an
// operator pasting a bad-port URL into the secret got `200 OK` at every write
// and a silently un-upstreamed run later.
//
// Both facts are read off the code here, so the prose is re-derived rather than
// left to rot if either lane moves.
func TestOperationsUpstreamProxyCheckNamesTheLaneThatIsCheckedWhere(t *testing.T) {
	// (1) The write-time validator sees the PLAIN url and nothing else: it never
	// reaches a secret store, so it cannot check a secret-held URL's value.
	validate := upstreamDocFuncBody(t, upstreamDocSrc(t, "site_config.go"), "func validateSiteConfig(")
	if !strings.Contains(validate, "proxy.ValidUpstreamProxyURL(cfg.UpstreamProxyURL)") {
		t.Fatalf("validateSiteConfig no longer delegates the plain URL to the sidecar's loader — re-derive the runbook's upstream-proxy paragraph before trusting this guard")
	}
	if !strings.Contains(validate, "validSecretRef(cfg.UpstreamProxySecretRef)") {
		t.Fatalf("validateSiteConfig no longer checks the secret REF's spelling — re-derive the runbook's upstream-proxy paragraph before trusting this guard")
	}
	// The ref is checked as a NAME; nothing here fetches what it holds.
	for _, secretRead := range []string{"Secrets", "getSecret", "Get(ctx"} {
		if strings.Contains(validate, secretRead) {
			t.Fatalf("validateSiteConfig now reads %q: if the write can see inside the secret, the runbook's split between write-time and dispatch-time checking is stale", secretRead)
		}
	}

	// (2) The secret lane's only check is at DISPATCH, and it degrades to direct
	// egress with an audited reason rather than failing the run.
	bedrock := upstreamDocSrc(t, "runs_bedrock.go")
	for _, want := range []string{"func loadableUpstreamProxyURL(", `"unloadable-upstream-url"`} {
		if !strings.Contains(bedrock, want) {
			t.Fatalf("runs_bedrock.go no longer contains %q — the dispatch-time drop the runbook names has changed shape", want)
		}
	}
	resolve := upstreamDocFuncBody(t, upstreamDocSrc(t, "runs_dispatch_mounts.go"), "func (s *Server) resolveRunUpstreamProxy(")
	for _, want := range []string{`"run.upstream_proxy.resolve"`, `detail["reason"] = failReason`, `return ""`} {
		if !strings.Contains(resolve, want) {
			t.Fatalf("resolveRunUpstreamProxy no longer contains %q — the audited fallback the runbook names has changed shape", want)
		}
	}

	// (3) The runbook states the split, and no longer claims the write covers
	// both paths.
	doc := upstreamDocRunbook(t)
	for _, want := range []string{
		"Both paths are checked by the **proxy sidecar's own loader**",
		"`upstream_proxy_url` is checked at the write",
		"A URL held in a secret is checked at\nDISPATCH instead, because no write-time validator can see inside the secret",
		"`unloadable-upstream-url`, a\n`run.upstream_proxy.resolve` failure",
		"the run falls back to direct egress",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/OPERATIONS.md's upstream-proxy paragraph no longer states: %q", want)
		}
	}
	if strings.Contains(doc, "Either way, the URL is checked at the write") {
		t.Error("docs/OPERATIONS.md is back to claiming the write-time check covers the secret lane too — it never has: validateSiteConfig sees only the plain URL, and a bad secret-held URL is dropped at dispatch")
	}
}

// upstreamDocSrc reads a file from this package's own directory.
func upstreamDocSrc(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// upstreamDocRunbook reads the runbook from this package's own directory, the
// same relative path internal/api's other doc guards use.
func upstreamDocRunbook(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../docs/OPERATIONS.md")
	if err != nil {
		t.Fatalf("read docs/OPERATIONS.md: %v", err)
	}
	return string(b)
}

// upstreamDocFuncBody returns the source text from decl's line to the closing
// brace in column 0 that ends it — enough to assert what a function does and
// does not reach for.
func upstreamDocFuncBody(t *testing.T, src, decl string) string {
	t.Helper()
	i := strings.Index(src, decl)
	if i < 0 {
		t.Fatalf("no declaration %q in source — re-derive this guard", decl)
	}
	rest := src[i:]
	if j := strings.Index(rest, "\n}\n"); j > 0 {
		return rest[:j]
	}
	return rest
}
