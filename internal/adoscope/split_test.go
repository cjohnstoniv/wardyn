// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adoscope

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

// splitMap is the per-area split's old → new mapping, the one the upgrade
// migration applies to every stored list. Written out here rather than read
// from the migration, so the test and the SQL are two statements of one fact.
var splitMap = map[Capability][]Capability{
	"read": {CapCodeRead, CapWorkRead, CapWikiRead, CapBuildRead, CapReleaseRead, CapServiceEndpointRead,
		CapLibraryRead, CapPackagingRead, CapTestRead, CapProjectRead, CapIdentityRead, CapAnalyticsRead},
	"work_write":            {CapWorkWrite, CapWorkAdmin},
	"build_execute":         {CapBuildExecute, CapReleaseExecute},
	"build_admin":           {CapBuildAdmin, CapReleaseAdmin},
	"packaging_write":       {CapPackagingWrite},
	"code_write":            {CapCodeWrite},
	"pr":                    {CapPR},
	"policy_admin":          {CapPolicyAdmin},
	"policy_bypass":         {CapPolicyBypass},
	"repo_admin":            {CapRepoAdmin},
	"security_admin":        {CapSecurityAdmin},
	"serviceendpoint_admin": {CapServiceEndpointAdmin},
	"wiki_write":            {CapWikiWrite},
	"project_admin":         {CapProjectAdmin},
}

// preSplitScopes is a LITERAL copy of the catalogue's capability -> scope
// table before the split, so a change to today's table cannot move both sides.
var preSplitScopes = map[Capability][]string{
	"read": {"vso.analytics", "vso.build", "vso.code", "vso.graph", "vso.identity", "vso.memberentitlementmanagement",
		"vso.packaging", "vso.profile", "vso.project", "vso.release", "vso.securefiles_read", "vso.serviceendpoint",
		"vso.test", "vso.variablegroups_read", "vso.wiki", "vso.work"},
	"code_write":            {"vso.code_write"},
	"pr":                    {"vso.code_write"},
	"policy_admin":          {"vso.code_write"},
	"policy_bypass":         {"vso.code_write"},
	"repo_admin":            {"vso.code_manage"},
	"security_admin":        {"vso.graph_manage", "vso.identity_manage", "vso.security_manage"},
	"serviceendpoint_admin": {"vso.serviceendpoint_manage"},
	"build_execute":         {"vso.build_execute", "vso.release_execute"},
	"build_admin":           {"vso.build_execute", "vso.release_manage"},
	"work_write":            {"vso.work_write"},
	"wiki_write":            {"vso.wiki_write"},
	"packaging_write":       {"vso.packaging_write"},
	"project_admin":         {"vso.project_manage"},
}

// TestScopesUnchangedUnderMigration: every old capability maps onto new ones
// needing EXACTLY the scopes it needed — so nobody is asked to consent again.
func TestScopesUnchangedUnderMigration(t *testing.T) {
	if len(splitMap) != 14 || len(preSplitScopes) != 14 {
		t.Fatalf("the split maps %d old capabilities and copies %d scope rows, want all 14", len(splitMap), len(preSplitScopes))
	}
	for old, want := range preSplitScopes {
		got, err := ScopesFor(splitMap[old])
		if err != nil {
			t.Fatalf("ScopesFor(map(%q)) error = %v", old, err)
		}
		q := make([]string, len(want))
		for i, s := range want {
			q[i] = ResourceID + "/" + s
		}
		slices.Sort(q)
		if !slices.Equal(got, q) {
			t.Errorf("ScopesFor(map(%q)) =\n  %v\nwant the pre-split\n  %v", old, got, q)
		}
	}
	// The map's images are pairwise disjoint and cover the grantable set, so a
	// migrated list permits exactly what the old one did.
	seen := map[Capability]Capability{}
	for old, news := range splitMap {
		for _, c := range news {
			if !c.Grantable() {
				t.Errorf("map(%q) names %q, which is not grantable", old, c)
			}
			if prev, dup := seen[c]; dup {
				t.Errorf("%q is in both map(%q) and map(%q)", c, prev, old)
			}
			seen[c] = old
		}
	}
	// packaging_manage is the one capability no old id maps to: the old
	// catalogue never requested its scope (see splitExceptions).
	for _, c := range GrantableCapabilities() {
		if _, ok := seen[c]; ok == (c == CapPackagingManage) {
			t.Errorf("%q: in the split's images = %v, want %v", c, ok, c != CapPackagingManage)
		}
	}
}

