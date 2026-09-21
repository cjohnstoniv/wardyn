// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Posture-gated autonomy, arithmetic half (0.8 #97). The api half — the gate
// that refuses a request shape and derives a hold — is
// internal/api/runs_autonomy.go; everything here is PURE, so launch and the
// Review dry run fold the same inputs to the same answer by construction
// rather than by two call sites agreeing to.
package composer

import (
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// AutonomyPostureOf grades a run's three-axis posture from its RESOLVED spec
// and the confinement class the run will actually enforce.
//
// The inputs are deliberately the same ones Grade and RequiredConfinementFloor
// already key on (grantIsWriteCapable, apiKeyToNonBaselineHost, beyondBaseline,
// safeBaselineDomains): a posture that disagreed with the risk grade about
// what "powerful" or "beyond baseline" means would put two different answers
// on the same Review screen.
//
// PURELY a function of the spec — never of anything a model claimed about it,
// for the reason Grade's own doc comment states.
func AutonomyPostureOf(spec types.RunPolicySpec, enforced types.ConfinementClass) types.AutonomyPosture {
	return types.AutonomyPosture{
		Egress:      autonomyEgress(spec),
		Secrets:     autonomySecrets(spec),
		Confinement: autonomyConfinement(enforced),
	}
}

// autonomyEgress: OPEN with allow-all or any allowlisted host beyond the safe
// coding-agent baseline, REVIEWED when first-use approval escalates an unknown
// host to a human, SEALED otherwise.
//
// Open beats reviewed, and the order is load-bearing rather than tidy:
// first_use_approval only governs hosts that are NOT on the allowlist, so a
// run already allowlisted to a custom host has that reach whatever the
// unknown-host posture is. Grade draws the same line (its first_use_approval
// item is scored only under a non-trivial allowlist, and is inert under
// allow-all).
func autonomyEgress(spec types.RunPolicySpec) types.AutonomyEgressPosture {
	if spec.AllowAllEgress || len(beyondBaseline(spec.AllowedDomains)) > 0 {
		return types.AutonomyEgressOpen
	}
	if spec.FirstUseApproval.RaisesApproval() {
		return types.AutonomyEgressReviewed
	}
	return types.AutonomyEgressSealed
}

// autonomySecrets: POWERFUL with any write-capable grant, an api_key to a
// non-baseline host, or a git_pat/ssh_key/env_secret; BASELINE with any grant;
// NONE with no grant at all.
//
// The three kinds listed by name are exactly the ones grantIsWriteCapable
// deliberately answers false for while gradeGrant scores them HIGH: their
// scope carries no read/write flag, so flooring confinement on them would
// block every SCM clone on a KVM-less host (see grantIsWriteCapable). That
// argument is about the CC3 floor and does not transfer here — an
// agent-readable SSH private key or a whole-run env secret is unambiguously a
// credential this run could spend unattended, which is the only question this
// axis asks.
func autonomySecrets(spec types.RunPolicySpec) types.AutonomySecretsPosture {
	if len(spec.EligibleGrants) == 0 {
		return types.AutonomySecretsNone
	}
	for _, g := range spec.EligibleGrants {
		if grantIsWriteCapable(g) || apiKeyToNonBaselineHost(g) {
			return types.AutonomySecretsPowerful
		}
		switch g.Kind {
		case types.GrantGitPAT, types.GrantSSHKey, types.GrantEnvSecret:
			return types.AutonomySecretsPowerful
		}
	}
	return types.AutonomySecretsBaseline
}

// autonomyConfinement reads an empty enforced class as CC1, which is what
// AutonomyRubric's own field docs promise. Failing CLOSED on the unknown: CC1
// is the weakest tier, so it selects the rubric's most restrictive confinement
// cap rather than leaving the axis unbound.
func autonomyConfinement(enforced types.ConfinementClass) types.ConfinementClass {
	if enforced == "" {
		return types.CC1
	}
	return enforced
}

// FoldAutonomy folds a rubric against a posture: the level is the MINIMUM over
// the three fields the posture selects, and bound names the field that produced
// it (the rubric's own wire name, so provenance reads back as something an
// admin can edit).
//
// An unset field caps nothing and is skipped, so a rubric that leaves every
// applicable posture unset returns ("", "") — indistinguishable from no rubric
// at all, which is the "empty means unrestricted" rule every GovernanceLimits
// field follows.
//
// Ties keep the FIRST field in the fixed order below. Nothing depends on which
// of two equal caps is named, but something depends on it being the same one
// every time: the audit row and the Review response must agree byte for byte.
func FoldAutonomy(rubric types.AutonomyRubric, posture types.AutonomyPosture) (types.AutonomyLevel, string) {
	var level types.AutonomyLevel
	var bound string
	for _, c := range applicableAutonomyCaps(rubric, posture) {
		if c.level == "" {
			continue
		}
		if bound == "" || c.level.Rank() < level.Rank() {
			level, bound = c.level, c.field
		}
	}
	return level, bound
}

// autonomyCap is one rubric field the posture selects.
type autonomyCap struct {
	field string
	level types.AutonomyLevel
}

// applicableAutonomyCaps returns the three caps this posture selects, in the
// fixed order FoldAutonomy breaks ties on.
//
// A posture axis whose value is not one of its three defined states selects NO
// cap — the zero AutonomyPosture (what resolveRunAutonomy returns for a run
// under no profile) must not silently select the sealed/none/CC1 row and cap a
// run nothing was meant to cap.
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
