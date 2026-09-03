// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// boundMemberSpec is THE member bounding pipeline — the three stages that turn
// a spec a member chose into one an admin authorized, in the one order that is
// correct:
//
//  1. composer.Clamp against the member's ceiling. It bounds egress,
//     confinement and TTL and drops grant KINDS outside the ceiling, but NOT a
//     stored-secret grant's SCOPE (which operator secret is paired with which
//     host) — that stays member-authored, and on its own would be a
//     secret-exfil primitive. Hence stage 2.
//  2. filterMemberGrants drops any api_key/git_pat/ssh_key grant whose pairing
//     the operator did not eligible-list. The run's own model-access grant is
//     re-added at launch by foldRunIntegration (an operator integration) or
//     applyWorkspaceRequirements (a workspace requirement).
//  3. narrowMemberInlinePolicy bounds what survived to what THIS member
//     personally holds: stage 1 is the OPERATOR's deployment-wide ceiling,
//     stage 3 is this member's own capability grants. A pairing clears BOTH.
//
// Every drop is AUDITED, not merely warned: a warning alone left an operator
// unable to tell that a member had tried to pair one of their secrets with a
// host of the member's own choosing (the gap ROADMAP names). dryRun suppresses
// that write for the same reason it suppresses policy.inline (see below).
//
// It is ONE function because the inline branch and the stored branch must bound
// identically: "member-selected content is bounded by the member's ceiling
// whether it arrived as a body or as a row id" (PF-1) is only true while both
// run THIS, and two hand-copied pipelines could drift into a member smuggling
// through one what the other refuses.
//
// errPrefix is the ONLY thing the two callers differ on, and it stays theirs —
// naming the wrong input back at the caller is a worse error message, not a
// smaller one. Returns ok==false when the response is already written.
func (s *Server) boundMemberSpec(ctx context.Context, w http.ResponseWriter, r *http.Request, spec, ceilingSpec types.RunPolicySpec, errPrefix string, dryRun bool) (types.RunPolicySpec, []string, bool) {
	spec, warns := composer.Clamp(spec, ceilingSpec)
	kept, grantWarns, code, gerr := s.filterMemberGrants(ctx, s.secretOwnerFromRequest(r), spec.AllowedDomains, spec.EligibleGrants)
	if gerr != nil {
		writeError(w, code, errPrefix+gerr.Error())
		return types.RunPolicySpec{}, nil, false
	}
	spec.EligibleGrants = kept
	warns = append(warns, grantWarns...)
	drops := make([]capDrop, 0, len(grantWarns))
	for _, gw := range grantWarns {
		drops = append(drops, capDrop{reason: "grant_pairing_not_eligible", detail: gw})
	}
	capWarns, capDrops, cerr := s.narrowMemberInlinePolicy(ctx, s.secretOwnerFromRequest(r), &spec)
	if cerr != nil {
		writeError(w, http.StatusInternalServerError, "resolve capability: "+cerr.Error())
		return types.RunPolicySpec{}, nil, false
	}
	warns = append(warns, capWarns...)
	if !dryRun {
		s.auditMemberPolicyDrops(ctx, r, append(drops, capDrops...))
	}
	return spec, warns, true
}

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
//     clamp to composer.Clamp(spec, THEIR CEILING) FIRST — an admin-authored
//     ceiling a member's own inline_policy can never exceed. An admin is
//     UNCLAMPED (they ARE the ceiling-setting authority). Only THEN validate
//     via validatePolicySpec (so runner.ValidateMount gates any inline mount)
//     AND validateInlineSecretRefs (so any inline api_key grant references a
//     real, non-reserved secret); on success the (possibly clamped) inline
//     spec attaches with a NIL policy id (it is not a stored row) and a
//     policy.inline audit event is emitted — which therefore already
//     reflects the clamped spec, not the raw member-submitted one.
//   - else (policy_id set, or neither)      => the existing resolvePolicy path
//     (stored row, else the caller's own CEILING),
//     THEN validateInlineSecretRefs against the resolved spec — same
//     secret-existence check as the inline branch, including a 422 when the
//     spec's grants exist but no secret store is configured. This is a
//     deliberate behavior change (see CHANGELOG): a stored or default policy
//     naming a missing/reserved secret now 422s at create instead of only
//     failing later at first proxy injection or clone. A member who SELECTED a
//     stored row runs the same three-stage member pipeline the inline branch
//     does, when and only when a governance profile applies to them — see that
//     branch for the scoping and for why a bare Clamp is not enough.
//
// THE CEILING IS RESOLVED ONCE, HERE, and is this PRINCIPAL's rather than the
// deployment's (effectiveCeiling). For a member with no governance assignment
// it IS Config.DefaultPolicy, so every path below is byte-for-byte today for
// them; for an assigned member it is the profile an admin bound to them.
//
// dryRun suppresses the policy.inline audit write: a preflight preview is not an
// inline-policy USE, and the audit feed is the system of record — orphan
// policy.inline rows with no following run.create would be indistinguishable
// from real authorizations.
//
// The 4th return is L6's clamp-warning list (composer.Clamp's own "what did I
// change" notes, plus the capability/grant drops, plus any grant a governance
// profile can no longer serve after a DefaultPolicy redeploy) — non-nil on the
// member branches, which are the only resolution paths that ever clamp. BOTH
// callers surface it: handlePreflightRun in Review, so a member
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

	// This principal's ceiling — resolved BEFORE either branch, because both
	// need it and a create must never resolve two different ceilings for one
	// request. A resolver failure is never fail-open: writeCeilingError 500s a
	// store error and 403s an unanswerable group snapshot (see effectiveCeiling).
	ceiling, ceilErr := s.effectiveCeiling(ctx)
	if ceilErr != nil {
		writeCeilingError(w, ceilErr)
		return types.RunPolicySpec{}, nil, nil, false
	}

	// Inline path: validate structurally (same validator as a stored policy) then
	// validate any inline secret references. On success attach with a nil id.
	if req.InlinePolicy != nil {
		// A member MAY author an inline_policy (item 5) — it is not refused,
		// it is CLAMPED below to the ceiling an admin set FOR THEM, so
		// a member can never smuggle wider egress/grants/confinement than the
		// operator already allows. An admin is the ceiling-setting authority
		// and is left unclamped. (This supersedes the earlier operator-only
		// SECMODEL-1 gate: a clamp bounds a member without blocking them.)
		spec := *req.InlinePolicy
		clampWarnings := append([]string(nil), ceiling.Warnings...)
		// env_secret's admin-only posture, applied FIRST and unconditionally for
		// a non-operator — it is a role check, not a ceiling check, so it must
		// not sit behind the ceiling-scoped gate below (see
		// memberEnvSecretIsAdminOnly).
		spec, envWarns := s.boundEnvSecretPosture(ctx, r, spec, dryRun)
		clampWarnings = append(clampWarnings, envWarns...)
		// DELIBERATELY isOperator (three-tier doctrine, internal/auth/oidc's
		// RoleSecurityAdmin), in lockstep with denyMemberRequest: a security
		// admin's OWN run is clamped like anyone else's. They author the
		// ceiling; they do not stand outside it.
		if !s.isOperator(r.Context()) {
			// Item 5: a member's inline_policy can never smuggle wider grants/
			// egress/confinement than THEIR ceiling allows — the governance
			// profile an admin assigned them, or Config.DefaultPolicy when
			// nobody assigned one.
			// Bounded BEFORE validation/resolution — never the fully-resolved
			// spec, which would strip the LATER admin-authored additions
			// (workspace integration binding, ensureLLMGrant,
			// applyRequiredSecretGrant, applyIntegrationRequirement) folded in
			// by runs.go/preflight.go AFTER this function returns.
			var warns []string
			var bounded bool
			spec, warns, bounded = s.boundMemberSpec(ctx, w, r, spec, ceiling.Spec, "invalid inline_policy: ", dryRun)
			if !bounded {
				return types.RunPolicySpec{}, nil, nil, false
			}
			clampWarnings = append(clampWarnings, warns...)
		}
		if err := validatePolicySpec(spec); err != nil {
			writeError(w, http.StatusBadRequest, "invalid inline_policy: "+err.Error())
			return types.RunPolicySpec{}, nil, nil, false
		}
		if code, err := s.validateInlineSecretRefs(ctx, s.secretOwnerFromRequest(r), spec); err != nil {
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
	// the inline branch does (one call, no duplicated logic — H1). The no-policy
	// default is now the CALLER's ceiling rather than the deployment's
	// (resolvePolicy), and a member-SELECTED stored row is bounded below.
	spec, policyID, err := s.resolvePolicy(ctx, req.PolicyID, ceiling)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusBadRequest, "policy_id not found")
			return types.RunPolicySpec{}, nil, nil, false
		}
		writeError(w, http.StatusInternalServerError, "resolve policy: "+err.Error())
		return types.RunPolicySpec{}, nil, nil, false
	}
	storedWarns := append([]string(nil), ceiling.Warnings...)
	// Same unconditional env_secret posture the inline branch applies, in the
	// same position, and it is the half PF-1's scoping below CANNOT carry: the
	// clamp fires only for an ASSIGNED member selecting a row, while this rule
	// binds every non-operator on every stored AND default resolution. Without
	// it an unassigned member — the default posture — selected a stored row (or
	// took the deployment default) carrying an env_secret grant and the
	// operator's raw secret value landed in their sandbox env at
	// resolveEnvSecretGrants, contradicting three docs that state the control
	// without qualification.
	spec, envWarns := s.boundEnvSecretPosture(ctx, r, spec, dryRun)
	storedWarns = append(storedWarns, envWarns...)
	// PF-1, the central escape: a stored policy row is admin-authored CONTENT,
	// but ANY signed-in caller may put one on their own run (policy_id is
	// ungated, and it has to stay that way — gating it removes a legitimate
	// feature and pushes members onto hand-authored inline specs). So a member
	// selecting a wide row got a wide run, entirely past the clamp their own
	// inline_policy would have hit. One rule falls out: member-selected content
	// is bounded by the member's ceiling whether it arrived as a body or as a
	// row id.
	//
	// SCOPED to policyID != nil && ceiling.Profile != nil, and both halves are
	// load-bearing:
	//
	//   - policyID != nil: the no-policy default is already the ceiling itself
	//     (resolvePolicy above), and clamping a spec against itself is at best a
	//     no-op and at worst order-dependent — composer.Clamp is not a lattice
	//     meet (PF-3).
	//   - Profile != nil: the UNCONDITIONAL variant is deliberately not chosen.
	//     Stored policies are routinely wider than a minimal DefaultPolicy, so
	//     clamping every member's selection to it would shred deployments that
	//     have never heard of this feature. The clamp fires exactly when an
	//     admin has DECLARED this principal's ceiling. The residual is named and
	//     accepted: an unassigned member still selects stored rows unclamped,
	//     which is today's behaviour, and the deployment-wide opt-in is an
	//     `all`-subject assignment.
	//
	// The FULL member pipeline, never Clamp alone: clampGrants passes same-kind
	// grant pairings through VERBATIM, so a Clamp-only stored branch would hand
	// a member every operator-secret pairing the row happened to carry — the
	// "one rule whether body or row id" claim would be false at exactly the
	// grant-bearing rows, which are the ones that matter.
	if policyID != nil && ceiling.Profile != nil && !s.isOperator(r.Context()) {
		var warns []string
		var bounded bool
		spec, warns, bounded = s.boundMemberSpec(ctx, w, r, spec, ceiling.Spec, "invalid policy: ", dryRun)
		if !bounded {
			return types.RunPolicySpec{}, nil, nil, false
		}
		storedWarns = append(storedWarns, warns...)
	}
	if code, err := s.validateInlineSecretRefs(ctx, s.secretOwnerFromRequest(r), spec); err != nil {
		writeError(w, code, "invalid policy: "+err.Error())
		return types.RunPolicySpec{}, nil, nil, false
	}
	return spec, policyID, storedWarns, true
}

