// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer_test

import (
	"reflect"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// leqExplainVariants is the Leq fixture policy, an empty policy, and copies of the fixture with one
// or two fields edited, some wider and some narrower.
func leqExplainVariants() map[string]types.RunPolicySpec {
	edit := func(f func(c *types.RunPolicySpec)) types.RunPolicySpec {
		c, _ := leqFixture()
		f(&c)
		return c
	}
	return map[string]types.RunPolicySpec{
		"the fixture":     edit(func(*types.RunPolicySpec) {}),
		"an empty policy": {},
		"an added domain": edit(func(c *types.RunPolicySpec) {
			c.AllowedDomains = append(slices.Clone(c.AllowedDomains), "api.vendor.example")
		}),
		"fewer domains": edit(func(c *types.RunPolicySpec) { c.AllowedDomains = []string{"pypi.org"} }),
		"a lost denial": edit(func(c *types.RunPolicySpec) { c.DeniedDomains = nil }),
		"an added denial": edit(func(c *types.RunPolicySpec) {
			c.DeniedDomains = []string{"evil.example", "worse.example"}
		}),
		"all egress":            edit(func(c *types.RunPolicySpec) { c.AllowAllEgress = true }),
		"an added method":       edit(func(c *types.RunPolicySpec) { c.AllowedMethods = []string{"GET", "POST", "DELETE"} }),
		"one method":            edit(func(c *types.RunPolicySpec) { c.AllowedMethods = []string{"GET"} }),
		"no method restriction": edit(func(c *types.RunPolicySpec) { c.AllowedMethods = nil }),
		"a looser first use":    edit(func(c *types.RunPolicySpec) { c.FirstUseApproval = types.FirstUseWaitForReview }),
		"a stricter first use":  edit(func(c *types.RunPolicySpec) { c.FirstUseApproval = types.FirstUseAlwaysDeny }),
		"a lower floor":         edit(func(c *types.RunPolicySpec) { c.MinConfinementClass = types.CC1 }),
		"a higher floor":        edit(func(c *types.RunPolicySpec) { c.MinConfinementClass = types.CC3 }),
		"no auto-stop":          edit(func(c *types.RunPolicySpec) { c.AutoStopAfterSec = 0 }),
		"one grant fewer":       edit(func(c *types.RunPolicySpec) { c.EligibleGrants = c.EligibleGrants[:1] }),
		"an added grant": edit(func(c *types.RunPolicySpec) {
			c.EligibleGrants = append(slices.Clone(c.EligibleGrants),
				leqGrant(types.GrantAPIKey, `{"host":"api.vendor.example","secret_name":"other-key"}`, true, true, 300))
		}),
		"a lower floor and an added domain": edit(func(c *types.RunPolicySpec) {
			c.MinConfinementClass = types.CC1
			c.AllowedDomains = append(slices.Clone(c.AllowedDomains), "api.vendor.example")
		}),
	}
}

// The explanation and the verdict agree on every ordered pair: Leq holds exactly when no field is
// named. A field the explanation skips, or a pair it judges the wrong way round, fails here.
func TestLeqWideningsAreEmptyExactlyWhenLeqHolds(t *testing.T) {
	variants := leqExplainVariants()
	var none types.GovernanceLimits
	held, refused := 0, 0
	for an, a := range variants {
		for bn, b := range variants {
			leq, wide := composer.Leq(a, b, none, none), composer.LeqWidenings(a, b)
			if leq {
				held++
			} else {
				refused++
			}
			if leq != (len(wide) == 0) {
				t.Errorf("%s within %s: Leq = %v, widenings = %+v", an, bn, leq, wide)
			}
		}
	}
	if held < len(variants) || refused < len(variants) {
		t.Fatalf("the pairs do not exercise both verdicts: %d held, %d refused", held, refused)
	}
}

// Each row is the fixture with a known edit, measured against the fixture: the explanation names
// that field, and for a list field the entries that are not within the fixture.
func TestLeqWideningsNameTheFieldAndItsEntries(t *testing.T) {
	variants := leqExplainVariants()
	base := variants["the fixture"]
	for _, row := range []struct {
		candidate string
		want      []composer.Widening
	}{
		{"the fixture", nil},
		{"fewer domains", nil},
		{"a higher floor", nil},
		{"an added domain", []composer.Widening{{Field: "allowed_domains", Entries: []string{"api.vendor.example"}}}},
		{"a lost denial", []composer.Widening{{Field: "denied_domains", Entries: []string{"evil.example"}}}},
		{"all egress", []composer.Widening{{Field: "allow_all_egress"}}},
		{"an added method", []composer.Widening{{Field: "allowed_methods", Entries: []string{"DELETE"}}}},
		{"no method restriction", []composer.Widening{{Field: "allowed_methods"}}},
		{"a looser first use", []composer.Widening{{Field: "first_use_approval"}}},
		{"a lower floor", []composer.Widening{{Field: "min_confinement_class"}}},
		{"no auto-stop", []composer.Widening{{Field: "auto_stop_after_sec"}}},
		{"an added grant", []composer.Widening{{Field: "eligible_grants", Entries: []string{"api_key"}}}},
		{"a lower floor and an added domain", []composer.Widening{
			{Field: "allowed_domains", Entries: []string{"api.vendor.example"}},
			{Field: "min_confinement_class"},
		}},
	} {
		if got := composer.LeqWidenings(variants[row.candidate], base); !reflect.DeepEqual(got, row.want) {
			t.Errorf("%s: widenings = %+v, want %+v", row.candidate, got, row.want)
		}
	}
}
