// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func (s *Server) boundRunUserSpec(ctx context.Context, r *http.Request, spec types.RunPolicySpec, ceiling governanceCeiling, errPrefix string, dryRun bool) (types.RunPolicySpec, []string, *runRefusal) {
	spec, warns, drops, refusal := s.boundRunUserSpecDrops(ctx, r, spec, ceiling, errPrefix)
	if refusal == nil && !dryRun {
		s.auditUserPolicyDrops(ctx, r, drops)
	}
	return spec, warns, refusal
}

// boundRunUserSpecDrops is the member pipeline without its audit: the bounded
// spec, the warnings and the structured drops the audit is made from.
func (s *Server) boundRunUserSpecDrops(ctx context.Context, r *http.Request, spec types.RunPolicySpec, ceiling governanceCeiling, errPrefix string) (types.RunPolicySpec, []string, []capDrop, *runRefusal) {
	domains, literalWarns := dropUncoveredPrivateLiterals(spec.AllowedDomains, ceiling.Spec)
	spec.AllowedDomains = domains
	spec, warns := composer.ClampMember(spec, ceiling.Spec, ceiling.Limits, ceiling.authoredSize(), ceiling.memberSize())
	warns = append(literalWarns, warns...)
	kept, grantWarns, code, gerr := s.filterUserGrants(ctx, s.secretOwnerFromRequest(r), spec.AllowedDomains, spec.EligibleGrants)
	if gerr != nil {
		return types.RunPolicySpec{}, nil, nil, runError(code, reasonInlinePolicyInvalid, errPrefix+gerr.Error())
	}
	spec.EligibleGrants = kept
	warns = append(warns, grantWarns...)
	drops := make([]capDrop, 0, len(grantWarns))
	for _, gw := range grantWarns {
		drops = append(drops, capDrop{reason: authz.ReasonGrantPairingNotEligible, detail: gw})
	}
	capWarns, capDrops, cerr := s.narrowUserInlinePolicy(ctx, s.secretOwnerFromRequest(r), &spec)
	if cerr != nil {
		return types.RunPolicySpec{}, nil, nil, capabilityRunRefusal(cerr)
	}
	return spec, append(warns, capWarns...), append(drops, capDrops...), nil
}