// splitExceptions are the rows whose new verdict is deliberately NOT in
// map(old verdict), each for a documented reason.
var splitExceptions = map[string]string{
	// Discovery is free to any run holding a capability, where it used to need read.
	"connectiondata GET":      "discovery",
	"resourceareas GET":       "discovery",
	"OPTIONS on an area":      "discovery",
	"OPTIONS on the API root": "discovery",
	// Azure DevOps serves these only under vso.packaging_manage, a scope the
	// old packaging_write never requested: permitted before, refused by ADO.
	"package client DELETE":  "packaging_manage",
	"package version DELETE": "packaging_manage",
	"feed create POST":       "packaging_manage",
	"feed DELETE":            "packaging_manage",
	"feed permissions PATCH": "packaging_manage",
	"feed recycle bin PATCH": "packaging_manage",
	// Only the three documented search resources classify.
	"almsearch undocumented resource POST": "unclassified_read",
	"almsearch undocumented resource GET":  "unclassified_read",
	// A $batch operation whose method cannot be read is refused.
	"batch op with an override header": "error",
	"batch op with no method":          "error",
	"batch op with an unknown method":  "error",
}

// TestSplitEquivalence replays the golden corpus against the verdicts the
// catalogue gave before the split: every new verdict is inside the mapping
// of the old one, errors stay errors, and the only departures are the named
// exceptions above.
func TestSplitEquivalence(t *testing.T) {
	raw, err := os.ReadFile("testdata/pre-split-verdicts.json")
	if err != nil {
		t.Fatal(err)
	}
	var pre map[string]string
	if err := json.Unmarshal(raw, &pre); err != nil {
		t.Fatal(err)
	}
	if len(pre) != len(perAreaCases) {
		t.Fatalf("the recording holds %d verdicts, the corpus has %d rows — re-record on the pre-split tree", len(pre), len(perAreaCases))
	}
	for _, tc := range perAreaCases {
		t.Run(tc.name, func(t *testing.T) {
			old, ok := pre[tc.name]
			if !ok {
				t.Fatalf("no pre-split verdict recorded for %q", tc.name)
			}
			v, err := Classify(tc.req)
			got := string(v.Capability)
			if err != nil {
				got = "error"
			}
			if want, ok := splitExceptions[tc.name]; ok {
				if got != want {
					t.Fatalf("documented exception: got %q, want %q (was %q)", got, want, old)
				}
				return
			}
			switch news, mapped := splitMap[Capability(old)]; {
			case old == "error":
				if got != "error" {
					t.Fatalf("was refused, now %q", got)
				}
			case mapped:
				if !slices.Contains(news, Capability(got)) {
					t.Fatalf("was %q, now %q — not in map(%q) = %v", old, got, old, news)
				}
			default:
				// A refusal or unclassified verdict stays exactly itself.
				if got != old {
					t.Fatalf("was %q, now %q", old, got)
				}
			}
		})
	}
}

// TestDiscoveryPermits: discovery is allowed to any run holding a grantable
// capability, refused to an empty grant, and never grantable or listable.
func TestDiscoveryPermits(t *testing.T) {
	v := Verdict{Capability: CapDiscovery}
	for _, c := range GrantableCapabilities() {
		if !Permits([]Capability{c}, v) {
			t.Errorf("Permits([%s], discovery) = false", c)
		}
	}
	if Permits(nil, v) {
		t.Error("Permits(nothing, discovery) = true")
	}
	if Permits([]Capability{CapUnclassifiedWrite, CapDiscovery}, v) {
		t.Error("discovery permitted to a list holding no grantable capability")
	}
	if CapDiscovery.Grantable() || CapDiscovery.Denied() || !CapDiscovery.Valid() {
		t.Errorf("discovery: grantable=%v denied=%v valid=%v, want false/false/true",
			CapDiscovery.Grantable(), CapDiscovery.Denied(), CapDiscovery.Valid())
	}
	if _, err := ScopesFor([]Capability{CapDiscovery}); err == nil {
		t.Error("ScopesFor(discovery) succeeded — it is not a capability a list may name")
	}
	if Label(CapDiscovery) != "Find where Azure DevOps serves each API — no organisation data" {
		t.Errorf("Label(discovery) = %q", Label(CapDiscovery))
	}
}

// TestEveryGrantableHasScopesAndNames: each grantable capability requests at
// least one scope, and carries both a label and a canon short name.
func TestEveryGrantableHasScopesAndNames(t *testing.T) {
	for _, c := range GrantableCapabilities() {
		if len(capabilityScopes[c]) == 0 {
			t.Errorf("%q requests no scope", c)
		}
		if Label(c) == "" {
			t.Errorf("%q has no label", c)
		}
		if ShortLabel(c) == string(c) {
			t.Errorf("%q has no short label", c)
		}
	}
	if len(labels) != len(GrantableCapabilities())+7 {
		t.Errorf("labels has %d entries, want one per grantable capability plus discovery and the six refusals", len(labels))
	}
	if len(shortLabels) != len(GrantableCapabilities()) {
		t.Errorf("shortLabels has %d entries, want %d", len(shortLabels), len(GrantableCapabilities()))
	}
}
