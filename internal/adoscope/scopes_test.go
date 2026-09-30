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
			res + "vso.packaging", res + "vso.profile", res + "vso.project",
			res + "vso.release", res + "vso.securefiles_read", res + "vso.serviceendpoint",
			res + "vso.test", res + "vso.variablegroups_read", res + "vso.wiki", res + "vso.work",
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
				res + "vso.profile", res + "vso.project", res + "vso.release", res + "vso.securefiles_read",
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
// resolves to a token-lifecycle scope. Consent, not the request, decides an
// Entra token's scopes, so a capability that asked for one would put it in
// every run's token; only MintScopes names two of them, for the sign-in.
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

// TestPATScope pins the personal access token's scope string: unqualified,
// sorted, deduplicated, space-joined. The rows use capabilities whose scopes
// #1409's re-cut of the read catalogue does not touch.
func TestPATScope(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps []Capability
		want string
	}{
		{"one capability, one scope", []Capability{CapCodeWrite}, "vso.code_write"},
		{"the four that share a scope collapse to it",
			[]Capability{CapPolicyBypass, CapCodeWrite, CapPR, CapPolicyAdmin}, "vso.code_write"},
		{"several scopes are sorted and space-joined",
			[]Capability{CapBuildExecute, CapCodeWrite}, "vso.build_execute vso.code_write vso.release_execute"},
		{"input order does not matter",
			[]Capability{CapWorkWrite, CapRepoAdmin, CapBuildAdmin},
			"vso.build_execute vso.code_manage vso.release_manage vso.work_write"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PATScope(tc.caps)
			if err != nil {
				t.Fatalf("PATScope(%v) error = %v", tc.caps, err)
			}
			if got != tc.want {
				t.Errorf("PATScope(%v) = %q, want %q", tc.caps, got, tc.want)
			}
		})
	}
}

// TestPATScopeIsScopesForUnqualified holds the seam: PATScope is ScopesFor with
// the resource prefix off, for every grantable capability, so the two cannot
// drift into two tables.
func TestPATScopeIsScopesForUnqualified(t *testing.T) {
	for _, c := range GrantableCapabilities() {
		qualified, err := ScopesFor([]Capability{c})
		if err != nil {
			t.Fatalf("ScopesFor(%q) error = %v", c, err)
		}
		want := strings.ReplaceAll(strings.Join(qualified, " "), ResourceID+"/", "")
		got, err := PATScope([]Capability{c})
		if err != nil {
			t.Fatalf("PATScope(%q) error = %v", c, err)
		}
		if got != want {
			t.Errorf("PATScope(%q) = %q, want ScopesFor unqualified %q", c, got, want)
		}
		if strings.Contains(got, "/") {
			t.Errorf("PATScope(%q) = %q, want no resource qualifier", c, got)
		}
	}
}

// TestPATScopeRefusesWhatIsNotGrantable: a denied area, an unclassified write,
// an invented capability, or nothing at all is an error and never an empty or
// partial scope — a token created with no scope must not read as a narrow one.
func TestPATScopeRefusesWhatIsNotGrantable(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps []Capability
	}{
		{"an invented capability", []Capability{"superuser"}},
		{"a good one beside a bad one", []Capability{CapCodeWrite, "superuser"}},
		{"a denied area", []Capability{CapDeniedTokens}},
		{"the unclassified-write placeholder", []Capability{CapUnclassifiedWrite}},
		{"no capabilities", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PATScope(tc.caps)
			if err == nil {
				t.Fatalf("PATScope(%v) = %q, want an error", tc.caps, got)
			}
			if got != "" {
				t.Errorf("PATScope(%v) = %q with an error, want no scope", tc.caps, got)
			}
		})
	}
}

// TestMintScopes pins the two permissions the sign-in asks for, qualified, and
// that no capability's scopes overlap them.
func TestMintScopes(t *testing.T) {
	want := []string{ResourceID + "/vso.pats", ResourceID + "/vso.pats_manage"}
	if got := MintScopes(); !slices.Equal(got, want) {
		t.Fatalf("MintScopes() = %v, want %v", got, want)
	}
	all, err := ScopesFor(GrantableCapabilities())
	if err != nil {
		t.Fatalf("ScopesFor(every grantable capability) error = %v", err)
	}
	for _, m := range MintScopes() {
		if slices.Contains(all, m) {
			t.Errorf("a capability resolves to %q — a mint scope is never a capability's", m)
		}
	}
}

// TestIsTokenScope: the five scopes that let a token create tokens, qualified
// or not, in any case; and nothing a capability needs.
func TestIsTokenScope(t *testing.T) {
	for _, s := range []string{"vso.pats", "vso.pats_manage", "vso.tokens", "vso.tokenadministration", "user_impersonation"} {
		for _, form := range []string{s, ResourceID + "/" + s, strings.ToUpper(s), strings.ToUpper(ResourceID) + "/" + s} {
			if !IsTokenScope(form) {
				t.Errorf("IsTokenScope(%q) = false, want true", form)
			}
		}
	}
	all, err := ScopesFor(GrantableCapabilities())
	if err != nil {
		t.Fatalf("ScopesFor(every grantable capability) error = %v", err)
	}
	for _, s := range append(all, "vso.code", "vso.code_write", "openid", "offline_access", "", "vso.pats_extra", "other/vso.pats") {
		if IsTokenScope(s) {
			t.Errorf("IsTokenScope(%q) = true, want false", s)
		}
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
