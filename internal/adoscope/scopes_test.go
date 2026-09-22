// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adoscope

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

// TestScopesForGolden is the capability -> Entra scope table, written out. It
// is a golden and not a loop over the table it is testing: the resource
// prefix, the exact scope spelling and the fact that four capabilities SHARE
// vso.code_write are the facts a token request depends on, and a test that
// derived them from the same map would assert nothing.
func TestScopesForGolden(t *testing.T) {
	const res = "499b84ac-1321-427f-aa17-267ca6975798/"
	for _, tc := range []struct {
		name string
		caps []Capability
		want []string
	}{
		{"nothing", nil, []string{}},
		{"read carries every area's read scope", []Capability{CapRead}, []string{
			res + "vso.analytics", res + "vso.build", res + "vso.code",
			res + "vso.graph", res + "vso.identity", res + "vso.memberentitlementmanagement",
			res + "vso.packaging", res + "vso.project", res + "vso.release",
			res + "vso.securefiles_read", res + "vso.serviceendpoint", res + "vso.test",
			res + "vso.variablegroups_read", res + "vso.wiki", res + "vso.work",
		}},
		{"code_write", []Capability{CapCodeWrite}, []string{res + "vso.code_write"}},
		{"pr", []Capability{CapPR}, []string{res + "vso.code_write"}},
		{"policy_admin", []Capability{CapPolicyAdmin}, []string{res + "vso.code_write"}},
		{"policy_bypass", []Capability{CapPolicyBypass}, []string{res + "vso.code_write"}},
		{"repo_admin", []Capability{CapRepoAdmin}, []string{res + "vso.code_manage"}},
		{"security_admin reaches the graph and identities too", []Capability{CapSecurityAdmin}, []string{
			res + "vso.graph_manage", res + "vso.identity_manage", res + "vso.security_manage",
		}},
		{"serviceendpoint_admin", []Capability{CapServiceEndpointAdmin}, []string{res + "vso.serviceendpoint_manage"}},
		{"build_execute covers the classic release area as well", []Capability{CapBuildExecute}, []string{
			res + "vso.build_execute", res + "vso.release_execute",
		}},
		{"build_admin is a WRITE, never the vso.build read scope", []Capability{CapBuildAdmin}, []string{
			res + "vso.build_execute", res + "vso.release_manage",
		}},
		{"work_write", []Capability{CapWorkWrite}, []string{res + "vso.work_write"}},
		{"wiki_write", []Capability{CapWikiWrite}, []string{res + "vso.wiki_write"}},
		{"packaging_write", []Capability{CapPackagingWrite}, []string{res + "vso.packaging_write"}},
		{"project_admin", []Capability{CapProjectAdmin}, []string{res + "vso.project_manage"}},

		{"the four policy-and-code capabilities collapse to ONE scope",
			[]Capability{CapCodeWrite, CapPR, CapPolicyAdmin, CapPolicyBypass},
			[]string{res + "vso.code_write"}},
		{"a repeated capability is not a repeated scope",
			[]Capability{CapWorkWrite, CapWorkWrite},
			[]string{res + "vso.work_write"}},
		{"the contribute profile",
			ProfileContribute(),
			[]string{
				res + "vso.analytics", res + "vso.build", res + "vso.code",
				res + "vso.code_write", res + "vso.graph", res + "vso.identity",
				res + "vso.memberentitlementmanagement", res + "vso.packaging",
				res + "vso.project", res + "vso.release", res + "vso.securefiles_read",
				res + "vso.serviceendpoint", res + "vso.test", res + "vso.variablegroups_read",
				res + "vso.wiki", res + "vso.wiki_write", res + "vso.work", res + "vso.work_write",
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ScopesFor(tc.caps)
			if err != nil {
				t.Fatalf("ScopesFor(%v) error = %v", tc.caps, err)
			}
			if got == nil {
				t.Fatalf("ScopesFor() = nil — a JSON caller would render null, not []")
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("ScopesFor(%v) =\n  %v\nwant\n  %v", tc.caps, got, tc.want)
			}
		})
	}
}

