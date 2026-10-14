// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Posture-gated autonomy, arithmetic half. The api half — the gate that refuses
// a request shape and derives a hold — is internal/api/runs_autonomy.go;
// everything here is PURE, so launch and the Review dry run fold the same
// inputs to the same answer by construction.
package composer

import (
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// AutonomyPostureOf grades a run's three-axis posture from its RESOLVED spec
// and the confinement class the run will actually enforce. It keys on the same
// inputs as Grade and RequiredConfinementFloor, so posture and risk grade never
// disagree about what "powerful" or "beyond baseline" means on the same Review
// screen. Purely a function of the spec, never of anything a model claimed.
func AutonomyPostureOf(spec types.RunPolicySpec, enforced types.ConfinementClass, b Baseline) types.AutonomyPosture {
	return types.AutonomyPosture{
		Egress:      autonomyEgress(spec, b),
		Secrets:     autonomySecrets(spec, b),
		Confinement: autonomyConfinement(enforced),
	}
}

// autonomyEgress: OPEN with allow-all or any allowlisted host beyond the safe
// coding-agent baseline, REVIEWED when first-use approval escalates an unknown
// host to a human, SEALED otherwise. Open beats reviewed: first_use_approval
// only governs hosts NOT on the allowlist, so an already-allowlisted custom
// host has that reach regardless of the unknown-host posture.
func autonomyEgress(spec types.RunPolicySpec, b Baseline) types.AutonomyEgressPosture {
	if spec.AllowAllEgress || len(b.beyond(spec.AllowedDomains)) > 0 {
		return types.AutonomyEgressOpen
	}
	if spec.FirstUseApproval.RaisesApproval() {
		return types.AutonomyEgressReviewed
	}
	return types.AutonomyEgressSealed
}

// autonomySecrets: POWERFUL with any write-capable grant, an api_key to a
// non-baseline host, or a git_pat/ssh_key/env_secret/file_secret; BASELINE with any grant;
// NONE with no grant at all. The three named kinds are ones grantIsWriteCapable
// deliberately treats as not write-capable (to avoid flooring confinement and
// blocking SCM clones), but each is still a credential this run could spend
// unattended, which is the only question this axis asks.
func autonomySecrets(spec types.RunPolicySpec, b Baseline) types.AutonomySecretsPosture {
	if len(spec.EligibleGrants) == 0 {
		return types.AutonomySecretsNone
	}
	for _, g := range spec.EligibleGrants {
		if grantIsWriteCapable(g) || apiKeyToNonBaselineHost(g, b) {
			return types.AutonomySecretsPowerful
		}
		switch g.Kind {
		case types.GrantGitPAT, types.GrantSSHKey, types.GrantEnvSecret, types.GrantFileSecret:
			return types.AutonomySecretsPowerful
		}
	}
	return types.AutonomySecretsBaseline
}

// autonomyConfinement reads an empty enforced class as CC1 (fails closed): the
// weakest tier selects the rubric's most restrictive cap rather than leaving
// the axis unbound.
func autonomyConfinement(enforced types.ConfinementClass) types.ConfinementClass {
	if enforced == "" {
		return types.CC1
	}
	return enforced
}

// FoldAutonomy folds a rubric against a posture: the level is the MINIMUM over
// the three fields the posture selects, and boundBy names EVERY field tied at
// that minimum (its rubric wire name), not just the first — so an admin who
// raises one capping row can see the level not move because another field tied
// it. An unset field caps nothing and is skipped, so an all-unset rubric
// returns ("", nil), matching the "empty means unrestricted" rule every
// GovernanceLimits field follows. Order is applicableAutonomyCaps's fixed field
// order, so the audit row and the Review response agree byte for byte.
func FoldAutonomy(rubric types.AutonomyRubric, posture types.AutonomyPosture) (types.AutonomyLevel, []string) {
	var level types.AutonomyLevel
	var boundBy []string
	for _, c := range applicableAutonomyCaps(rubric, posture) {
		switch {
		case c.level == "":
		case boundBy == nil || c.level.Rank() < level.Rank():
			level, boundBy = c.level, []string{c.field}
		case c.level.Rank() == level.Rank():
			boundBy = append(boundBy, c.field)
		}
	}
	return level, boundBy
}

// autonomyCap is one rubric field the posture selects.
type autonomyCap struct {
	field string
	level types.AutonomyLevel
}

// applicableAutonomyCaps returns the three caps this posture selects, in the
// fixed order FoldAutonomy lists tied causes in. A posture axis whose value is
// not one of its three defined states selects no cap, so the zero
// AutonomyPosture (a run under no profile) caps nothing.
func applicableAutonomyCaps(rubric types.AutonomyRubric, posture types.AutonomyPosture) []autonomyCap {
	var caps []autonomyCap
	switch posture.Egress {
	case types.AutonomyEgressOpen:
		caps = append(caps, autonomyCap{"egress_open", rubric.EgressOpen})
	case types.AutonomyEgressReviewed:
		caps = append(caps, autonomyCap{"egress_reviewed", rubric.EgressReviewed})
	case types.AutonomyEgressSealed:
		caps = append(caps, autonomyCap{"egress_sealed", rubric.EgressSealed})
	}
	switch posture.Secrets {
	case types.AutonomySecretsPowerful:
		caps = append(caps, autonomyCap{"secrets_powerful", rubric.SecretsPowerful})
	case types.AutonomySecretsBaseline:
		caps = append(caps, autonomyCap{"secrets_baseline", rubric.SecretsBaseline})
	case types.AutonomySecretsNone:
		caps = append(caps, autonomyCap{"secrets_none", rubric.SecretsNone})
	}
	switch posture.Confinement {
	case types.CC1:
		caps = append(caps, autonomyCap{"confinement_cc1", rubric.ConfinementCC1})
	case types.CC2:
		caps = append(caps, autonomyCap{"confinement_cc2", rubric.ConfinementCC2})
	case types.CC3:
		caps = append(caps, autonomyCap{"confinement_cc3", rubric.ConfinementCC3})
	}
	return caps
}
