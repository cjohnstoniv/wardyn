// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// resolveRunPolicy resolves the RunPolicySpec + policy id to attach to a run,
// from the create-run request. It is the inline-policy-aware replacement for the
// bare resolvePolicy call: it owns the XOR check, the inline validation, and the
// stored/default fallback. It writes its own HTTP error and returns ok=false
// when it has already responded; callers must stop on ok=false.
//
// Resolution (fail closed, admin-gated surface):
//   - inline_policy AND policy_id both set  => 400 (mutually exclusive).
//   - inline_policy set                     => validate via validatePolicySpec
//     (so runner.ValidateMount gates any inline mount) AND validateInlineSecretRefs
//     (so any inline api_key grant references a real, non-reserved secret); on
//     success the inline spec attaches with a NIL policy id (it is not a stored
//     row) and a policy.inline audit event is emitted.
//   - else (policy_id set, or neither)      => the existing resolvePolicy path
//     (stored row, else the configured default), THEN validateInlineSecretRefs
//     against the resolved spec — same secret-existence check as the inline
//     branch, including a 422 when the spec's grants exist but no secret store
//     is configured. This is a deliberate behavior change (see CHANGELOG): a
//     stored or default policy naming a missing/reserved secret now 422s at
//     create instead of only failing later at first proxy injection or clone.
func (s *Server) resolveRunPolicy(ctx context.Context, w http.ResponseWriter, r *http.Request, req *createRunRequest) (types.RunPolicySpec, *uuid.UUID, bool) {
	// XOR: a run picks EITHER a stored policy_id OR an inline policy, never both.
	if req.InlinePolicy != nil && req.PolicyID != nil {
		writeError(w, http.StatusBadRequest, "specify either policy_id or inline_policy, not both")
		return types.RunPolicySpec{}, nil, false
	}

	// Inline path: validate structurally (same validator as a stored policy) then
	// validate any inline secret references. On success attach with a nil id.
	if req.InlinePolicy != nil {
		spec := *req.InlinePolicy
		if err := validatePolicySpec(spec); err != nil {
			writeError(w, http.StatusBadRequest, "invalid inline_policy: "+err.Error())
			return types.RunPolicySpec{}, nil, false
		}
		if code, err := s.validateInlineSecretRefs(ctx, spec); err != nil {
			writeError(w, code, "invalid inline_policy: "+err.Error())
			return types.RunPolicySpec{}, nil, false
		}
		// Audit the use of an inline (non-stored) policy. The run id is not yet
		// minted at this point, so this event carries a nil run id (like the
		// secret.* admin events); the subsequent run.create event records
		// inline_policy=true bound to the run id for correlation.
		s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
			"policy.inline", "", "success", mustJSON(map[string]any{
				"min_confinement_class": spec.MinConfinementClass,
				"workspace_mounts":      len(spec.WorkspaceMounts),
				"eligible_grants":       len(spec.EligibleGrants),
			})))
		return spec, nil, true
	}

	// Stored/default path: resolve, then validate secret references the SAME way
	// the inline branch does (ponytail: one call, no duplicated logic — H1).
	spec, policyID, err := s.resolvePolicy(ctx, req.PolicyID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusBadRequest, "policy_id not found")
			return types.RunPolicySpec{}, nil, false
		}
		writeError(w, http.StatusInternalServerError, "resolve policy: "+err.Error())
		return types.RunPolicySpec{}, nil, false
	}
	if code, err := s.validateInlineSecretRefs(ctx, spec); err != nil {
		writeError(w, code, "invalid policy: "+err.Error())
		return types.RunPolicySpec{}, nil, false
	}
	return spec, policyID, true
}