// memberEnvSecretIsAdminOnly is THE env_secret posture rule, in one place: a
// NON-OPERATOR does not hold an env_secret grant unless the deployment opened
// envAllowMemberEnvSecret.
//
// It takes no ceiling and no principal's assignment, because the rule needs
// neither — it is a role check plus an env switch. That is exactly what made
// the original placement wrong: the drop lived only inside filterMemberGrants,
// which is reached only from boundMemberSpec, whose stored/default invocation is
// scoped to `ceiling.Profile != nil`. A member with NO governance assignment —
// the default posture, and every pre-0.7 deployment upgrading into 0.7 — ran no
// member pipeline at all, so the control the docs state UNCONDITIONALLY
// (threatmodel/THREAT-MODEL.md §5.1a, docs/ENV.md's WARDYN_ALLOW_MEMBER_ENV_SECRET
// row, docs/POLICIES.md's env_secret row) simply did not fire for them and the
// operator's raw secret value reached their sandbox env at
// resolveEnvSecretGrants. A ceiling-scoped gate must never carry a rule that is
// not about the ceiling.
func memberEnvSecretIsAdminOnly() bool { return !envEnabled(os.Getenv(envAllowMemberEnvSecret)) }

// envSecretAdminOnlyWarning is the one message both drop sites use, so the
// member sees the same sentence in Review whichever path bounded their run.
func envSecretAdminOnlyWarning(secretRef string) string {
	return fmt.Sprintf("dropped env_secret grant for %q: env_secret is admin-only (an operator can open it with %s)",
		secretRef, envAllowMemberEnvSecret)
}

