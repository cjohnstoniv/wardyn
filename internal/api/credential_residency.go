// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// WHERE A RUN'S MODEL CREDENTIAL LIVES, graded once by the server.
//
// The New Run rail used to state this as unconditional static copy — "Minted at
// launch, injected by the proxy. Never written into the sandbox." — beside a
// second unconditional claim about recording. Both were false on the estate the
// 0.7.4 field report came from, and the first is a FALSE ASSURANCE: it is read
// by the person deciding whether a per-user AWS credential may sit inside a
// shared-kernel container, at the moment they decide.
//
// So the console stops asserting and starts repeating. This file is the one
// place that answers "where does this run's model credential live?", and it
// answers from the lanes that actually RESOLVE (resolveRunLLMLanes ->
// selectedMechanism), never from the roster's declared enum: under a `shared`
// row mechanismSatisfied compares only the coarse provider type, so a declared
// bedrock_bearer row is satisfied by a chain that fell through to the host
// ~/.aws mount or to resident SigV4 keys — every one of which IS resident. An
// enum-to-sentence map in the console would have shipped a new false claim in
// place of the old one.
//
// It is a sibling file rather than more of runs_dispatch_llm_mechanism.go for
// that file's own reason: nothing here resolves a credential or compares a
// declaration. It reads a resolved lane and names a consequence.
package api

import "github.com/cjohnstoniv/wardyn/internal/types"

// modelCredentialResidency is where the MODEL credential of a run lands. The
// vocabulary is deliberately about PLACE, not mechanism: the rail's job is to
// tell a reviewer whether a credential is inside the sandbox they are about to
// grant, and four values cover every row of THREAT-MODEL's resident-secret
// exceptions table.
type modelCredentialResidency string

const (
	// residencyProxy: late-bound and injected on the wire; the sandbox holds a
	// placeholder at most. api_key, the Bedrock bearer token, and a subscription
	// whose token the proxy injects.
	residencyProxy modelCredentialResidency = "proxy"
	// residencySandbox: a live credential lands inside the sandbox for the run's
	// lifetime. Every SigV4 Bedrock lane (SigV4 signs in-process, so it cannot be
	// proxy-injected), and the ~/.claude mount with injection off.
	residencySandbox modelCredentialResidency = "sandbox"
	// residencyImage: Wardyn wires no model credential (the `none` row, BYOA), so
	// it cannot say where the image's own one lives — and must not imply the
	// proxy holds it.
	residencyImage modelCredentialResidency = "image"
	// residencyUnknown: nothing has resolved yet. The absent-row doctrine in one
	// value — the rail says "Resolved at launch." rather than guessing, and the
	// proxy sentence is never reachable from an absence.
	residencyUnknown modelCredentialResidency = "unknown"
)

// modelCredentialFacts is what both surfaces publish: the /setup/status harness
// row (the DEFAULT path, no click) and the preflight response (which overrides
// it once the operator has actually dry-run this body).
//
// MEMBER-SAFE by construction. Mechanism and CredentialSource are already
// declared member-safe on SetupHarnessTool — a lane name and "shared"/"per_user"
// carry no host, no secret name and no access-portal URL — and a residency is
// strictly less than either.
type modelCredentialFacts struct {
	// Mechanism is the lane that RESOLVED when one did, else the declared row's.
	// The two agree on every deployment where the gate admits the run at all;
	// where they differ, the resolved one is what the credential actually is.
	Mechanism        string                   `json:"mechanism,omitempty"`
	Residency        modelCredentialResidency `json:"residency"`
	CredentialSource string                   `json:"credential_source,omitempty"`
	// StagedPlaceholder marks the ONE state where "proxy" is the deployment's
	// stated mode rather than something Wardyn verified: the ~/.claude mount with
	// injection ON. The staged .credentials.json is sanitized to an inert
	// sentinel by an operator-run script (scripts/stage-claude-creds.sh), and the
	// daemon never reads that file back, so the rail names the mount instead of
	// silently promising nothing is mounted at all.
	StagedPlaceholder bool `json:"staged_placeholder,omitempty"`
}

