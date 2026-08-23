// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"

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
// change" notes, plus the capability/grant drops) — non-nil only on the member
// inline-policy branch, since that is the ONLY resolution path that ever
// clamps. BOTH callers surface it: handlePreflightRun in Review, so a member
// sees WHY their inline_policy differs from what they typed before they
// launch, and handleCreateRun on the 201, because the console launches without
// preflighting and a silent narrowing is a run that quietly is not the run the
// member asked for.
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
			// Every one of those drops is now AUDITED as well. It used to be a
			// warning and nothing else, so an operator reading the stream could
			// not tell that a member had tried to pair one of their secrets with
			// a host of the member's own choosing (the gap ROADMAP names).
			drops := make([]capDrop, 0, len(grantWarns))
			for _, gw := range grantWarns {
				drops = append(drops, capDrop{reason: "grant_pairing_not_eligible", detail: gw})
			}
			// 0.6 capabilities: the clamp above bounds the member to the
			// OPERATOR's ceiling; this bounds what survived it to what THIS
			// member personally holds.
			capWarns, capDrops, cerr := s.narrowMemberInlinePolicy(ctx, &spec)
			if cerr != nil {
				writeError(w, http.StatusInternalServerError, "resolve capability: "+cerr.Error())
				return types.RunPolicySpec{}, nil, nil, false
			}
			clampWarnings = append(clampWarnings, capWarns...)
			// Skipped for a dry run for the SAME reason policy.inline is (see
			// the doc comment): Review re-resolves on every edit, and a stream
			// of denials for a policy nobody launched is indistinguishable from
			// denials that actually bounded a run.
			if !dryRun {
				s.auditMemberPolicyDrops(ctx, r, append(drops, capDrops...))
			}
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

// capDrop is one thing a capability took away from a member's inline policy:
// the reason it went (one of the values OPERATIONS lists under authz.denied)
// and the detail that names WHICH thing — a host, a secret name, or the
// pairing warning the member is also shown in Review.
type capDrop struct{ reason, detail string }

