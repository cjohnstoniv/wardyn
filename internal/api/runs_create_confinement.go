// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// enforcedConfinement is the PURE confinement math both the launch path and the
// preflight dry-run run: the requested class when set (never WEAKER than the
// policy minimum), else the STRONGEST class advertised is that meets
// the policy minimum (never the minimum itself; see strongestAdvertisedAtOrAbove),
// then the deterministic BLAST-RADIUS floor.
//
// advertised is the runner's advertised confinement set, passed IN rather than
// fetched here: this function stays pure (no runner, no context) so preflight
// can share it verbatim (TestPreflightMirrorsLaunchGates encodes that split).
// Callers with no runner (or an unread one) pass nil — strongestAdvertisedAtOrAbove
// then leaves the default at the policy minimum, byte-identical to before this
// rule existed.
//
// That floor is defense-in-depth and applies to EVERY run (manual wizard and
// direct API callers, not just composed ones): a run holding powerful
// credentials (write-capable, or a third-party/production api_key) MUST run in
// the strongest sandbox so a sandbox escape can't carry those credentials out to
// your host. A host that cannot provide CC3 then fails closed at the caller's
// capability check rather than running the workload under-confined (invariant 5).
//
// It writes no HTTP so preflight can share it: the returned error's text IS the
// 422 body both callers send (the wizard test hardcodes that string), and
// preflight deliberately skips the caller-side gates that follow it.
func enforcedConfinement(spec types.RunPolicySpec, reqCC types.ConfinementClass, advertised []types.ConfinementClass) (types.ConfinementClass, error) {
	// Assigned in BOTH branches below — declared without a value so the dead
	// store staticcheck flags (SA4006) cannot come back: the floor is no longer
	// the default, it is only the lower bound the default is chosen at or above.
	var enforced types.ConfinementClass
	if reqCC != "" {
		if !confinementGE(reqCC, spec.MinConfinementClass) {
			return "", fmt.Errorf("confinement_class %s is weaker than the policy minimum %s",
				reqCC, spec.MinConfinementClass)
		}
		enforced = reqCC
	} else {
		enforced = strongestAdvertisedAtOrAbove(advertised, spec.MinConfinementClass)
	}
	if composer.RequiredConfinementFloor(spec) == types.CC3 && !confinementGE(enforced, types.CC3) {
		enforced = types.CC3
	}
	return enforced, nil
}

// credentialConfinementBelowFloor is the ONE value the run.create audit row's
// credential_confinement field carries today — a closed vocabulary, like
// confinement_source's requested/defaulted, so an incident review can GROUP on
// it instead of parsing free text. Absent (field omitted) covers everything
// else: no SSO-delivered credential, or one whose enforced confinement already
// meets CC3.
const credentialConfinementBelowFloor = "below_floor"

// credentialConfinementAdvisory is the WARN-never-refuse counterpart to the
// blast-radius floor above (0.8 #150). A stored AWS SSO credential is
// delivered to the sandbox at DISPATCH — after enforcedConfinement has already
// resolved the class above — so it is never itself an eligible grant and
// composer.RequiredConfinementFloor never sees it: folding it in there would
// change ENFORCEMENT, which is explicitly out of scope here. Silence would
// leave a run holding a captured AWS identity under a confinement class
// nothing chose for that reason; this says so instead of raising the floor for
// it, because a host that can only ever offer the weakest class must still be
// able to launch — adding a refusal here would break every single-class
// deployment.
//
// ssoDelivered is the caller's own answer to "is this run's model credential
// the captured-AWS-SSO lane" (the chosen provider's kind is bedrock_sso) —
// taken once by the caller from the SAME choice the model-credential grade
// already made (modelCredentialFacts.Kind), never re-derived here. Pure, with the same purity contract as enforcedConfinement,
// so it is called from both the launch path and the preflight path off the
// same resolved body — the two can never disagree about whether a run carries
// the advisory.
//
// spec is currently unread: it rides along for the same reason
// enforcedConfinement takes the whole spec rather than just the fields it
// needs today — a future policy-level exception would have somewhere to read
// from without a signature change.
func credentialConfinementAdvisory(spec types.RunPolicySpec, enforced types.ConfinementClass, ssoDelivered bool) string {
	if !ssoDelivered || confinementGE(enforced, types.CC3) {
		return ""
	}
	return fmt.Sprintf(credentialConfinementAdvisorySentence, enforced)
}

