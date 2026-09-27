// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The Amazon Bedrock model credential on the autonomy secrets axis (#504).
//
// The secrets axis reads spec.EligibleGrants, and a run's Bedrock credential is
// never one: dispatch hands it over (resolveLLMInjections) long after the
// autonomy gate froze the level. So a run carrying a captured AWS SSO session —
// a session the in-sandbox SDK exchanges for real AWS role credentials — graded
// `secrets=none` on both doors, and a rubric whose secrets_powerful row binds
// launched it on the secrets_none rung. The per-person Azure DevOps lane had the
// same defect one lane over (unionADOEntraLane); this is the same fix.
//
// WHICH VALUE: POWERFUL, by the rubric's existing rule rather than a new one.
// autonomySecrets grades an api_key to a host outside
// composer.safeBaselineDomains powerful, and every Bedrock lane's credential is
// for bedrock-runtime.<region>.amazonaws.com (the captured-SSO lane's grant is
// to portal.sso.<region>.amazonaws.com), neither of which is in that set. The
// baseline set's model hosts are the vendor APIs a coding agent's own key
// reaches; an AWS credential is an identity in the operator's cloud account.
//
// RESIDENCY DOES NOT MOVE IT. The rubric has no residency input: every api_key
// grant is proxy-injected and never resident, and an api_key to a non-baseline
// host is powerful all the same. So the proxy-injected lanes (the bearer, and
// the captured-SSO lane with WARDYN_AWS_SSO_PROXY_INJECT on) grade exactly what
// the resident ones do. What residency changes is the confinement advisory
// (credentialConfinementAdvisory), which is a separate question.

// bedrockCredGrade is what the autonomy gate RESOLVED about this run's Bedrock
// model credential at CREATE, frozen for dispatch — adoEntraGrade's shape, for
// adoEntraGrade's reason: the gate and dispatch resolve the credential from two
// different reads, and between them an admin can store a bearer key, a member
// can capture an AWS sign-in, or the roster can change lanes.
type bedrockCredGrade struct {
	// graded records that a rubric graded this run. False means no rubric bound
	// it, and dispatch then resolves the credential exactly as it always did —
	// a run with no cap cannot be launched above one.
	graded bool
	// host is the Bedrock data-plane host the gate graded a credential for.
	// Empty with graded=true is the explicit "graded, and the run resolved NO
	// Bedrock credential".
	host string
}

// bedrockCredUngraded is the answer for every dispatch lane that runs no
// autonomy gate (adoEntraUngraded names them).
func bedrockCredUngraded() bedrockCredGrade { return bedrockCredGrade{} }

// bedrockCredGradedAs freezes the door's one model-credential resolution for
// the gate that grades it.
func bedrockCredGradedAs(m modelCredentialFacts) bedrockCredGrade {
	return bedrockCredGrade{graded: true, host: m.bedrockHost}
}

// unionBedrockCredential folds the Bedrock model credential into the spec the
// posture is graded on, as the api_key the secrets axis reads.
//
// Secrets axis only. The Bedrock hosts are model-provider hosts resolved from
// global configuration, which the egress axis leaves out by an existing,
// documented decision (autonomyPostureSpec); changing that is not this fix.
//
// ONE grant, to the data-plane host, whichever Bedrock lane resolved: the
// bearer is injected there, and the SigV4 keys and the SSO-minted role
// credentials sign for it. apiKeyToNonBaselineHost reads only the kind and the
// host, so the captured-SSO lane's portal grant would grade identically.
func unionBedrockCredential(out *types.RunPolicySpec, g bedrockCredGrade) {
	if g.host == "" {
		return
	}
	out.EligibleGrants = append(slices.Clone(out.EligibleGrants), types.GrantSpec{
		Kind: types.GrantAPIKey, Scope: mustJSON(map[string]any{"host": g.host}),
	})
}

// bedrockPowerfulSecretCause names the Bedrock credential when it is why the
// secrets axis graded POWERFUL — autonomyPowerfulSecretCause's reason: the
// member's request declared no secret, so "narrow the run's secrets" names
// nothing they can act on without it.
func bedrockPowerfulSecretCause(boundBy []string, g bedrockCredGrade) string {
	if g.host == "" || !slices.Contains(boundBy, "secrets_powerful") {
		return ""
	}
	return " — this run's model credential is an AWS credential for Amazon Bedrock, and that credential is graded a powerful secret"
}

// bedrockCredGradeHolds refuses a dispatch that would hand the run a Bedrock
// model credential the autonomy gate graded it WITHOUT. Returns true when
// dispatch may proceed; false once the run is already FAILED.
//
// Sited ahead of the per-run certificate authority and every grant author, so a
// refused run mints nothing — enforceConfiguredLLMMechanism's placement.
//
// Only the escape direction refuses, as adoEntraGradeHolds does: a run graded
// WITH a Bedrock credential that dispatch now resolves without one is capped
// more strictly than it needs, which harms no one. A different Bedrock lane
// than the one graded is allowed too: every Bedrock lane grades the same value,
// so the frozen level is still the right one.
func (s *Server) bedrockCredGradeHolds(ctx context.Context, run types.AgentRun, g bedrockCredGrade, llm llmTransport) bool {
	if !g.graded || g.host != "" || !llm.bedrockReady {
		return true
	}
	const reason = "autonomy_grade_drift"
	detail := "this run was not launched: its autonomy level was graded WITHOUT an Amazon Bedrock model credential, " +
		"and the configuration changed between then and now so that dispatch would hand it one. Re-launch the run " +
		"so it is graded against the credential it will actually get."
	s.failAndRevoke(ctx, run.ID, types.RunStarting, detail)
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
		run.ID.String(), "failure", mustJSON(map[string]any{
			"error": "bedrock model credential: " + reason, "reason": reason, "detail": detail,
		})))
	return false
}