// narrowMemberInlinePolicy bounds a MEMBER's own inline policy by the
// capability grants that member holds. It runs after composer.Clamp and
// filterMemberGrants, and the difference between them is the whole doctrine:
// the clamp bounds a member to what the OPERATOR authorized deployment-wide,
// this bounds what survived to what THIS member was granted personally. A
// stored-secret pairing therefore has to clear BOTH — operator-eligible AND
// granted here — because either one alone is a hole.
//
// It touches only the MEMBER-AUTHORED spec. Everything admin-authored — a
// stored policy, the workspace's requirements, the scan's seeded hosts, the
// model provider's own egress, the grant re-added at launch by
// foldRunIntegration/applyWorkspaceRequirements — is folded in by the callers
// AFTER this returns, and is deliberately left alone: narrowing what an admin
// already authorized would brick workspace runs at scale, and a member who
// cannot be trusted with a workspace should not be granted the workspace.
//
// DROPS, never rejects, exactly as filterMemberGrants does — with a warning per
// drop, so preflight/Review names what will not be there before launch, and a
// capDrop so the audit stream records it. A member whose whole allowlist is
// ungranted gets a run with no member-authored egress, not a 403: the run's
// admin-authored egress is still there and is what the task usually needs.
//
// Under an operator ceiling of allow_all_egress the allowlist is not the gate
// at all (composer.Clamp leaves AllowAllEgress set and the proxy allows any
// non-denied public host), so egress_host narrowing does nothing there. That is
// the operator's own posture, named here so nobody reads a green switch as a
// bound that deployment does not have.
func (s *Server) narrowMemberInlinePolicy(ctx context.Context, spec *types.RunPolicySpec) ([]string, []capDrop, error) {
	var warns []string
	var drops []capDrop

	keptDomains := spec.AllowedDomains[:0:0]
	for _, d := range spec.AllowedDomains {
		ok, err := s.capSeamAllowed(ctx, capEgressHost, d)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			warns = append(warns, fmt.Sprintf("dropped egress host %q: not granted to you", d))
			drops = append(drops, capDrop{reason: "capability_" + capEgressHost, detail: d})
			continue
		}
		keptDomains = append(keptDomains, d)
	}
	spec.AllowedDomains = keptDomains

	keptGrants := spec.EligibleGrants[:0:0]
	for _, g := range spec.EligibleGrants {
		// filterMemberGrants 422s an undecodable stored-secret scope — and, since
		// the pairing switch closed, an unknown kind too — before this runs, and
		// it is the only order that exists. The error is still HONORED here
		// rather than discarded: relying on that ordering is what let the old
		// open default arm through TWO gates instead of one, and an unreadable
		// pairing has no secretRef to check, so keeping it would be a free pass.
		_, secretRef, knownHostsRef, covered, derr := storedSecretGrantPairing(g)
		if covered && derr != nil {
			warns = append(warns, fmt.Sprintf("dropped %s grant: %v", g.Kind, derr))
			drops = append(drops, capDrop{reason: "capability_" + capSecret, detail: string(g.Kind)})
			continue
		}
		if !covered {
			keptGrants = append(keptGrants, g) // github_token, cloud_sts name no stored secret
			continue
		}
		if g.Kind == types.GrantAPIKey {
			if _, _, isSentinel := s.oauthProviderForSentinel(secretRef); isSentinel {
				keptGrants = append(keptGrants, g) // a live OAuth token, not a stored secret
				continue
			}
		}
		// BOTH refs, because an ssh_key grant's known_hosts_secret_ref resolves
		// a stored secret whose raw value the broker hands back (see
		// storedSecretPairingInCeiling) — gating only the key would leave the
		// smaller half of the same door open.
		refused := ""
		for _, ref := range []string{secretRef, knownHostsRef} {
			if ref == "" {
				continue
			}
			ok, err := s.capSeamAllowed(ctx, capSecret, ref)
			if err != nil {
				return nil, nil, err
			}
			if !ok {
				refused = ref
				break
			}
		}
		if refused != "" {
			warns = append(warns, fmt.Sprintf("dropped %s grant referencing secret %q: not granted to you", g.Kind, refused))
			drops = append(drops, capDrop{reason: "capability_" + capSecret, detail: refused})
			continue
		}
		keptGrants = append(keptGrants, g)
	}
	spec.EligibleGrants = keptGrants

	// Workspace repos. denyMemberRequest gates the req.workspace_id door, but an
	// inline workspace_repos entry naming an ONBOARDED repo is a second door to
	// the same room: composer.Clamp drops only WorkspaceMounts, referencedWorkspaces
	// matches the entry by URL, and applyWorkspaceRequirements then folds that
	// workspace's admin-authored egress, operator_set secret grants and base image
	// into the member's run. Gated here rather than at either call site because
	// resolveRunPolicy is the chokepoint launch AND the preflight dry-run share, so
	// Review can never preview a workspace launch would refuse.
	//
	// Repos only: a member's WorkspaceMounts are already nil by the time this runs
	// (composer.Clamp drops every proposed mount unconditionally), so a second loop
	// over them would be filtering an empty slice.
	//
	// A repo NO workspace owns is left alone — validateWorkspaceSources 422s it as
	// un-onboarded a few lines later, and dropping it here would turn that clear
	// refusal into a silently smaller run.
	if len(spec.WorkspaceRepos) > 0 && s.cfg.Store != nil {
		all, err := s.cfg.Store.ListWorkspaces(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("api: list workspaces: %w", err)
		}
		idx := indexWorkspacesBySource(all)
		keptRepos := spec.WorkspaceRepos[:0:0]
		for _, wr := range spec.WorkspaceRepos {
			ws, onboarded := idx.repo[wr.Repo]
			if onboarded {
				ok, err := s.capSeamAllowed(ctx, capWorkspace, ws.ID.String())
				if err != nil {
					return nil, nil, err
				}
				if !ok {
					warns = append(warns, fmt.Sprintf("dropped repo %q: workspace %s is not granted to you", wr.Repo, ws.ID))
					drops = append(drops, capDrop{reason: "capability_" + capWorkspace, detail: wr.Repo})
					continue
				}
			}
			keptRepos = append(keptRepos, wr)
		}
		spec.WorkspaceRepos = keptRepos
	}

	return warns, drops, nil
}

// auditMemberPolicyDrops records what a member's inline policy LOST — one event
// per REASON, not one per dropped item. A spec naming twenty ungranted hosts is
// one authorization outcome, not twenty, and a stream that flooded on it is the
// first thing an operator would filter away. Grouping by reason (rather than
// blending every drop into one event) keeps `reason` the single value every
// authz.denied consumer already reads, with the affected values beside it.
func (s *Server) auditMemberPolicyDrops(ctx context.Context, r *http.Request, drops []capDrop) {
	byReason := map[string][]string{}
	for _, d := range drops {
		byReason[d.reason] = append(byReason[d.reason], d.detail)
	}
	for _, reason := range slices.Sorted(maps.Keys(byReason)) { // stable order for the stream
		s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
			"authz.denied", "runs.inline_policy", "denied", mustJSON(map[string]any{
				"reason": reason, "dropped": byReason[reason],
			})))
	}
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
// validateInlineSecretRefs host-pins it to the provider. An UNDECODABLE scope —
// or, since the pairing switch closed, an UNKNOWN KIND — is a malformed request
// → error (fail closed), never a silent drop.
func (s *Server) filterMemberGrants(grants []types.GrantSpec) (kept []types.GrantSpec, warns []string, code int, err error) {
	ceiling := s.cfg.DefaultPolicy.EligibleGrants
	for _, g := range grants {
		host, secretRef, knownHostsRef, covered, derr := storedSecretGrantPairing(g)
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
		if !storedSecretPairingInCeiling(g.Kind, host, secretRef, knownHostsRef, ceiling) {
			warns = append(warns, fmt.Sprintf(
				"dropped %s grant pairing secret %q with host %q: not in the operator's eligible grants (the run's own model access is provisioned by the platform)",
				g.Kind, secretRef, host))
			continue
		}
		kept = append(kept, g)
	}
	return kept, warns, 0, nil
}

