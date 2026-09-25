// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"encoding/json"
	"slices"
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
			// The Amazon Bedrock model credential (#504): the api layer grades it
			// as an api_key to its host, and neither AWS host is a baseline model
			// API — the credential is an identity in a cloud account.
			{"the Bedrock data-plane credential is powerful",
				[]types.GrantSpec{apiKey("bedrock-runtime.us-east-1.amazonaws.com")}, types.AutonomySecretsPowerful},
			{"the captured AWS SSO portal credential is powerful",
				[]types.GrantSpec{apiKey("portal.sso.us-east-1.amazonaws.com")}, types.AutonomySecretsPowerful},
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
// EVERY field that tied at it, and the two ways nothing binds at all.
//
// The tie rows are the load-bearing ones. A fold that kept the first cause
// would satisfy every single-cause row here and still ship the wrong wire:
// the resolution's bound_by is what an admin edits by, and one name out of a
// three-way tie points at a row they can raise without the level moving.
func TestFoldAutonomyMinimum(t *testing.T) {
	sealedNoneCC2 := types.AutonomyPosture{
		Egress: types.AutonomyEgressSealed, Secrets: types.AutonomySecretsNone, Confinement: types.CC2,
	}
	for _, tc := range []struct {
		name      string
		rubric    types.AutonomyRubric
		posture   types.AutonomyPosture
		wantLevel types.AutonomyLevel
		wantBound []string
	}{
		{"an all-unset rubric caps nothing", types.AutonomyRubric{}, sealedNoneCC2, "", nil},
		{"a rubric that names only inapplicable postures caps nothing",
			types.AutonomyRubric{EgressOpen: types.AutonomyL0, SecretsPowerful: types.AutonomyL0, ConfinementCC1: types.AutonomyL0},
			sealedNoneCC2, "", nil},
		{"one applicable field binds",
			types.AutonomyRubric{EgressSealed: types.AutonomyL2}, sealedNoneCC2, types.AutonomyL2, []string{"egress_sealed"}},
		{"the minimum wins, not the last",
			types.AutonomyRubric{EgressSealed: types.AutonomyL3, SecretsNone: types.AutonomyL1, ConfinementCC2: types.AutonomyL2},
			sealedNoneCC2, types.AutonomyL1, []string{"secrets_none"}},
		{"the minimum wins, not the first",
			types.AutonomyRubric{EgressSealed: types.AutonomyL3, SecretsNone: types.AutonomyL3, ConfinementCC2: types.AutonomyL0},
			sealedNoneCC2, types.AutonomyL0, []string{"confinement_cc2"}},
		{"a two-way tie names both causes",
			types.AutonomyRubric{EgressSealed: types.AutonomyL1, SecretsNone: types.AutonomyL1},
			sealedNoneCC2, types.AutonomyL1, []string{"egress_sealed", "secrets_none"}},
		{"a three-way tie names all three, in field order",
			types.AutonomyRubric{EgressSealed: types.AutonomyL1, SecretsNone: types.AutonomyL1, ConfinementCC2: types.AutonomyL1},
			sealedNoneCC2, types.AutonomyL1, []string{"egress_sealed", "secrets_none", "confinement_cc2"}},
		// A cause that ties with a LOSING cap already in hand: the third field
		// must join the winners, not the field it was compared against.
		{"a tie found after a higher cap replaces it, never appends to it",
			types.AutonomyRubric{EgressSealed: types.AutonomyL2, SecretsNone: types.AutonomyL1, ConfinementCC2: types.AutonomyL1},
			sealedNoneCC2, types.AutonomyL1, []string{"secrets_none", "confinement_cc2"}},
		// A value no AutonomyLevel defines. Unreachable through the API —
		// governanceLimitsRefusal calls AutonomyRubric.Validate at write — so
		// this is the hand-edited column, and it must fail CLOSED: Rank() is -1
		// for an unrecognised value, which is below L0, so it wins the min() and
		// every threshold in the api ladder refuses. It is carried through
		// verbatim rather than clamped to a real rung, because inventing a level
		// nobody authored is how a corrupted row becomes a permitted one.
		{"an undefined level wins the minimum and is carried through verbatim",
			types.AutonomyRubric{EgressSealed: "L9", SecretsNone: types.AutonomyL0},
			sealedNoneCC2, types.AutonomyLevel("L9"), []string{"egress_sealed"}},
		// The zero posture is what the api gate holds for a run under no
		// profile. It must select NO cap, or a rubric would bind a run nothing
		// was meant to bind.
		{"the zero posture selects nothing",
			types.AutonomyRubric{EgressSealed: types.AutonomyL0, SecretsNone: types.AutonomyL0, ConfinementCC1: types.AutonomyL0},
			types.AutonomyPosture{}, "", nil},
	} {
		level, bound := FoldAutonomy(tc.rubric, tc.posture)
		if level != tc.wantLevel || !slices.Equal(bound, tc.wantBound) {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", tc.name, level, bound, tc.wantLevel, tc.wantBound)
		}
	}
}

// TestFoldAutonomyBoundNamesARubricField keeps bound_by editable: it is shown
// to an admin as provenance, so every value it can take has to be a field name
// AutonomyRubric.Validate itself reports on — and, over the rubric below where
// all nine fields tie, every posture has to name all three of its causes.
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
				if len(bound) != 3 {
					t.Errorf("posture %s/%s/%s bound %v; all nine fields tie at L0, so all three causes are due", eg, se, cc, bound)
				}
				for _, b := range bound {
					if !fields[b] {
						t.Errorf("posture %s/%s/%s bound %q, which is not an AutonomyRubric field", eg, se, cc, b)
					}
				}
			}
		}
	}
}
