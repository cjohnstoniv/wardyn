// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adoscope

import (
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
			res + "vso.build", res + "vso.code", res + "vso.packaging",
			res + "vso.project", res + "vso.wiki", res + "vso.work",
		}},
		{"code_write", []Capability{CapCodeWrite}, []string{res + "vso.code_write"}},
		{"pr", []Capability{CapPR}, []string{res + "vso.code_write"}},
		{"policy_admin", []Capability{CapPolicyAdmin}, []string{res + "vso.code_write"}},
		{"policy_bypass", []Capability{CapPolicyBypass}, []string{res + "vso.code_write"}},
		{"repo_admin", []Capability{CapRepoAdmin}, []string{res + "vso.code_manage"}},
		{"security_admin", []Capability{CapSecurityAdmin}, []string{res + "vso.security_manage"}},
		{"serviceendpoint_admin", []Capability{CapServiceEndpointAdmin}, []string{res + "vso.serviceendpoint_manage"}},
		{"build_execute", []Capability{CapBuildExecute}, []string{res + "vso.build_execute"}},
		{"build_admin", []Capability{CapBuildAdmin}, []string{res + "vso.build"}},
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
				res + "vso.build", res + "vso.code", res + "vso.code_write",
				res + "vso.packaging", res + "vso.project", res + "vso.wiki",
				res + "vso.wiki_write", res + "vso.work", res + "vso.work_write",
			}},

		{"a denied area contributes nothing", []Capability{CapDeniedTokens}, []string{}},
		{"an unclassified write contributes nothing", []Capability{CapUnclassifiedWrite}, []string{}},
		{"an unknown string contributes nothing", []Capability{"invented"}, []string{}},
		{"a denied area beside a real one contributes only the real one",
			[]Capability{CapDeniedTokens, CapWikiWrite},
			[]string{res + "vso.wiki_write"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ScopesFor(tc.caps)
			if got == nil {
				t.Fatalf("ScopesFor() = nil — a JSON caller would render null, not []")
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("ScopesFor(%v) =\n  %v\nwant\n  %v", tc.caps, got, tc.want)
			}
		})
	}
}

// TestScopesForNeverMintsTheTokenScope is the record that a minted_pat row's
// extra scope belongs to the CONTROL PLANE and never to a run: no capability a
// row can grant resolves to it, so no run's token can mint a second
// credential outside the capabilities it was granted.
func TestScopesForNeverMintsTheTokenScope(t *testing.T) {
	all := ScopesFor(GrantableCapabilities())
	for _, s := range all {
		if strings.HasSuffix(s, "/"+ScopeTokens) {
			t.Fatalf("the grantable capabilities resolve to %q — a run's token must never carry the token scope", s)
		}
	}
	if len(all) == 0 {
		t.Fatal("the grantable capabilities resolved to no scopes at all")
	}
}

// TestEveryCapabilityIsLabelledAndClassified pins the three sets against each
// other: every value the catalogue defines is Valid, has a label, and is
// either grantable or denied-or-unclassified — never both and never neither.
func TestEveryCapabilityIsLabelledAndClassified(t *testing.T) {
	var all []Capability
	all = append(all, GrantableCapabilities()...)
	all = append(all, CapDeniedTokens, CapDeniedServiceHooks, CapDeniedExtensions,
		CapDeniedInternal, CapUnclassifiedWrite)
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