// dropAdminOnlyEnvSecretGrants applies memberEnvSecretIsAdminOnly to a spec a
// non-operator is putting on their own run, and returns the warnings + audit
// drops the removal owes.
//
// UNCONDITIONAL on every member path, which is the whole fix: the other kinds a
// member may reuse are bounded after delivery (an api_key value never leaves the
// broker, a git_pat reaches git through the helper, an ssh_key is wiped after
// the clone), while an env_secret is a raw value in the process environment for
// the run's whole life, with no mint, no TTL and nothing to revoke (see
// GrantEnvSecret). "The operator listed this pairing" is a weaker statement here
// than for every other kind, so it is not the statement this rule rests on.
//
// The drop is AUDITED, not merely warned, under the SAME authz.denied reason
// filterMemberGrants' drops already carry (`grant_pairing_not_eligible`, a
// closed vocabulary docs/OPERATIONS.md is the source of record for): an operator
// reading the stream must be able to see that a member's grant went, and the
// inline path already recorded it that way — adding a second reason value for
// the same event would break the enum's documented stability for no new
// information.
//
// An operator is never called with (the caller checks isOperator): the ceiling
// authority is not clamped by its own ceiling. A SECURITY admin is, deliberately
// — same three-tier doctrine as resolveRunPolicy's inline clamp.
func dropAdminOnlyEnvSecretGrants(grants []types.GrantSpec) ([]types.GrantSpec, []string, []capDrop) {
	if !memberEnvSecretIsAdminOnly() || !slices.ContainsFunc(grants,
		func(g types.GrantSpec) bool { return g.Kind == types.GrantEnvSecret }) {
		return grants, nil, nil
	}
	kept := make([]types.GrantSpec, 0, len(grants))
	var warns []string
	var drops []capDrop
	for _, g := range grants {
		if g.Kind != types.GrantEnvSecret {
			kept = append(kept, g)
			continue
		}
		// The env var NAME sits in the host slot for this kind
		// (storedSecretGrantPairing); the SECRET name is what the warning names,
		// matching filterMemberGrants' wording. An undecodable scope is NOT an
		// error here — it is dropped like any other env_secret, and the caller's
		// later validatePolicySpec/validateInlineSecretRefs still see a spec
		// with nothing left to be malformed about.
		_, secretRef, _, _, _ := storedSecretGrantPairing(g)
		w := envSecretAdminOnlyWarning(secretRef)
		warns = append(warns, w)
		drops = append(drops, capDrop{reason: "grant_pairing_not_eligible", detail: w})
	}
	return kept, warns, drops
}

