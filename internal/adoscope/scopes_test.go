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
		{"code_read", []Capability{CapCodeRead}, []string{res + "vso.code"}},
		{"work_read", []Capability{CapWorkRead}, []string{res + "vso.work"}},
		{"wiki_read", []Capability{CapWikiRead}, []string{res + "vso.wiki"}},
		{"build_read", []Capability{CapBuildRead}, []string{res + "vso.build"}},
		{"release_read", []Capability{CapReleaseRead}, []string{res + "vso.release"}},
		{"serviceendpoint_read", []Capability{CapServiceEndpointRead}, []string{res + "vso.serviceendpoint"}},
		{"library_read reaches both library scopes", []Capability{CapLibraryRead}, []string{
			res + "vso.securefiles_read", res + "vso.variablegroups_read",
		}},
		{"packaging_read", []Capability{CapPackagingRead}, []string{res + "vso.packaging"}},
		{"test_read", []Capability{CapTestRead}, []string{res + "vso.test"}},
		{"project_read carries the profile too", []Capability{CapProjectRead}, []string{res + "vso.profile", res + "vso.project"}},
		{"identity_read", []Capability{CapIdentityRead}, []string{
			res + "vso.graph", res + "vso.identity", res + "vso.memberentitlementmanagement",
		}},
		{"analytics_read", []Capability{CapAnalyticsRead}, []string{res + "vso.analytics"}},
		{"code_write", []Capability{CapCodeWrite}, []string{res + "vso.code_write"}},
		{"pr", []Capability{CapPR}, []string{res + "vso.code_write"}},
		{"policy_admin", []Capability{CapPolicyAdmin}, []string{res + "vso.code_write"}},
		{"policy_bypass", []Capability{CapPolicyBypass}, []string{res + "vso.code_write"}},
		{"repo_admin", []Capability{CapRepoAdmin}, []string{res + "vso.code_manage"}},
		{"security_admin reaches the graph and identities too", []Capability{CapSecurityAdmin}, []string{
			res + "vso.graph_manage", res + "vso.identity_manage", res + "vso.security_manage",
		}},
		{"serviceendpoint_admin", []Capability{CapServiceEndpointAdmin}, []string{res + "vso.serviceendpoint_manage"}},
		{"build_execute", []Capability{CapBuildExecute}, []string{res + "vso.build_execute"}},
		{"build_admin is a WRITE, never the vso.build read scope", []Capability{CapBuildAdmin}, []string{res + "vso.build_execute"}},
		{"release_execute", []Capability{CapReleaseExecute}, []string{res + "vso.release_execute"}},
		{"release_admin", []Capability{CapReleaseAdmin}, []string{res + "vso.release_manage"}},
		{"work_write", []Capability{CapWorkWrite}, []string{res + "vso.work_write"}},
		{"work_admin shares work_write's scope", []Capability{CapWorkAdmin}, []string{res + "vso.work_write"}},
		{"wiki_write", []Capability{CapWikiWrite}, []string{res + "vso.wiki_write"}},
		{"packaging_write", []Capability{CapPackagingWrite}, []string{res + "vso.packaging_write"}},
		{"packaging_manage", []Capability{CapPackagingManage}, []string{res + "vso.packaging_manage"}},
		{"project_admin", []Capability{CapProjectAdmin}, []string{res + "vso.project_manage"}},

		{"the four policy-and-code capabilities collapse to ONE scope",
			[]Capability{CapCodeWrite, CapPR, CapPolicyAdmin, CapPolicyBypass},
			[]string{res + "vso.code_write"}},
		{"a repeated capability is not a repeated scope",
			[]Capability{CapWorkWrite, CapWorkWrite},
			[]string{res + "vso.work_write"}},
		{"the default profile",
			ProfileDefault(),
			[]string{res + "vso.code", res + "vso.profile", res + "vso.project"}},
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
	granted := []Capability{CapCodeRead, CapCodeWrite, CapPR, CapWorkRead, CapWorkWrite}
	for _, tc := range []struct {
		name string
		cap  Capability
		want bool
	}{
		{"a granted capability", CapCodeWrite, true},
		{"a granted read", CapCodeRead, true},
		{"a read of another area", CapWikiRead, false},
		{"a capability outside the profile", CapPolicyBypass, false},
		{"an unclassified write", CapUnclassifiedWrite, false},
		{"an unclassified read", CapUnclassifiedRead, false},
		{"a denied area", CapDeniedTokens, false},
		{"an invented capability", "invented", false},
		{"nothing at all", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Permits(granted, Verdict{Capability: tc.cap}); got != tc.want {
				t.Fatalf("Permits(%v, %q) = %v, want %v", granted, tc.cap, got, tc.want)
			}
		})
	}
	// Not even a run granted EVERYTHING may perform what the catalogue refused.
	for _, c := range []Capability{CapUnclassifiedWrite, CapDeniedTokens, CapDeniedInternal} {
		if Permits(GrantableCapabilities(), Verdict{Capability: c}) {
			t.Errorf("Permits(every capability, %q) = true", c)
		}
	}
	if Permits(nil, Verdict{Capability: CapCodeRead}) {
		t.Error("Permits(nothing granted, code_read) = true")
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
// are the same before and after #1409, which moves the release scopes onto
// capabilities of their own.
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
			[]Capability{CapWorkWrite, CapCodeWrite}, "vso.code_write vso.work_write"},
		{"input order does not matter",
			[]Capability{CapWikiWrite, CapRepoAdmin, CapProjectAdmin},
			"vso.code_manage vso.project_manage vso.wiki_write"},
		{"one capability, three scopes",
			[]Capability{CapSecurityAdmin}, "vso.graph_manage vso.identity_manage vso.security_manage"},
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
		for _, form := range []string{s, ResourceID + "/" + s, strings.ToUpper(s), strings.ToUpper(ResourceID) + "/" + s,
			"https://app.vssps.visualstudio.com/" + s, "HTTPS://APP.VSSPS.VISUALSTUDIO.COM/" + s} {
			if !IsTokenScope(form) {
				t.Errorf("IsTokenScope(%q) = false, want true", form)
			}
		}
	}
	all, err := ScopesFor(GrantableCapabilities())
	if err != nil {
		t.Fatalf("ScopesFor(every grantable capability) error = %v", err)
	}
	for _, s := range append(all, "vso.code", "vso.code_write", "openid", "offline_access", "", "vso.pats_extra", "other/vso.pats", "https://example.com/vso.pats") {
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
		CapDeniedInternal, CapUnclassifiedWrite, CapUnclassifiedRead, CapDiscovery)
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
	if len(GrantableCapabilities()) != 29 {
		t.Fatalf("GrantableCapabilities() has %d entries — the catalogue's grantable set changed; update the profiles and the scope golden with it",
			len(GrantableCapabilities()))
	}
	if Label("invented") != "" {
		t.Error("Label() invented a label for a capability outside the set")
	}
}

