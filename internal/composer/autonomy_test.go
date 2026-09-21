// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"encoding/json"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestAutonomyPostureAxes walks each axis independently. The three are folded
// by a min() that cannot tell them apart, so a wrong axis is invisible in the
// resolved level whenever another axis happens to cap lower — which is most of
// the time on a real spec.
func TestAutonomyPostureAxes(t *testing.T) {
	apiKey := func(host string) types.GrantSpec {
		return types.GrantSpec{Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"host":"` + host + `"}`)}
	}
	t.Run("egress", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			spec types.RunPolicySpec
			want types.AutonomyEgressPosture
		}{
			{"allow-all is open", types.RunPolicySpec{AllowAllEgress: true}, types.AutonomyEgressOpen},
			{"a host beyond baseline is open",
				types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com", "db.corp.example"}}, types.AutonomyEgressOpen},
			{"baseline hosts alone are not open",
				types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com", "pypi.org"}}, types.AutonomyEgressSealed},
			{"first-use review raises to reviewed",
				types.RunPolicySpec{AllowedDomains: []string{"pypi.org"}, FirstUseApproval: types.FirstUseWaitForReview}, types.AutonomyEgressReviewed},
			{"deny_with_review raises too",
				types.RunPolicySpec{FirstUseApproval: types.FirstUseDenyWithReview}, types.AutonomyEgressReviewed},
			{"always_deny is sealed", types.RunPolicySpec{FirstUseApproval: types.FirstUseAlwaysDeny}, types.AutonomyEgressSealed},
			{"unset first-use is sealed (Normalize fails closed)", types.RunPolicySpec{}, types.AutonomyEgressSealed},
			// Open BEATS reviewed: first_use_approval only governs hosts that are
			// NOT allowlisted, so a custom host already on the list is reach the
			// review posture never sees.
			{"allow-all outranks a review posture",
				types.RunPolicySpec{AllowAllEgress: true, FirstUseApproval: types.FirstUseWaitForReview}, types.AutonomyEgressOpen},
		} {
			if got := AutonomyPostureOf(tc.spec, types.CC2).Egress; got != tc.want {
				t.Errorf("%s: egress = %q, want %q", tc.name, got, tc.want)
			}
		}
	})

	t.Run("secrets", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			grants []types.GrantSpec
			want   types.AutonomySecretsPosture
		}{
			{"no grants", nil, types.AutonomySecretsNone},
			{"a read-only github token is baseline",
				[]types.GrantSpec{{Kind: types.GrantGitHubToken, Scope: json.RawMessage(`{"permissions":{"contents":"read"}}`)}},
				types.AutonomySecretsBaseline},
			{"a write github token is powerful",
				[]types.GrantSpec{{Kind: types.GrantGitHubToken, Scope: json.RawMessage(`{"permissions":{"contents":"write"}}`)}},
				types.AutonomySecretsPowerful},
			{"cloud_sts is powerful", []types.GrantSpec{{Kind: types.GrantCloudSTS}}, types.AutonomySecretsPowerful},
			{"a baseline api_key is baseline", []types.GrantSpec{apiKey("api.anthropic.com")}, types.AutonomySecretsBaseline},
			{"an api_key to a third-party host is powerful", []types.GrantSpec{apiKey("api.stripe.com")}, types.AutonomySecretsPowerful},
			{"git_pat is powerful", []types.GrantSpec{{Kind: types.GrantGitPAT}}, types.AutonomySecretsPowerful},
			{"ssh_key is powerful", []types.GrantSpec{{Kind: types.GrantSSHKey}}, types.AutonomySecretsPowerful},
			{"env_secret is powerful", []types.GrantSpec{{Kind: types.GrantEnvSecret}}, types.AutonomySecretsPowerful},
			// The scan must not stop at the first non-powerful grant.
			{"a powerful grant behind a baseline one still wins",
				[]types.GrantSpec{apiKey("api.anthropic.com"), {Kind: types.GrantSSHKey}}, types.AutonomySecretsPowerful},
		} {
			got := AutonomyPostureOf(types.RunPolicySpec{EligibleGrants: tc.grants}, types.CC2).Secrets
			if got != tc.want {
				t.Errorf("%s: secrets = %q, want %q", tc.name, got, tc.want)
			}
		}
	})

	t.Run("confinement", func(t *testing.T) {
		for _, tc := range []struct {
			enforced, want types.ConfinementClass
		}{
			{types.CC1, types.CC1}, {types.CC2, types.CC2}, {types.CC3, types.CC3},
			// An empty enforced class reads as CC1, which is the rubric field
			// docs' own promise and the fail-closed reading: CC1 is the weakest
			// tier, so it selects the most restrictive cap rather than none.
			{"", types.CC1},
		} {
			if got := AutonomyPostureOf(types.RunPolicySpec{}, tc.enforced).Confinement; got != tc.want {
				t.Errorf("enforced %q: confinement = %q, want %q", tc.enforced, got, tc.want)
			}
		}
	})
}

// TestFoldAutonomyMinimum pins the fold: the minimum over the applicable caps,
// the field that bound it, and the two ways nothing binds at all.
func TestFoldAutonomyMinimum(t *testing.T) {
	sealedNoneCC2 := types.AutonomyPosture{
		Egress: types.AutonomyEgressSealed, Secrets: types.AutonomySecretsNone, Confinement: types.CC2,
	}
	for _, tc := range []struct {
		name      string
		rubric    types.AutonomyRubric
		posture   types.AutonomyPosture
		wantLevel types.AutonomyLevel
		wantBound string
	}{
		{"an all-unset rubric caps nothing", types.AutonomyRubric{}, sealedNoneCC2, "", ""},
		{"a rubric that names only inapplicable postures caps nothing",
			types.AutonomyRubric{EgressOpen: types.AutonomyL0, SecretsPowerful: types.AutonomyL0, ConfinementCC1: types.AutonomyL0},
			sealedNoneCC2, "", ""},
		{"one applicable field binds",
			types.AutonomyRubric{EgressSealed: types.AutonomyL2}, sealedNoneCC2, types.AutonomyL2, "egress_sealed"},
		{"the minimum wins, not the last",
			types.AutonomyRubric{EgressSealed: types.AutonomyL3, SecretsNone: types.AutonomyL1, ConfinementCC2: types.AutonomyL2},
			sealedNoneCC2, types.AutonomyL1, "secrets_none"},
		{"the minimum wins, not the first",
			types.AutonomyRubric{EgressSealed: types.AutonomyL3, SecretsNone: types.AutonomyL3, ConfinementCC2: types.AutonomyL0},
			sealedNoneCC2, types.AutonomyL0, "confinement_cc2"},
		{"a tie keeps the first field in order",
			types.AutonomyRubric{EgressSealed: types.AutonomyL1, SecretsNone: types.AutonomyL1},
			sealedNoneCC2, types.AutonomyL1, "egress_sealed"},
		// The zero posture is what the api gate holds for a run under no
		// profile. It must select NO cap, or a rubric would bind a run nothing
		// was meant to bind.
		{"the zero posture selects nothing",
			types.AutonomyRubric{EgressSealed: types.AutonomyL0, SecretsNone: types.AutonomyL0, ConfinementCC1: types.AutonomyL0},
			types.AutonomyPosture{}, "", ""},
	} {
		level, bound := FoldAutonomy(tc.rubric, tc.posture)
		if level != tc.wantLevel || bound != tc.wantBound {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", tc.name, level, bound, tc.wantLevel, tc.wantBound)
		}
	}
}

// TestFoldAutonomyBoundNamesARubricField keeps `bound` editable: it is shown to
// an admin as provenance, so every value it can take has to be a field name
// AutonomyRubric.Validate itself reports on.
func TestFoldAutonomyBoundNamesARubricField(t *testing.T) {
	fields := map[string]bool{}
	for _, f := range []string{
		"egress_open", "egress_reviewed", "egress_sealed",
		"secrets_powerful", "secrets_baseline", "secrets_none",
		"confinement_cc1", "confinement_cc2", "confinement_cc3",
	} {
		fields[f] = true
	}
	all := types.AutonomyRubric{
		EgressOpen: types.AutonomyL0, EgressReviewed: types.AutonomyL0, EgressSealed: types.AutonomyL0,
		SecretsPowerful: types.AutonomyL0, SecretsBaseline: types.AutonomyL0, SecretsNone: types.AutonomyL0,
		ConfinementCC1: types.AutonomyL0, ConfinementCC2: types.AutonomyL0, ConfinementCC3: types.AutonomyL0,
	}
	for _, eg := range []types.AutonomyEgressPosture{types.AutonomyEgressOpen, types.AutonomyEgressReviewed, types.AutonomyEgressSealed} {
		for _, se := range []types.AutonomySecretsPosture{types.AutonomySecretsPowerful, types.AutonomySecretsBaseline, types.AutonomySecretsNone} {
			for _, cc := range []types.ConfinementClass{types.CC1, types.CC2, types.CC3} {
				_, bound := FoldAutonomy(all, types.AutonomyPosture{Egress: eg, Secrets: se, Confinement: cc})
				if !fields[bound] {
					t.Errorf("posture %s/%s/%s bound %q, which is not an AutonomyRubric field", eg, se, cc, bound)
				}
			}
		}
	}
}
