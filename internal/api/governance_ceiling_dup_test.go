// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A ceiling that lists one stored-secret pairing twice is refused when a profile is written (#2736):
// the runtime clamp answers it by list order, the write-time comparator by any entry. A row already
// stored keeps resolving, and the clamp still forces owner_only on it as before.
func TestProfileWriteRefusesDuplicateCeilingPairing(t *testing.T) {
	vendor := func(secret string, ownerOnly bool) types.GrantSpec {
		return types.GrantSpec{Kind: types.GrantAPIKey, OwnerOnly: ownerOnly,
			Scope: json.RawMessage(`{"host":"api.vendor.example","secret_name":"` + secret + `"}`)}
	}
	s, _ := govCovServer(t, &govCovStore{}, func(c *Config) {
		c.DefaultPolicy.EligibleGrants = []types.GrantSpec{vendor("vendor-key", false), vendor("vendor-key-2", false)}
	})
	dup := `[{"kind":"api_key","owner_only":true,"scope":{"host":"api.vendor.example","secret_name":"vendor-key"}},` +
		`{"kind":"api_key","scope":{"host":"api.vendor.example","secret_name":"vendor-key"}}]`
	base := govCovStandalone(govCovBase, "base", "pypi.org")
	base.Ceiling.EligibleGrants = []types.GrantSpec{vendor("vendor-key", false)}
	const wantMsg = `eligible_grants[1] names the same api_key pairing ("api.vendor.example") as eligible_grants[0]`

	t.Run("a standalone ceiling", func(t *testing.T) {
		body := `{"name":"c","ceiling":{"min_confinement_class":"CC2","eligible_grants":` + dup + `}}`
		ref := govCovAsRefusal(t, govCovBuildErr(t, s, nil, govCovPID, body))
		if ref.status != 400 || ref.reason != reasonGovernanceCeilingInvalid || !strings.Contains(ref.msg, wantMsg) {
			t.Errorf("refusal = %+v", ref)
		}
	})
	t.Run("an overlay", func(t *testing.T) {
		body := `{"name":"child","parent_id":"` + govCovBase.String() + `","overlay":{"eligible_grants":` + dup + `}}`
		ref := govCovAsRefusal(t, govCovBuildErr(t, s, []types.GovernanceUnit{base}, govCovPID, body))
		if ref.status != 400 || ref.reason != reasonGovernanceOverlayInvalid || !strings.Contains(ref.msg, wantMsg) {
			t.Errorf("refusal = %+v", ref)
		}
	})
	t.Run("two secrets for one host are two pairings", func(t *testing.T) {
		body := `{"name":"c","ceiling":{"min_confinement_class":"CC2","eligible_grants":[` +
			`{"kind":"api_key","scope":{"host":"api.vendor.example","secret_name":"vendor-key"}},` +
			`{"kind":"api_key","scope":{"host":"api.vendor.example","secret_name":"vendor-key-2"}}]}}`
		govCovBuilt(t, s, nil, govCovPID, body)
	})
	t.Run("a stored row keeps resolving, owner_only forced as before", func(t *testing.T) {
		stored := govCovStandalone(govCovTop, "stored", "pypi.org")
		stored.Ceiling.EligibleGrants = []types.GrantSpec{vendor("vendor-key", true), vendor("vendor-key", false)}
		res, err := s.composeChain([]types.GovernanceUnit{stored})
		if err != nil || len(res.Ceiling.EligibleGrants) != 2 {
			t.Fatalf("resolve = %+v, %v", res, err)
		}
		out, _ := composer.Clamp(types.RunPolicySpec{MinConfinementClass: types.CC2, EligibleGrants: []types.GrantSpec{vendor("vendor-key", false)}},
			res.Ceiling, res.Limits)
		if len(out.EligibleGrants) != 1 || !out.EligibleGrants[0].OwnerOnly {
			t.Errorf("clamped grants = %+v, want the one grant forced owner_only", out.EligibleGrants)
		}
	})
}