// boundEnvSecretPosture is resolveRunPolicy's per-branch application of
// dropAdminOnlyEnvSecretGrants: it runs for every non-operator caller on BOTH
// branches, BEFORE (and independently of) the ceiling-scoped member pipeline, so
// no assignment state can decide whether the rule fires. dryRun suppresses the
// audit write for the same reason the rest of resolveRunPolicy does — a
// preflight preview is not a policy USE.
func (s *Server) boundEnvSecretPosture(ctx context.Context, r *http.Request, spec types.RunPolicySpec, dryRun bool) (types.RunPolicySpec, []string) {
	if s.isOperator(r.Context()) {
		return spec, nil
	}
	kept, warns, drops := dropAdminOnlyEnvSecretGrants(spec.EligibleGrants)
	if len(drops) == 0 {
		return spec, nil
	}
	spec.EligibleGrants = kept
	if !dryRun {
		s.auditMemberPolicyDrops(ctx, r, drops)
	}
	return spec, warns
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
func (s *Server) narrowMemberInlinePolicy(ctx context.Context, owner string, spec *types.RunPolicySpec) ([]string, []capDrop, error) {
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
			// A name the member OWNS is exempt from capSecret: that capability
			// bounds access to OPERATOR material, and a member's own row widens
			// nothing (6c).
			if s.ownsSecret(ctx, owner, ref) {
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
// → error (fail closed), never a silent drop. env_secret is admin-only and is
// dropped for a member regardless of the ceiling; see the block below.
//
// The eligible-grant list it compares against is the CALLER's ceiling
// (effectiveCeiling), not Config.DefaultPolicy: a governance profile narrows
// which credential pairings its members may reuse, and reading the deployment
// list here would have left that narrowing unenforced at the one seam where a
// pairing actually becomes an injected credential. Resolved INSIDE rather than
// threaded in from resolveRunPolicy — ponytail: that costs one extra indexed
// read on the member create path (PF-13's accepted double resolution) and buys
// a signature no caller, present or future, can pass the wrong ceiling to;
// thread it through if a profile-load benchmark ever says to.
func (s *Server) filterMemberGrants(ctx context.Context, owner string, allowedDomains []string, grants []types.GrantSpec) (kept []types.GrantSpec, warns []string, code int, err error) {
	resolved, cerr := s.effectiveCeiling(ctx)
	if cerr != nil {
		// Fail CLOSED, and never by silently substituting the deployment list:
		// an unknown ceiling cannot decide whether a pairing is eligible, and
		// guessing here is the one direction that leaks a credential.
		return nil, nil, ceilingErrorStatus(cerr), cerr
	}
	ceiling := resolved.Spec.EligibleGrants
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
			// 6c own-key arm: a member's OWN api_key secret, paired with a
			// model-provider host (isModelProviderHost — the gateway counts
			// too) that the run's own already-clamped egress allows, is
			// admitted with NO operator eligible-grant pairing at all — this
			// is what lets a member's own model key be used with no hand-
			// authored inline grant naming an operator secret. host must be
			// an EXACT allowedDomains entry (the load-bearing half — Clamp
			// has already intersected egress to the ceiling; the suffix
			// match alone would admit evilanthropic.com), and ownership is
			// proved by a names-only For(owner).List, never a value read.
			if s.isModelProviderHost(host) && domainAllowedExact(allowedDomains, host) && s.ownsSecret(ctx, owner, secretRef) {
				kept = append(kept, g)
				continue
			}
		}
		// env_secret is ADMIN-ONLY by default, ahead of the pairing check — a
		// member does not get one even for a pairing the operator DID list.
		// Defence in depth only: resolveRunPolicy already ran
		// dropAdminOnlyEnvSecretGrants over the same spec on EVERY member path,
		// so by the time a grant reaches here there is nothing left for this arm
		// to drop. It stays because filterMemberGrants is the grant gate and a
		// gate that trusts its caller to have already applied half its rule is
		// one refactor away from applying none of it.
		if g.Kind == types.GrantEnvSecret && memberEnvSecretIsAdminOnly() {
			warns = append(warns, envSecretAdminOnlyWarning(secretRef))
			continue
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
// added to types.GrantKind and wired to a stored secret — env_secret is exactly
// that — was member-authorable, unclamped by the operator's eligible-grant
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
	case types.GrantEnvSecret:
		// An env_secret has no host — it is delivered TO the sandbox, not to a
		// destination — so the env var NAME takes the host slot. That makes the
		// ceiling comparison an exact (name, secret) pairing, the same shape
		// git_pat gets, rather than letting a member reuse an operator-blessed
		// secret under a variable name the operator never wrote. hostEqual's
		// lowercasing is harmless here: envSecretScopeFields admits upper case
		// only, so two names that compare equal ARE equal.
		n, sn, e := envSecretScopeFields(g.Scope)
		return n, sn, "", true, e
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

func (s *Server) validateInlineSecretRefs(ctx context.Context, owner string, spec types.RunPolicySpec) (int, error) {
	// Collect the secret names referenced by api_key, git_pat AND ssh_key
	// grants (all three resolve a stored secret by name — api_key proxy-side,
	// git_pat via the git helper, ssh_key as the resident key + optional
	// known_hosts). If there are none, there is nothing to check and no secret
	// store is required.
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
		// A name in owner's OWN namespace (For(owner).List — own rows only) is
		// accepted too — this is what lets a member's inline_policy name their
		// own model key with no operator row of that name at all (6c). Never
		// widens: an operator-only name still 422s below.
		if !known[n] && !s.ownsSecret(ctx, owner, n) {
			return http.StatusUnprocessableEntity, fmt.Errorf(
				"api_key grant references unknown secret %q (set it first via the secrets API)", n)
		}
	}
	return 0, nil
}