func (s *Server) resolveRunPolicyFacts(ctx context.Context, r *http.Request, req *createRunRequest, dryRun, credentials bool) (types.RunPolicySpec, *uuid.UUID, []string, policySourceRecord, *runRefusal) {
	// The caller's OWN secret names, resolved at most once for this whole
	// resolution rather than once per eligible_grant at each of the three sites
	// that ask (filterUserGrants' 6c arm, narrowUserInlinePolicy's ownership
	// exemption, validateInlineSecretRefs' unknown-name arm). eligible_grants is
	// request-body-sized and uncapped, so an unmemoized read per grant let one
	// member choose how many store round trips this handler made — see
	// ownedSecretMemo, which is capBatch's law applied to the list capBatch did
	// not cover.
	ctx = withOwnedSecretMemo(ctx)

	// XOR: a run picks EITHER a stored policy_id OR an inline policy, never both.
	if req.InlinePolicy != nil && req.PolicyID != nil {
		return types.RunPolicySpec{}, nil, nil, policySourceRecord{}, runError(http.StatusBadRequest, reasonInlinePolicyXOR, "specify either policy_id or inline_policy, not both")
	}

	// This principal's ceiling — resolved BEFORE either branch, because both
	// need it and a create must never resolve two different ceilings for one
	// request. A resolver failure is never fail-open: writeCeilingError 500s a
	// store error and 403s an unanswerable group snapshot (see effectiveCeiling).
	ceiling, ceilErr := s.effectiveCeiling(ctx)
	if ceilErr != nil {
		return types.RunPolicySpec{}, nil, nil, policySourceRecord{}, ceilingRunRefusal(ceilErr)
	}

	// Inline path: validate structurally (same validator as a stored policy) then
	// validate any inline secret references. On success attach with a nil id.
	if req.InlinePolicy != nil {
		// A member MAY author an inline_policy — it is not refused, it is
		// CLAMPED below to the ceiling an admin set FOR THEM, so a member can
		// never smuggle wider egress/grants/confinement than the operator
		// already allows. An admin is the ceiling-setting authority and is
		// left unclamped. (This supersedes the earlier operator-only gate: a
		// clamp bounds a member without blocking them.)
		spec := *req.InlinePolicy
		// Count-capped first, before any narrowing. validatePolicySpec below
		// applies the same cap, but it runs AFTER boundUserSpec, and
		// boundUserSpec's narrowing is the per-entry work an unbounded
		// allowed_domains buys with a single request body (see
		// maxAllowedDomainsPerSpec and capBatch). A cap that only fires
		// afterwards bounds the stored policy and not the request.
		if err := validateAllowedDomainsCount(spec.AllowedDomains); err != nil {
			return types.RunPolicySpec{}, nil, nil, policySourceRecord{}, runError(http.StatusBadRequest, reasonInlinePolicyInvalid, "invalid inline_policy: "+err.Error())
		}
		// The policy as authored, recorded BEFORE the env-secret posture and the
		// member clamp change it (policySourceRecord stamps it redacted).
		source := newPolicySourceRecord(policyKindInline, nil, "", nil, !ceiling.Operator, spec)
		// The person's New Run edits, BEFORE the posture and the clamp below, so
		// everything an edit adds is bounded like anything else they authored.
		spec, edits, refusal := s.applyRunOverrides(ctx, r, spec, req)
		if refusal != nil {
			return types.RunPolicySpec{}, nil, nil, policySourceRecord{}, refusal
		}
		clampWarnings := append([]string(nil), ceiling.Warnings...)
		// env_secret's admin-only posture, applied FIRST and unconditionally for
		// a non-operator — it is a role check, not a ceiling check, so it must
		// not sit behind the ceiling-scoped gate below (see
		// userEnvSecretIsAdminOnly).
		spec, envWarns := s.boundEnvSecretPosture(ctx, r, spec, dryRun)
		clampWarnings = append(clampWarnings, envWarns...)
		// Deliberately isOperator (three-tier doctrine, internal/auth/oidc's
		// RoleSecurityAdmin), in lockstep with denyUserRequest: a security
		// admin's OWN run is clamped like anyone else's. They author the
		// ceiling; they do not stand outside it.
		if !s.runUngoverned(r.Context()) {
			// A member's inline_policy can never smuggle wider grants/
			// egress/confinement than THEIR ceiling allows — the governance
			// profile an admin assigned them, or Config.DefaultPolicy when
			// nobody assigned one.
			// Bounded BEFORE validation/resolution — never the fully-resolved
			// spec, which would strip the LATER admin-authored additions
			// (applyRequiredSecretGrant, applyIntegrationRequirement) folded in
			// by runs.go/preflight.go AFTER this function returns.
			var warns []string
			spec, warns, refusal = s.boundRunUserSpec(ctx, r, spec, ceiling, "invalid inline_policy: ", dryRun)
			if refusal != nil {
				return types.RunPolicySpec{}, nil, nil, policySourceRecord{}, refusal
			}
			clampWarnings = append(clampWarnings, warns...)
		}
		if refusal := patDuplicatesRunRefusal("invalid inline_policy: ", spec); refusal != nil {
			return types.RunPolicySpec{}, nil, nil, policySourceRecord{}, refusal
		}
		if err := validatePolicySpec(spec); err != nil {
			return types.RunPolicySpec{}, nil, nil, policySourceRecord{}, runError(http.StatusBadRequest, specRefusalReason(err, reasonInlinePolicyInvalid), "invalid inline_policy: "+err.Error())
		}
		if code, err := s.validateRunSecretRefs(ctx, s.secretOwnerFromRequest(r), runIdentitySubject(ctx, principalFromRequest(r)), spec, credentials); err != nil {
			return types.RunPolicySpec{}, nil, nil, policySourceRecord{}, runSecretRefsRefusal("invalid inline_policy: ", code, err)
		}
		// The CPU and memory half for the inline arm, the same helper the stored arm
		// calls below. The main drive is sized by stepMainDrive, for every door.
		s.boundResources(ctx, r, &spec, ceiling)
		clampWarnings = append(clampWarnings, s.boundUIApps(ctx, r, &spec, ceiling, dryRun)...)
		clampWarnings = append(clampWarnings, boundBackgroundIdleStop(*req, &spec)...)
		if refusal := noRepositoriesResolvedRefusal(*req, spec); refusal != nil {
			return types.RunPolicySpec{}, nil, nil, policySourceRecord{}, refusal
		}
		// Audit the use of an inline (non-stored) policy. The run id is not yet
		// minted at this point, so this event carries a nil run id (like the
		// secret.* admin events); the subsequent run.create event records
		// inline_policy=true bound to the run id for correlation. Skipped for a
		// preflight dry-run (see the doc comment).
		if !dryRun {
			source.applyAudits = append(source.applyAudits, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
				"policy.inline.apply", "", "success", mustJSON(map[string]any{
					"min_confinement_class": spec.MinConfinementClass,
					"workspace_mounts":      len(spec.WorkspaceMounts),
					"eligible_grants":       len(spec.EligibleGrants),
				})))
		}
		edits.finish(&source, spec)
		return spec, nil, clampWarnings, source, nil
	}

	// Stored/default path: resolve, then validate secret references the SAME way
	// the inline branch does (one call, no duplicated logic). The no-policy
	// default is now the CALLER's ceiling rather than the deployment's
	// (resolvePolicy), and a member-SELECTED stored row is bounded below.
	spec, policyID, origin, err := s.resolvePolicy(ctx, req.PolicyID, ceiling)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return types.RunPolicySpec{}, nil, nil, policySourceRecord{}, runError(http.StatusBadRequest, reasonPolicyIDNotFound, "policy_id not found")
		}
		return types.RunPolicySpec{}, nil, nil, policySourceRecord{}, runServerError("resolve policy", err)
	}
	// The starting policy, before the env-secret posture and the member clamp
	// below change it: a stored row, or the caller's own ceiling.
	source := newPolicySourceRecord(origin.kind, policyID, origin.name, origin.updatedAt, !ceiling.Operator, spec)
	// A document's partial policy, laid onto the default (policyID is nil: the
	// overlay never rides beside one) with its own keys bounded, before the
	// person's card edits, which edit the policy the overlay made.
	spec, overlayWarns, refusal := s.applyPolicyOverlay(ctx, r, spec, req, ceiling, dryRun, &source)
	if refusal != nil {
		return types.RunPolicySpec{}, nil, nil, policySourceRecord{}, refusal
	}
	// The person's New Run edits, before the posture and the clamp (see the
	// inline branch).
	spec, edits, refusal := s.applyRunOverrides(ctx, r, spec, req)
	if refusal != nil {
		return types.RunPolicySpec{}, nil, nil, policySourceRecord{}, refusal
	}
	storedWarns := append(append([]string(nil), ceiling.Warnings...), overlayWarns...)
	// Same unconditional env_secret posture the inline branch applies, in the
	// same position, and it is the half the scoped clamp below CANNOT carry: the
	// clamp fires only for an ASSIGNED member selecting a row, while this rule
	// binds every non-operator on every stored AND default resolution. Without
	// it an unassigned member — the default posture — selected a stored row (or
	// took the deployment default) carrying an env_secret grant and the
	// operator's raw secret value landed in their sandbox env at
	// resolveEnvSecretGrants, contradicting three docs that state the control
	// without qualification.
	spec, envWarns := s.boundEnvSecretPosture(ctx, r, spec, dryRun)
	storedWarns = append(storedWarns, envWarns...)
	// The central escape: a stored policy row is admin-authored CONTENT,
	// but ANY signed-in caller may put one on their own run (policy_id is
	// open until an admin enforces the `policy` kind, and it has to stay that
	// way by default — gating it removes a legitimate feature and pushes
	// members onto hand-authored inline specs). So a member
	// selecting a wide row got a wide run, entirely past the clamp their own
	// inline_policy would have hit. One rule falls out: member-selected content
	// is bounded by the member's ceiling whether it arrived as a body or as a
	// row id.
	//
	// Scoped to policyID != nil && ceiling.Profile != nil, and both halves are
	// load-bearing:
	//
	//   - policyID != nil: the no-policy default is already the ceiling itself
	//     (resolvePolicy above), and clamping a spec against itself is at best a
	//     no-op and at worst order-dependent — composer.Clamp is not a lattice
	//     meet.
	//   - Profile != nil: the UNCONDITIONAL variant is deliberately not chosen.
	//     Stored policies are routinely wider than a minimal DefaultPolicy, so
	//     clamping every member's selection to it would shred deployments that
	//     have never heard of this feature. The clamp fires exactly when an
	//     admin has DECLARED this principal's ceiling. The residual is named and
	//     accepted: an unassigned member still selects stored rows unclamped,
	//     which is today's behaviour, and the deployment-wide opt-in is an
	//     `all`-subject assignment.
	//
	// The FULL member pipeline, never Clamp alone: clampGrants BOUNDS a grant by
	// the ceiling entry whose pairing it names, but it does not GATE on the
	// pairing — an unpaired grant is kept (bounded by the strictest same-kind
	// entry) rather than dropped. So a Clamp-only stored branch would hand a
	// member every operator-secret pairing the row happened to carry — the "one
	// rule whether body or row id" claim would be false at exactly the
	// grant-bearing rows, which are the ones that matter. Dropping the unlisted
	// pairing is filterUserGrants' job, stage 2.
	//
	// The person's New Run edits do not change which spec is clamped as a whole:
	// wherever that is not the rule (the no-policy default, which IS the ceiling,
	// and a stored row with no declared profile), only the additions are bounded
	// (boundOverrideAdds). Clamping the default as a whole would drop the
	// operator's workspace_mounts and onboarded workspace_repos for a person who
	// only narrowed something.
	if !s.runUngoverned(r.Context()) {
		var warns []string
		switch {
		case policyID != nil && ceiling.Profile != nil:
			spec, warns, refusal = s.boundRunUserSpec(ctx, r, spec, ceiling, "invalid policy: ", dryRun)
		default:
			spec, warns, refusal = s.boundOverrideAdds(ctx, r, spec, edits, ceiling, dryRun)
		}
		if refusal != nil {
			return types.RunPolicySpec{}, nil, nil, policySourceRecord{}, refusal
		}
		storedWarns = append(storedWarns, warns...)
	}
	// The CPU and memory half, for the arm composer.Clamp never reaches. The block
	// above runs only for a member who SELECTED a stored row (policyID != nil), but
	// the commonest member request there is carries no policy at all — and its spec
	// is then the profile's OWN ceiling, which an admin may well have written wider
	// than the limit standing beside it. Both arms call the SAME helper
	// (runs_dispatch_ceiling.go).
	s.boundResources(ctx, r, &spec, ceiling)
	storedWarns = append(storedWarns, s.boundUIApps(ctx, r, &spec, ceiling, dryRun)...)
	storedWarns = append(storedWarns, boundBackgroundIdleStop(*req, &spec)...)
	if refusal := noRepositoriesResolvedRefusal(*req, spec); refusal != nil {
		return types.RunPolicySpec{}, nil, nil, policySourceRecord{}, refusal
	}
	if refusal := patDuplicatesRunRefusal("invalid policy: ", spec); refusal != nil {
		return types.RunPolicySpec{}, nil, nil, policySourceRecord{}, refusal
	}
	if code, err := s.validateRunSecretRefs(ctx, s.secretOwnerFromRequest(r), runIdentitySubject(ctx, principalFromRequest(r)), spec, credentials); err != nil {
		return types.RunPolicySpec{}, nil, nil, policySourceRecord{}, runSecretRefsRefusal("invalid policy: ", code, err)
	}
	if !dryRun {
		source.applyAudits = append(source.applyAudits, s.auditPolicyOverlay(r, source.Overlay)...)
	}
	edits.finish(&source, spec)
	return spec, policyID, storedWarns, source, nil
}

// Stored policies may predate write-time validation; duplicate narrowed PATs
// must still refuse before a run is created.
func patDuplicatesRunRefusal(prefix string, spec types.RunPolicySpec) *runRefusal {
	if err := validatePATNarrowedDuplicates(spec.EligibleGrants); err != nil {
		return runError(http.StatusUnprocessableEntity, reasonInlinePolicyInvalid, prefix+err.Error())
	}
	return nil
}
