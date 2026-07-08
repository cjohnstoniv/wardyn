// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"reflect"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// TestSubstituteArtifactEgress_CorpReplacesPublic: for a CONFIGURED ecosystem the
// public-registry hosts are dropped and the corp host is added; UNCONFIGURED
// langs are untouched; unrelated hosts survive.
func TestSubstituteArtifactEgress_CorpReplacesPublic(t *testing.T) {
	// npm + pip redirected; go left public. github.com is unrelated (kept).
	sc := types.SiteConfig{ArtifactOverrides: map[string]types.ArtifactOverride{
		"npm": {BaseURL: "https://artifactory.corp/api/npm/npm-remote/"},
		"pip": {BaseURL: "https://artifactory.corp/api/pypi/pypi-remote/simple"},
	}}
	in := []string{
		"registry.npmjs.org",     // npm public -> dropped
		"pypi.org",               // pip public -> dropped
		"files.pythonhosted.org", // pip public -> dropped
		"proxy.golang.org",       // go public -> KEPT (unconfigured)
		"sum.golang.org",         // go public -> KEPT
		"github.com",             // unrelated -> KEPT
	}
	got := substituteArtifactEgress(in, sc)
	gotSet := map[string]bool{}
	for _, d := range got {
		gotSet[d] = true
	}
	for _, dropped := range []string{"registry.npmjs.org", "pypi.org", "files.pythonhosted.org"} {
		if gotSet[dropped] {
			t.Errorf("public host %q should have been dropped; got %v", dropped, got)
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

// TestSubstituteArtifactEgress_NoOpAndFreshSlice: no overrides returns the input
// unchanged; a malformed base URL leaves that ecosystem's public hosts in place
// (fail-safe); the returned slice never mutates the caller's backing array.
func TestSubstituteArtifactEgress_NoOpAndFreshSlice(t *testing.T) {
	in := []string{"registry.npmjs.org", "github.com"}
	if got := substituteArtifactEgress(in, types.SiteConfig{}); !reflect.DeepEqual(got, in) {
		t.Errorf("no overrides must be a no-op; got %v", got)
	}

	// Malformed base URL (no host): fail-safe, keep the public host.
	sc := types.SiteConfig{ArtifactOverrides: map[string]types.ArtifactOverride{
		"npm": {BaseURL: "not a url"},
	}}
	got := substituteArtifactEgress(in, sc)
	found := false
	for _, d := range got {
		if d == "registry.npmjs.org" {
			found = true
		}
	}
	if !found {
		t.Errorf("malformed base URL should leave npm public host in place; got %v", got)
	}

	// Backing-array safety: a real substitution must not clobber `in`.
	orig := append([]string(nil), in...)
	_ = substituteArtifactEgress(in, types.SiteConfig{ArtifactOverrides: map[string]types.ArtifactOverride{
		"npm": {BaseURL: "https://corp.example/npm/"},
	}})
	if !reflect.DeepEqual(in, orig) {
		t.Errorf("substitution mutated the caller's slice: %v (was %v)", in, orig)
	}
}

// TestArtifactBaseURLs extracts ecosystem->base (URL-only) and drops tokens.
func TestArtifactBaseURLs(t *testing.T) {
	sc := types.SiteConfig{ArtifactOverrides: map[string]types.ArtifactOverride{
		"npm":   {BaseURL: "https://corp/npm/", TokenSecretRef: "npm-token"},
		"cargo": {BaseURL: "https://corp/cargo/"},
	}}
	got := artifactBaseURLs(sc)
	want := map[string]string{"npm": "https://corp/npm/", "cargo": "https://corp/cargo/"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("artifactBaseURLs\n got: %v\nwant: %v", got, want)
	}
	if artifactBaseURLs(types.SiteConfig{}) != nil {
		t.Errorf("no overrides must yield nil")
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
	c = artifactRepoCheck(types.SiteConfig{ArtifactOverrides: map[string]types.ArtifactOverride{
		"npm":   {BaseURL: "https://corp/npm/", TokenSecretRef: "npm-token"},
		"maven": {BaseURL: "https://corp/maven"},
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

// TestPublicRegistryHostsCoverage: every ecosystem the substitution supports has
// public hosts to drop; junk yields nil.
func TestPublicRegistryHostsCoverage(t *testing.T) {
	for _, e := range []string{"npm", "pip", "go", "cargo", "maven", "nuget"} {
		if len(workspacescan.PublicRegistryHosts(e)) == 0 {
			t.Errorf("ecosystem %q has no public hosts", e)
		}
	}
	if workspacescan.PublicRegistryHosts("bogus") != nil {
		t.Errorf("unknown ecosystem must return nil")
	}
}