// TestScopesForRefusesWhatIsNotGrantable is the F6 pin: a non-grantable
// capability is an ERROR and never an empty scope set. A consumer gating with
// "required scopes are inside the granted scopes" would pass every
// unclassified write on an empty set, because the empty set is inside
// everything — the fail-closed answer would have read as permission.
func TestScopesForRefusesWhatIsNotGrantable(t *testing.T) {
	for _, c := range []Capability{
		CapUnclassifiedWrite, CapUnclassifiedRead, CapDeniedTokens, CapDeniedServiceHooks,
		CapDeniedExtensions, CapDeniedInternal, "invented", "",
	} {
		got, err := ScopesFor([]Capability{c})
		if err == nil {
			t.Errorf("ScopesFor(%q) = %v with no error", c, got)
		}
		if got != nil {
			t.Errorf("ScopesFor(%q) returned %v beside its error", c, got)
		}
	}
	// One bad capability poisons the whole request — a partial scope set for a
	// request that must be refused is the same bug in a smaller shape.
	if _, err := ScopesFor([]Capability{CapWikiWrite, CapDeniedTokens}); err == nil {
		t.Error("ScopesFor() minted scopes for a set containing a denied area")
	}
}

// TestPermitsIsTheGate pins the one spelling of "allowed".
func TestPermitsIsTheGate(t *testing.T) {
	granted := ProfileContribute()
	for _, tc := range []struct {
		name string
		cap  Capability
		want bool
	}{
		{"a granted capability", CapCodeWrite, true},
		{"the read floor", CapRead, true},
		{"a capability outside the profile", CapPolicyBypass, false},
		{"an unclassified write", CapUnclassifiedWrite, false},
		{"an unclassified read", CapUnclassifiedRead, false},
		{"a denied area", CapDeniedTokens, false},
		{"an invented capability", "invented", false},
		{"nothing at all", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Permits(granted, Verdict{Capability: tc.cap}); got != tc.want {
				t.Fatalf("Permits(contribute, %q) = %v, want %v", tc.cap, got, tc.want)
			}
		})
	}
	// Not even a run granted EVERYTHING may perform what the catalogue refused.
	for _, c := range []Capability{CapUnclassifiedWrite, CapDeniedTokens, CapDeniedInternal} {
		if Permits(GrantableCapabilities(), Verdict{Capability: c}) {
			t.Errorf("Permits(every capability, %q) = true", c)
		}
	}
	if Permits(nil, Verdict{Capability: CapRead}) {
		t.Error("Permits(nothing granted, read) = true")
	}
}

// TestScopesForNeverRequestsTheTokenScopes pins that no grantable capability
// resolves to a token-lifecycle scope. Nothing can use them — Azure DevOps
// mints personal access tokens only for Microsoft's own clients — and consent,
// not the request, decides a token's scopes, so asking for one would put it in
// every run's token.
func TestScopesForNeverRequestsTheTokenScopes(t *testing.T) {
	all, err := ScopesFor(GrantableCapabilities())
	if err != nil {
		t.Fatalf("ScopesFor(every grantable capability) error = %v", err)
	}
	if len(all) == 0 {
		t.Fatal("the grantable capabilities resolved to no scopes at all")
	}
	for _, never := range neverRequestedScopes {
		if slices.Contains(all, ResourceID+"/"+never) {
			t.Errorf("the grantable capabilities resolve to %q — nothing may request a token-lifecycle scope", never)
		}
	}
	if !slices.Contains(neverRequestedScopes, "vso.tokens") || !slices.Contains(neverRequestedScopes, "vso.pats") {
		t.Errorf("neverRequestedScopes = %v, want both vso.tokens and vso.pats", neverRequestedScopes)
	}
}

