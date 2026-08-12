// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// resolveRunPolicy resolves the RunPolicySpec + policy id to attach to a run,
// from the create-run request. It is the inline-policy-aware replacement for the
// bare resolvePolicy call: it owns the XOR check, the inline validation, and the
// stored/default fallback. It writes its own HTTP error and returns ok=false
// when it has already responded; callers must stop on ok=false.
//
// Resolution (fail closed; shared by both handleCreateRun and the
// handlePreflightRun dry-run, so a member's preview can never disagree with
// what launch actually does):
//   - inline_policy AND policy_id both set  => 400 (mutually exclusive).
//   - inline_policy set                     => for a MEMBER caller (item 5),
//     clamp to composer.Clamp(spec, DefaultPolicy) FIRST — an admin-authored
//     ceiling a member's own inline_policy can never exceed. An admin is
//     UNCLAMPED (they ARE the ceiling-setting authority). Only THEN validate
//     via validatePolicySpec (so runner.ValidateMount gates any inline mount)
//     AND validateInlineSecretRefs (so any inline api_key grant references a
//     real, non-reserved secret); on success the (possibly clamped) inline
//     spec attaches with a NIL policy id (it is not a stored row) and a
//     policy.inline audit event is emitted — which therefore already
//     reflects the clamped spec, not the raw member-submitted one.
//   - else (policy_id set, or neither)      => the existing resolvePolicy path
//     (stored row, else the configured default; NOT re-clamped — a stored
//     policy or the default IS already admin-authored/the ceiling itself),
//     THEN validateInlineSecretRefs against the resolved spec — same
//     secret-existence check as the inline branch, including a 422 when the
//     spec's grants exist but no secret store is configured. This is a
//     deliberate behavior change (see CHANGELOG): a stored or default policy
//     naming a missing/reserved secret now 422s at create instead of only
//     failing later at first proxy injection or clone.
//
// dryRun suppresses the policy.inline audit write: a preflight preview is not an
// inline-policy USE, and the audit feed is the system of record — orphan
// policy.inline rows with no following run.create would be indistinguishable
// from real authorizations.
//
// The 4th return is L6's clamp-warning list (composer.Clamp's own "what did I
// change" notes) — non-nil only on the member inline-policy branch, since
// that is the ONLY resolution path that ever clamps. handleCreateRun (launch)
// discards it: a launch may stay silent about a clamp exactly as it always
// has (the resolved/attached spec is already the clamped one regardless — the
// clamp itself is never skipped). handlePreflightRun surfaces it in Review so
// a member sees WHY their inline_policy differs from what they typed, before
// they launch.
func (s *Server) resolveRunPolicy(ctx context.Context, w http.ResponseWriter, r *http.Request, req *createRunRequest, dryRun bool) (types.RunPolicySpec, *uuid.UUID, []string, bool) {
	// XOR: a run picks EITHER a stored policy_id OR an inline policy, never both.
	if req.InlinePolicy != nil && req.PolicyID != nil {
		writeError(w, http.StatusBadRequest, "specify either policy_id or inline_policy, not both")
		return types.RunPolicySpec{}, nil, nil, false
	}

	// Inline path: validate structurally (same validator as a stored policy) then
	// validate any inline secret references. On success attach with a nil id.
	if req.InlinePolicy != nil {
		// A member MAY author an inline_policy (item 5) — it is not refused,
		// it is CLAMPED below to the operator's own DefaultPolicy ceiling, so
		// a member can never smuggle wider egress/grants/confinement than the
		// operator already allows. An admin is the ceiling-setting authority
		// and is left unclamped. (This supersedes the earlier operator-only
		// SECMODEL-1 gate: a clamp bounds a member without blocking them.)
		spec := *req.InlinePolicy
		var clampWarnings []string
		if !s.isOperator(r.Context()) {
			// Item 5: a member's inline_policy can never smuggle wider grants/
			// egress/confinement than the operator's own DefaultPolicy allows.
			// Clamped BEFORE validation/resolution — never the fully-resolved
			// spec, which would strip the LATER admin-authored additions
			// (workspace integration binding, ensureLLMGrant,
			// applyRequiredSecretGrant, applyIntegrationRequirement) folded in
			// by runs.go/preflight.go AFTER this function returns.
			spec, clampWarnings = composer.Clamp(spec, s.cfg.DefaultPolicy)
			// composer.Clamp bounds egress/confinement/TTL and drops grant KINDS
			// outside the ceiling, but NOT a stored-secret grant's SCOPE (which
			// operator secret is paired with which host) — that stays member-
			// authored and would be a secret-exfil primitive. Drop a member's
			// api_key/git_pat/ssh_key grant whose pairing the operator did not
			// eligible-list; the run's own model-access grant is re-added at
			// launch by foldRunIntegration (an operator integration) or
			// applyWorkspaceRequirements (a workspace requirement) below. See
			// filterMemberGrants.
			kept, grantWarns, code, gerr := s.filterMemberGrants(spec.EligibleGrants)
			if gerr != nil {
				writeError(w, code, "invalid inline_policy: "+gerr.Error())
				return types.RunPolicySpec{}, nil, nil, false
			}
			spec.EligibleGrants = kept
			clampWarnings = append(clampWarnings, grantWarns...)
		}
		if err := validatePolicySpec(spec); err != nil {
			writeError(w, http.StatusBadRequest, "invalid inline_policy: "+err.Error())
			return types.RunPolicySpec{}, nil, nil, false
		}
		if code, err := s.validateInlineSecretRefs(ctx, spec); err != nil {
			writeError(w, code, "invalid inline_policy: "+err.Error())
			return types.RunPolicySpec{}, nil, nil, false
		}
		// Audit the use of an inline (non-stored) policy. The run id is not yet
		// minted at this point, so this event carries a nil run id (like the
		// secret.* admin events); the subsequent run.create event records
		// inline_policy=true bound to the run id for correlation. Skipped for a
		// preflight dry-run (see the doc comment).
		if !dryRun {
			s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
				"policy.inline", "", "success", mustJSON(map[string]any{
					"min_confinement_class": spec.MinConfinementClass,
					"workspace_mounts":      len(spec.WorkspaceMounts),
					"eligible_grants":       len(spec.EligibleGrants),
				})))
		}
		return spec, nil, clampWarnings, true
	}

	// Stored/default path: resolve, then validate secret references the SAME way
	// the inline branch does (one call, no duplicated logic — H1). Never
	// clamped (a stored policy or the default IS already the ceiling), so no
	// warnings to return here either.
	spec, policyID, err := s.resolvePolicy(ctx, req.PolicyID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusBadRequest, "policy_id not found")
			return types.RunPolicySpec{}, nil, nil, false
		}
		writeError(w, http.StatusInternalServerError, "resolve policy: "+err.Error())
		return types.RunPolicySpec{}, nil, nil, false
	}
	if code, err := s.validateInlineSecretRefs(ctx, spec); err != nil {
		writeError(w, code, "invalid policy: "+err.Error())
		return types.RunPolicySpec{}, nil, nil, false
	}
	return spec, policyID, nil, true
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
// filterMemberGrants drops a MEMBER's inline stored-secret grant (api_key /
// git_pat / ssh_key) whose {host, secret} pairing the operator did not
// eligible-list. composer.Clamp bounds egress, confinement, TTL and grant
// KINDS, but the SCOPE of a non-github grant — which stored credential is
// injected on which host — is member-authored and left unclamped; without this
// a member could pair ANY non-reserved stored secret with ANY allowlisted host
// (a secret-exfil primitive, amplified by scan-seeded egress). DROPPED, not
// rejected: a run's legitimate model-access grant is re-added admin-side at
// launch (handleCreateRun) AFTER this returns — foldRunIntegration folds an
// operator integration's own key (DefaultFor:agent_runs or a workspace pin),
// applyWorkspaceRequirements folds a workspace requirement's secret — so a
// member's own copy of one is redundant, and dropping an unmatched grant
// removes a genuine exfil pairing while the real grant still arrives. (A member
// run whose model access relies on a raw operator secret with NO integration
// and NO workspace requirement gets no grant re-added — fail-closed, no exfil;
// operators provision member model access via an integration.) Rejecting
// instead would 403 the composer's own model-access grant under a wildcard
// api_key ceiling, and an integration key's name is not the provider convention
// so it cannot be exempt-matched at this layer anyway. Called for MEMBERS only —
// an operator is the ceiling authority and stays unclamped. A sentinel LLM
// api_key grant is kept: it references no operator stored secret and
// validateInlineSecretRefs host-pins it to the provider. An UNDECODABLE scope is
// a malformed request → error (fail closed), never a silent drop.
func (s *Server) filterMemberGrants(grants []types.GrantSpec) (kept []types.GrantSpec, warns []string, code int, err error) {
	ceiling := s.cfg.DefaultPolicy.EligibleGrants
	for _, g := range grants {
		host, secretRef, covered, derr := storedSecretGrantPairing(g)
		if !covered {
			kept = append(kept, g) // github_token (scope-intersected by the clamp), cloud_sts, …
			continue
		}
		if derr != nil {
			return nil, nil, http.StatusUnprocessableEntity, fmt.Errorf("%s grant scope invalid: %w", g.Kind, derr)
		}
		if g.Kind == types.GrantAPIKey {
			if _, _, isSentinel := s.oauthProviderForSentinel(secretRef); isSentinel {
				kept = append(kept, g) // host-pinned to the provider by validateInlineSecretRefs
				continue
			}
		}
		if !storedSecretPairingInCeiling(g.Kind, host, secretRef, ceiling) {
			warns = append(warns, fmt.Sprintf(
				"dropped %s grant pairing secret %q with host %q: not in the operator's eligible grants (the run's own model access is provisioned by the platform)",
				g.Kind, secretRef, host))
			continue
		}
		kept = append(kept, g)
	}
	return kept, warns, 0, nil
}