// storedSecretGrantPairing returns the (host, secretRef, knownHostsRef) a
// stored-secret grant pairs and whether the kind is one enforceMemberGrantScope
// covers. knownHostsRef is populated only for ssh_key (empty — and compared as
// such — for every other kind; see storedSecretPairingInCeiling). An undecodable
// scope returns covered=true WITH the error (fail closed — an unreadable
// stored-secret grant is rejected, never skipped).
//
// The switch is CLOSED — every types.GrantKind is named, and the default arm
// REFUSES rather than falling through. That arm used to return covered=false,
// which reads as "this kind names no stored secret" and is the answer both
// callers give a free pass: filterMemberGrants `kept = append(kept, g)` and
// narrowMemberInlinePolicy `keptGrants = append(keptGrants, g)`. So a grant kind
// added to types.GrantKind and wired to a stored secret was member-authorable,
// unclamped by the operator's eligible-grant
// pairing and unchecked against capSecret, until somebody remembered to come
// back here. The kind set is small and closed; a compiler-visible list plus a
// refusing default makes forgetting fail shut instead of open.
// (TestStoredSecretGrantPairing_UnknownKindIsRefused is the regression.)
func storedSecretGrantPairing(g types.GrantSpec) (host, secretRef, knownHostsRef string, covered bool, err error) {
	switch g.Kind {
	case types.GrantAPIKey:
		r, e := injectionRuleFromScope(g.Scope)
		return r.Host, r.SecretName, "", true, e
	case types.GrantGitPAT:
		h, sn, _, e := gitPATScopeFields(g.Scope)
		return h, sn, "", true, e
	case types.GrantSSHKey:
		h, kr, _, khr, e := sshKeyScopeFields(g.Scope)
		return h, kr, khr, true, e
	case types.GrantGitHubToken, types.GrantCloudSTS:
		// Genuinely name no stored secret: github_token mints an App
		// installation token (scope-intersected by composer.Clamp), cloud_sts is
		// hard-refused by the embedded IdP. Not covered, and safe to keep.
		return "", "", "", false, nil
	default:
		return "", "", "", true, fmt.Errorf("unknown grant kind %q", g.Kind)
	}
}

// storedSecretPairingInCeiling reports whether some operator eligible-grant of
// the same kind pairs the SAME host with the SAME secret (host case-insensitive)
// AND, for ssh_key, the SAME known_hosts_secret_ref (empty included) — an
// exact-pairing match, so a member may only reuse a pairing the operator
// explicitly listed, never invent one.
//
// known_hosts_secret_ref is part of this match (W12-B-2): before this it was
// left out of the comparison entirely, so a member could reuse an
// operator-approved (host, key_secret_ref) pairing while attaching ANY
// known_hosts_secret_ref of their own choosing — including one naming a stored
// secret with no relation to SSH host keys — and mintSSHKey (broker.go) would
// return that secret's raw value as Minted.KnownHosts, an rbac-bypass escaping
// this gate entirely. Requiring an exact match (including the common
// empty==empty case, where the operator named no known_hosts_secret_ref at all)
// closes that: a member can only obtain known_hosts material the operator's OWN
// ceiling grant already named for that exact pairing.
//
// An operator ceiling grant with a wildcard (empty/undecodable) scope carries no
// pairing and matches nothing here, so it authorizes the kind for the
// composer's own (sentinel, host-pinned) grants without empowering a member to
// pick the secret and host.
func storedSecretPairingInCeiling(kind types.GrantKind, host, secretRef, knownHostsRef string, ceiling []types.GrantSpec) bool {
	for _, cg := range ceiling {
		if cg.Kind != kind {
			continue
		}
		ch, cs, ckhr, covered, err := storedSecretGrantPairing(cg)
		if !covered || err != nil {
			continue
		}
		if cs == secretRef && hostEqual(ch, host) && ckhr == knownHostsRef {
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