// TestEveryCapabilityIsLabelledAndClassified pins the three sets against each
// other: every value the catalogue defines is Valid, has a label, and is
// either grantable or denied-or-unclassified — never both and never neither.
func TestEveryCapabilityIsLabelledAndClassified(t *testing.T) {
	var all []Capability
	all = append(all, GrantableCapabilities()...)
	all = append(all, CapDeniedTokens, CapDeniedServiceHooks, CapDeniedExtensions,
		CapDeniedInternal, CapUnclassifiedWrite, CapUnclassifiedRead)
	for _, c := range all {
		if !c.Valid() {
			t.Errorf("%q is not Valid", c)
		}
		if Label(c) == "" {
			t.Errorf("%q has no label — a console would render it as nothing", c)
		}
		if c.Grantable() && c.Denied() {
			t.Errorf("%q is both grantable and denied", c)
		}
	}
	if len(GrantableCapabilities()) != 14 {
		t.Fatalf("GrantableCapabilities() has %d entries — the catalogue's grantable set changed; update the profiles and the scope golden with it",
			len(GrantableCapabilities()))
	}
	if Label("invented") != "" {
		t.Error("Label() invented a label for a capability outside the set")
	}
}

// TestProfilesAreInsideTheCatalogue keeps the two shipped profiles honest: a
// profile naming a capability that is not grantable could never be minted, and
// the read profile is the DEFAULT, so it has to be exactly reads.
func TestProfilesAreInsideTheCatalogue(t *testing.T) {
	for _, p := range [][]Capability{ProfileRead(), ProfileContribute()} {
		for _, c := range p {
			if !c.Grantable() {
				t.Errorf("profile %v names %q, which is not grantable", p, c)
			}
		}
	}
	if !slices.Equal(ProfileRead(), []Capability{CapRead}) {
		t.Fatalf("ProfileRead() = %v, want exactly the read capability", ProfileRead())
	}
	if slices.Contains(ProfileContribute(), CapBuildExecute) {
		t.Error("ProfileContribute() queues pipelines — running YAML under the pipeline's own identity is an opt-in, not part of contributing")
	}
	if !slices.Contains(ProfileContribute(), CapRead) {
		t.Error("ProfileContribute() cannot read")
	}
}

// TestReadScopesCoverEveryAreaThatReads ENUMERATES readAreas — every area the
// classifier can answer CapRead for — rather than a hand list, which is what
// let three areas classify as reads whose token could not perform them. For
// each: a GET classifies as CapRead, and its scope is in the read scope set.
func TestReadScopesCoverEveryAreaThatReads(t *testing.T) {
	scopes, err := ScopesFor([]Capability{CapRead})
	if err != nil {
		t.Fatalf("ScopesFor(read) error = %v", err)
	}
	if len(readAreas) == 0 {
		t.Fatal("readAreas is empty")
	}
	for key, scope := range readAreas {
		t.Run(key, func(t *testing.T) {
			path := "/acme/proj/_apis/" + key + "/x"
			v, err := Classify(Request{Method: "GET", Host: "dev.azure.com", Path: path, Org: "acme"})
			if err != nil || v.Capability != CapRead {
				t.Fatalf("Classify(GET %s) = %q, %v — want the read floor", path, v.Capability, err)
			}
			if scope != "" && !slices.Contains(scopes, ResourceID+"/"+scope) {
				t.Fatalf("%s reads as CapRead but %q is not in the read scope set", key, scope)
			}
		})
	}
	// And the reverse: nothing in the read set that no area needs.
	for _, s := range scopes {
		short := strings.TrimPrefix(s, ResourceID+"/")
		if !slices.ContainsFunc(slices.Collect(maps.Values(readAreas)), func(v string) bool { return v == short }) {
			t.Errorf("the read scope set carries %q, which no read area needs", short)
		}
	}
}