// appendCredentialConfinementAdvisory is the two call sites' shared plumbing
// around credentialConfinementAdvisory (runs.go's handleCreateRun and
// preflight.go's handlePreflightRun): append the sentence to warnings when it
// fires, and report whether it did, since the create path's audit row needs
// that same answer for credential_confinement. kind is the chosen provider's
// modelCredentialFacts.Kind both callers already have in hand.
func appendCredentialConfinementAdvisory(warnings []string, spec types.RunPolicySpec, enforced types.ConfinementClass, kind string) ([]string, bool) {
	advisory := credentialConfinementAdvisory(spec, enforced, kind == string(types.ModelProviderBedrockSSO))
	if advisory == "" {
		return warnings, false
	}
	return append(warnings, advisory), true
}

// resolveEnforcedConfinement resolves the run's confinement class and gates it
// against what the runner and identity provider can actually deliver (invariant
// 5, fail closed). The request value wins when set (never WEAKER than the
// policy minimum); an unspecified request defaults to the STRONGEST class the
// runner advertises at or above the policy minimum (strongestAdvertisedAtOrAbove).
// The deterministic BLAST-RADIUS floor then raises powerful-credential
// runs to CC3, and the runner must advertise the EXACT enforced class
// (membership, not rank: a Kata-only host advertises [CC1, CC3] with no
// CC2, so a rank check would pass a CC2 demand and fail later with a raw docker
// error). Writes the HTTP error itself and returns ok=false on any refusal.
// Extracted verbatim from handleCreateRun.
func (s *Server) resolveEnforcedConfinement(ctx context.Context, w http.ResponseWriter, spec types.RunPolicySpec, reqCC types.ConfinementClass) (types.ConfinementClass, bool) {
	// Capabilities are read BEFORE the pure math now, not after: the default
	// branch needs the advertised set to pick the strongest class rather than
	// just the policy minimum. Still one read, still fail-closed on a Capabilities
	// error — only its place in the function moved.
	var caps runner.Capabilities
	if s.cfg.Runner != nil {
		var cerr error
		caps, cerr = s.cfg.Runner.Capabilities(ctx)
		if cerr != nil {
			writeErrorReason(w, http.StatusServiceUnavailable, reasonRunnerCapabilitiesUnavailable, loggedMsg(ctx, "runner capabilities unavailable", cerr))
			return "", false
		}
	}

	enforced, err := enforcedConfinement(spec, reqCC, caps.ConfinementClasses)
	if err != nil {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonConfinementClassConflict, err.Error())
		return "", false
	}

	// Confinement gating: refuse to schedule a run whose confinement class the
	// runner cannot structurally enforce (invariant 5, fail closed).
	if s.cfg.Runner != nil {
		// Membership, not rank: CC2 (gVisor/runsc) and CC3 (Kata/krun) resolve to
		// INDEPENDENT runtimes, so a host can advertise a non-contiguous set (e.g. a
		// Kata-only host advertises [CC1, CC3], no CC2). A rank check —
		// confinementGE(best, enforced) — would let a CC2 demand pass on that host
		// because CC3 outranks CC2, then fail at sandbox create with a raw docker
		// error. Require the exact enforced class to be advertised. enforced=="" means
		// no class is required (policy floor unset, no request) ⇒ any runner passes.
		// Also the fail-closed net for a floor (explicit or defaulted) the runner
		// cannot enforce at all — strongestAdvertisedAtOrAbove falls back to the
		// floor unchanged when nothing advertised meets it, so THIS check is what
		// still 422s that case exactly as it did before the default rule existed.
		if enforced != "" && !slices.Contains(caps.ConfinementClasses, enforced) {
			writeErrorReason(w, http.StatusUnprocessableEntity, reasonConfinementClassUnsupported, fmt.Sprintf(
				"runner %q cannot enforce confinement_class %s (available: %s)",
				caps.Driver, enforced, classesOrNone(caps.ConfinementClasses)))
			return "", false
		}
	}

	// Reject cloud_sts grants up front: the embedded provider hard-requires
	// SPIRE for them (invariant 5). We check via the identity provider so the
	// spire provider can later accept them without an API change.
	if checker, ok := s.cfg.Identity.(grantChecker); ok {
		if err := checker.CheckGrants(spec.EligibleGrants); err != nil {
			writeErrorReason(w, http.StatusUnprocessableEntity, reasonRunGrantsRequireSPIRE,
				"policy requires the spire identity provider: "+err.Error())
			return "", false
		}
	}
	return enforced, true
}