// storedSecretGrantPairing returns the (host, secretRef) a stored-secret grant
// pairs and whether the kind is one enforceMemberGrantScope covers. An
// undecodable scope returns covered=true WITH the error (fail closed — an
// unreadable stored-secret grant is rejected, never skipped).
func storedSecretGrantPairing(g types.GrantSpec) (host, secretRef string, covered bool, err error) {
	switch g.Kind {
	case types.GrantAPIKey:
		r, e := injectionRuleFromScope(g.Scope)
		return r.Host, r.SecretName, true, e
	case types.GrantGitPAT:
		h, sn, _, e := gitPATScopeFields(g.Scope)
		return h, sn, true, e
	case types.GrantSSHKey:
		h, kr, _, _, e := sshKeyScopeFields(g.Scope)
		return h, kr, true, e
	default:
		return "", "", false, nil
	}
}

// storedSecretPairingInCeiling reports whether some operator eligible-grant of
// the same kind pairs the SAME host with the SAME secret (host case-insensitive)
// — an exact-pairing match, so a member may only reuse a pairing the operator
// explicitly listed, never invent one. An operator ceiling grant with a wildcard
// (empty/undecodable) scope carries no pairing and matches nothing here, so it
// authorizes the kind for the composer's own (sentinel, host-pinned) grants
// without empowering a member to pick the secret and host.
func storedSecretPairingInCeiling(kind types.GrantKind, host, secretRef string, ceiling []types.GrantSpec) bool {
	for _, cg := range ceiling {
		if cg.Kind != kind {
			continue
		}
		ch, cs, covered, err := storedSecretGrantPairing(cg)
		if !covered || err != nil {
			continue
		}
		if cs == secretRef && hostEqual(ch, host) {
			return true
		}
	}
	return false
}

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
			if sinkReservedSecret(rule.SecretName) {
				return http.StatusUnprocessableEntity, fmt.Errorf(
					"api_key grant references reserved secret name %q", rule.SecretName)
			}
			// The subscription/managed OAuth sentinels are NOT stored secrets — they
			// resolve live at inject time (resident ~/.claude, or the Wardyn-managed
			// captured setup-token). Don't require them in the secret store (that's the
			// "references unknown secret" bug for a subscription/managed-recorded
			// profile); just require the matching provider to be wired.
			if provider, source, isSentinel := s.oauthProviderForSentinel(rule.SecretName); isSentinel {
				if provider == nil {
					return http.StatusUnprocessableEntity, fmt.Errorf(
						"policy uses %s LLM auth, but no %s token provider is configured", source, source)
				}
				// Host pin (H2, write-time defense): the sentinel resolves to a LIVE
				// OAuth token and may only ever target Anthropic. Reject an authored
				// grant that points it elsewhere (the inject sink also enforces this,
				// fail-closed).
				if !hostEqual(rule.Host, subscriptionInjectionHost) {
					return http.StatusUnprocessableEntity, fmt.Errorf(
						"%s LLM auth may only target %s, not %q", source, subscriptionInjectionHost, rule.Host)
				}
				continue
			}
			needed = append(needed, rule.SecretName)
		case types.GrantGitPAT:
			_, secretName, _, derr := gitPATScopeFields(g.Scope)
			if derr != nil {
				return http.StatusUnprocessableEntity, fmt.Errorf("git_pat grant scope invalid: %w", derr)
			}
			if sinkReservedSecret(secretName) {
				return http.StatusUnprocessableEntity, fmt.Errorf(
					"git_pat grant references reserved secret name %q", secretName)
			}
			needed = append(needed, secretName)
		case types.GrantSSHKey:
			_, keyRef, _, khRef, derr := sshKeyScopeFields(g.Scope)
			if derr != nil {
				return http.StatusUnprocessableEntity, fmt.Errorf("ssh_key grant scope invalid: %w", derr)
			}
			if sinkReservedSecret(keyRef) || sinkReservedSecret(khRef) {
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
			"an api_key grant requires a secret store, but none is configured")
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
