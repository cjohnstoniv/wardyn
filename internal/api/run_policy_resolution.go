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
	spec, warns := composer.Clamp(spec, ceiling.Spec, ceiling.Limits)
	kept, grantWarns, code, gerr := s.filterUserGrants(ctx, s.secretOwnerFromRequest(r), spec.AllowedDomains, spec.EligibleGrants)
	if gerr != nil {
		return types.RunPolicySpec{}, nil, runError(code, reasonInlinePolicyInvalid, errPrefix+gerr.Error())
	}
	spec.EligibleGrants = kept
	warns = append(warns, grantWarns...)
	drops := make([]capDrop, 0, len(grantWarns))
	for _, gw := range grantWarns {
		drops = append(drops, capDrop{reason: authz.ReasonGrantPairingNotEligible, detail: gw})
	}
	capWarns, capDrops, cerr := s.narrowUserInlinePolicy(ctx, s.secretOwnerFromRequest(r), &spec)
	if cerr != nil {
		return types.RunPolicySpec{}, nil, capabilityRunRefusal(cerr)
	}
	warns = append(warns, capWarns...)
	if !dryRun {
		s.auditUserPolicyDrops(ctx, r, append(drops, capDrops...))
	}
	return spec, warns, nil
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
			var refusal *runRefusal
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
			return types.RunPolicySpec{}, nil, nil, policySourceRecord{}, runError(code, reasonInlinePolicyInvalid, "invalid inline_policy: "+err.Error())
		}
		// The size half for the inline arm, the same helper the stored arm calls
		// below: composer.Clamp bounds a member's disk_mib by the PROFILE, but
		// the org's default_disk_mib/max_disk_mib are dispatch's and reach no
		// preview at all without this. A no-op on launch (see the helper).
		clampWarnings = append(clampWarnings, s.boundResources(ctx, r, &spec, ceiling, dryRun)...)
		clampWarnings = append(clampWarnings, s.boundUIApps(ctx, r, &spec, ceiling, dryRun)...)
		// Audit the use of an inline (non-stored) policy. The run id is not yet
		// minted at this point, so this event carries a nil run id (like the
		// secret.* admin events); the subsequent run.create event records
		// inline_policy=true bound to the run id for correlation. Skipped for a
		// preflight dry-run (see the doc comment).
		if !dryRun {
			s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
				"policy.inline.apply", "", "success", mustJSON(map[string]any{
					"min_confinement_class": spec.MinConfinementClass,
					"workspace_mounts":      len(spec.WorkspaceMounts),
					"eligible_grants":       len(spec.EligibleGrants),
				})))
		}
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
	storedWarns := append([]string(nil), ceiling.Warnings...)
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
	if policyID != nil && ceiling.Profile != nil && !s.runUngoverned(r.Context()) {
		var warns []string
		var refusal *runRefusal
		spec, warns, refusal = s.boundRunUserSpec(ctx, r, spec, ceiling, "invalid policy: ", dryRun)
		if refusal != nil {
			return types.RunPolicySpec{}, nil, nil, policySourceRecord{}, refusal
		}
		storedWarns = append(storedWarns, warns...)
	}
	// The size half, for the arm composer.Clamp never reaches. The block above
	// runs only for a member who SELECTED a stored row (policyID != nil), but the
	// commonest member request there is carries no policy at all — and its spec is
	// then the profile's OWN ceiling, which an admin may well have written wider
	// than the limit standing beside it. That member previewed a scratch size
	// their run does not get, and a dispatch-side log line is not a disclosure to
	// them. Both arms call the SAME helper (runs_dispatch_ceiling.go).
	storedWarns = append(storedWarns, s.boundResources(ctx, r, &spec, ceiling, dryRun)...)
	storedWarns = append(storedWarns, s.boundUIApps(ctx, r, &spec, ceiling, dryRun)...)
	if refusal := patDuplicatesRunRefusal("invalid policy: ", spec); refusal != nil {
		return types.RunPolicySpec{}, nil, nil, policySourceRecord{}, refusal
	}
	if code, err := s.validateRunSecretRefs(ctx, s.secretOwnerFromRequest(r), runIdentitySubject(ctx, principalFromRequest(r)), spec, credentials); err != nil {
		return types.RunPolicySpec{}, nil, nil, policySourceRecord{}, runError(code, reasonInlinePolicyInvalid, "invalid policy: "+err.Error())
	}
	return spec, policyID, storedWarns, source, nil
}

func patDuplicatesRunRefusal(prefix string, spec types.RunPolicySpec) *runRefusal {
	if err := validatePATNarrowedDuplicates(spec.EligibleGrants); err != nil {
		return runError(http.StatusUnprocessableEntity, reasonInlinePolicyInvalid, prefix+err.Error())
	}
	return nil
}