// validateInlineSecretRefs fails a policy spec closed when any of its api_key
// OR git_pat eligible grants names a secret that does not actually exist, is a
// reserved platform-internal key, or when no secret store is configured at all.
// Despite the name (kept for the inline call site it was written for — H1 now
// also calls it from the stored/default branch, same check either way) it takes
// a plain types.RunPolicySpec, not anything inline-specific.
// It NEVER reads a secret VALUE — it consults Secrets.List (names only). The
// returned status code is 422 (Unprocessable Entity) for every failure so the
// create call fails closed: an inline policy whose grant references a
// missing/forbidden secret would otherwise brick the run (a proxy-injection
// api_key at startup, or a git_pat clone at task time).
//
// Other grant kinds are skipped here — the structural validity of a grant scope
// is the job of validatePolicySpec / the broker; this check is solely about
// secret existence.
func (s *Server) validateInlineSecretRefs(ctx context.Context, spec types.RunPolicySpec) (int, error) {
	// Collect the secret names referenced by api_key AND git_pat grants (both
	// resolve a stored secret by name — api_key proxy-side, git_pat via the git
	// helper). If there are none, there is nothing to check and no secret store
	// is required.
	var needed []string
	for _, g := range spec.EligibleGrants {
		switch g.Kind {
		case types.GrantAPIKey:
			rule, derr := injectionRuleFromScope(g.Scope)
			if derr != nil {
				// An undecodable api_key scope cannot reference a resolvable secret;
				// fail closed rather than silently skipping it.
				return http.StatusUnprocessableEntity, fmt.Errorf("api_key grant scope invalid: %w", derr)
			}
			if reservedSecretNames[rule.SecretName] {
				return http.StatusUnprocessableEntity, fmt.Errorf(
					"api_key grant references reserved secret name %q", rule.SecretName)
			}
			// The subscription OAuth sentinel is NOT a stored secret — it resolves
			// live from the operator's ~/.claude at inject time. Don't require it in
			// the secret store (that's the "references unknown secret" bug for a
			// subscription-recorded profile); just require the provider to be wired.
			if rule.SecretName == subscriptionOAuthSecret {
				if s.cfg.SubscriptionToken == nil {
					return http.StatusUnprocessableEntity, errors.New(
						"policy uses subscription LLM auth, but no subscription token provider is configured")
				}
				continue
			}
			needed = append(needed, rule.SecretName)
		case types.GrantGitPAT:
			_, secretName, _, derr := gitPATScopeFields(g.Scope)
			if derr != nil {
				return http.StatusUnprocessableEntity, fmt.Errorf("git_pat grant scope invalid: %w", derr)
			}
			if reservedSecretNames[secretName] {
				return http.StatusUnprocessableEntity, fmt.Errorf(
					"git_pat grant references reserved secret name %q", secretName)
			}
			needed = append(needed, secretName)
		case types.GrantSSHKey:
			_, keyRef, _, khRef, derr := sshKeyScopeFields(g.Scope)
			if derr != nil {
				return http.StatusUnprocessableEntity, fmt.Errorf("ssh_key grant scope invalid: %w", derr)
			}
			if reservedSecretNames[keyRef] || reservedSecretNames[khRef] {
				return http.StatusUnprocessableEntity, errors.New(
					"ssh_key grant references a reserved secret name")
			}
			needed = append(needed, keyRef)
			if khRef != "" {
				needed = append(needed, khRef)
			}
		default:
			continue
		}
	}
	if len(needed) == 0 {
		return 0, nil
	}

	// At least one api_key grant needs a secret: a secret store MUST be wired or
	// the injection can never resolve (fail closed).
	if s.cfg.Secrets == nil {
		return http.StatusUnprocessableEntity, errors.New(
			"inline_policy api_key grant requires a secret store, but none is configured")
	}

	// Names only — never Get a value here.
	have, err := s.cfg.Secrets.List(ctx)
	if err != nil {
		return http.StatusUnprocessableEntity, fmt.Errorf("list secrets: %w", err)
	}
	known := make(map[string]bool, len(have))
	for _, n := range have {
		known[n] = true
	}
	for _, n := range needed {
		if !known[n] {
			return http.StatusUnprocessableEntity, fmt.Errorf(
				"api_key grant references unknown secret %q (set it first via the secrets API)", n)
		}
	}
	return 0, nil
}