// gradeModelCredential grades where this run's model credential lands.
//
// row/declared are the org roster's answer for the agent (legacy mode has no
// row); lanes + selected/ok are resolveRunLLMLanes' output folded by
// selectedMechanism; subscriptionInject is subscriptionInjectEnabled() — the
// daemon's own posture-and-token-provider predicate, NOT !DisableSubscriptionInject,
// which over-claims injection on a deployment whose provider was never built.
//
// Pure, so the table test can pin every row of THREAT-MODEL's resident-secret
// table against it without a server.
func gradeModelCredential(row types.AgentProvider, declared bool, lanes llmLanes,
	selected types.AgentMechanism, ok, subscriptionInject bool,
) modelCredentialFacts {
	f := modelCredentialFacts{Residency: residencyUnknown}
	if declared {
		f.Mechanism, f.CredentialSource = string(row.Mechanism), string(row.CredentialSource)
	}

	// THE ONE CASE FIXED BY THE ROW RATHER THAN BY A RESOLVED LANE. Under
	// per_user the only admissible lane is the principal's OWN captured AWS SSO
	// session — resolveBedrockAuth refuses to fall through to the operator's
	// bearer/mount/static arms, and mechanismSatisfied admits nothing else — and
	// that lane is resident. So the answer is the same before and after the
	// member signs in, which is precisely the state the rail has to be honest
	// about: a member reading it has not signed in yet.
	if declared && row.CredentialSource == types.CredentialSourcePerUser &&
		row.Mechanism == types.AgentMechanismBedrockSSO {
		f.Residency = residencySandbox
		return f
	}

	if !ok {
		// A declared `none` row matches exactly when no lane fired — that IS what
		// "Wardyn wires no model credential" means (mechanismSatisfied). Every
		// other not-yet-resolved state stays unknown.
		if declared && row.Mechanism == types.AgentMechanismNone {
			f.Residency = residencyImage
		}
		return f
	}
	f.Mechanism = string(selected)

	switch {
	case selected == types.AgentMechanismAnthropicSubscription:
		// Resident ONLY where the ~/.claude mount is the path AND injection is
		// off — THREAT-MODEL's "Subscription ~/.claude mount,
		// WARDYN_SUBSCRIPTION_INJECT=off only" row, whose note that the COMPOSE
		// stack defaults the variable to `off` is why this cannot be assumed.
		// The managed setup-token lane (no mount) has no resident copy to be off
		// about: the sandbox holds the sentinel and the proxy injects the live
		// token, so it is proxy either way.
		if lanes.subscription && !subscriptionInject {
			f.Residency = residencySandbox
		} else {
			f.Residency = residencyProxy
			f.StagedPlaceholder = lanes.subscription
		}
	case selected == types.AgentMechanismBedrockBearer:
		// A static Authorization header — the one Bedrock lane the proxy can
		// substitute on the wire.
		f.Residency = residencyProxy
	case selected.ProviderType() == types.AgentProviderTypeBedrock:
		// Every OTHER Bedrock lane signs SigV4 in-process, so the credential — and,
		// per the "Derived AWS role credentials" row, the role credentials the
		// in-sandbox SDK mints from it — is resident regardless of how it arrived.
		f.Residency = residencySandbox
	default:
		// The api-key lanes: brokered and injected at the proxy's own gateway.
		f.Residency = residencyProxy
	}
	return f
}

// rowFixedResidency is the ONE residency a roster ROW settles on its own, "" for
// every other row — the whole of what GET /setup/status publishes.
//
// Residency is a property of the lane that RESOLVES, and a roster is not a
// resolution: a status handler that dry-ran one would have to invent a request
// body, and New Run never sends the deployment default policy (it sends a
// policy_id or a minimal inline spec, and create folds the run / workspace /
// default integration first, which is where a wizard-built Bedrock integration's
// region and model arrive). That approximation can be confidently wrong in BOTH
// directions — "never written into the sandbox" over a run carrying resident
// SigV4 keys is the exact defect this lane exists to remove — so it is not made.
//
// The per-user Bedrock SSO row is different in kind, not in confidence. It
// admits no other lane at all: resolveBedrockAuth stops at its per-user branch
// rather than falling through to the operator's bearer / mount / static arms,
// and mechanismSatisfied accepts only bedrock_sso under per_user. That lane
// materialises the captured session inside the sandbox and the in-sandbox SDK
// mints resident role credentials from it, whatever policy, workspace or
// integration the run carries. It is also the one state whose precise answer is
// unavailable on demand: Preflight answers 422 for a member who has not signed
// in — which is exactly the person deciding whether to sign in.
//
// A DISABLED row launches nothing, so it says nothing.
func rowFixedResidency(row types.AgentProvider) string {
	if row.Disabled || row.CredentialSource != types.CredentialSourcePerUser ||
		row.Mechanism != types.AgentMechanismBedrockSSO {
		return ""
	}
	return string(residencySandbox)
}