// TestProfilesAreInsideTheCatalogue keeps the shipped default profile honest:
// a profile naming a capability that is not grantable could never be granted,
// and the default is read the code and see the projects, nothing more.
func TestProfilesAreInsideTheCatalogue(t *testing.T) {
	for _, c := range ProfileDefault() {
		if !c.Grantable() {
			t.Errorf("ProfileDefault() names %q, which is not grantable", c)
		}
	}
	if !slices.Equal(ProfileDefault(), []Capability{CapProjectRead, CapCodeRead}) {
		t.Fatalf("ProfileDefault() = %v, want [project_read code_read]", ProfileDefault())
	}
}

// TestReadScopesCoverEveryAreaThatReads ENUMERATES readAreas — every area the
// classifier answers a read for — rather than a hand list, which is what let
// three areas classify as reads whose token could not perform them. For each:
// a GET classifies as the area's read, and its scope is in that read's scopes.
func TestReadScopesCoverEveryAreaThatReads(t *testing.T) {
	if len(readAreas) == 0 {
		t.Fatal("readAreas is empty")
	}
	for key, a := range readAreas {
		t.Run(key, func(t *testing.T) {
			path := "/acme/proj/_apis/" + key + "/x"
			v, err := Classify(Request{Method: "GET", Host: "dev.azure.com", Path: path, Org: "acme"})
			if err != nil || v.Capability != a.cap {
				t.Fatalf("Classify(GET %s) = %q, %v — want %q", path, v.Capability, err, a.cap)
			}
			if a.scope == "" {
				if a.cap != CapDiscovery {
					t.Fatalf("%s reads as %q with no scope — only discovery is unscoped", key, a.cap)
				}
				return
			}
			scopes, err := ScopesFor([]Capability{a.cap})
			if err != nil || !slices.Contains(scopes, ResourceID+"/"+a.scope) {
				t.Fatalf("%s reads as %q but %q is not in its scopes %v (%v)", key, a.cap, a.scope, scopes, err)
			}
		})
	}
	// And the reverse: no read capability carries a scope no area needs.
	for c, scopes := range capabilityScopes {
		if !strings.HasSuffix(string(c), "_read") {
			continue
		}
		for _, s := range scopes {
			if !slices.ContainsFunc(slices.Collect(maps.Values(readAreas)), func(a readArea) bool { return a.cap == c && a.scope == s }) {
				t.Errorf("%q carries %q, which no read area of it needs", c, s)
			}
		}
	}
}
