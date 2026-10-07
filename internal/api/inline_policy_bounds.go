// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// userEnvSecretIsAdminOnly is THE env_secret posture rule, in one place: a
// NON-OPERATOR does not hold an env_secret grant unless the deployment opened
// envAllowMemberEnvSecret.
//
// It takes no ceiling and no principal's assignment, because the rule needs
// neither — it is a role check plus an env switch. That is exactly what made
// the original placement wrong: the drop lived only inside filterUserGrants,
// which is reached only from boundUserSpec, whose stored/default invocation is
// scoped to `ceiling.Profile != nil`. A member with NO governance assignment —
// the default posture, and every pre-0.7 deployment upgrading into 0.7 — ran no
// member pipeline at all, so the control the docs state UNCONDITIONALLY
// (threatmodel/THREAT-MODEL.md §5.1a, docs/ENV.md's WARDYN_ALLOW_USER_ENV_SECRET
// row, docs/POLICIES.md's env_secret row) simply did not fire for them and the
// operator's raw secret value reached their sandbox env at
// resolveEnvSecretGrants. A ceiling-scoped gate must never carry a rule that is
// not about the ceiling.
func userEnvSecretIsAdminOnly() bool { return !envEnabled(envAllowMemberEnvSecret) }

// envSecretAdminOnlyWarning is the one message both drop sites use, so the
// member sees the same sentence in Review whichever path bounded their run.
func envSecretAdminOnlyWarning(secretRef string) string {
	return fmt.Sprintf("dropped env_secret grant for %q: env_secret is admin-only (an operator can open it with %s)",
		secretRef, envAllowMemberEnvSecret)
}

// dropAdminOnlyEnvSecretGrants applies userEnvSecretIsAdminOnly to a spec a
// non-operator is putting on their own run, and returns the warnings + audit
// drops the removal owes.
//
// Unconditional on every member path, which is the whole fix: the other kinds a
// member may reuse are bounded after delivery (an api_key value never leaves the
// broker, a git_pat reaches git through the helper, an ssh_key is wiped after
// the clone), while an env_secret is a raw value in the process environment for
// the run's whole life, with no mint, no TTL and nothing to revoke (see
// GrantEnvSecret). "The operator listed this pairing" is a weaker statement here
// than for every other kind, so it is not the statement this rule rests on.
//
// The drop is AUDITED, not merely warned, under the SAME authz.denied reason
// filterUserGrants' drops already carry (`grant_pairing_not_eligible`, a
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
	if !userEnvSecretIsAdminOnly() || !slices.ContainsFunc(grants,
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
		// matching filterUserGrants' wording. An undecodable scope is NOT an
		// error here — it is dropped like any other env_secret, and the caller's
		// later validatePolicySpec/validateInlineSecretRefs still see a spec
		// with nothing left to be malformed about.
		_, secretRef, _, _, _ := storedSecretGrantPairing(g)
		w := envSecretAdminOnlyWarning(secretRef)
		warns = append(warns, w)
		drops = append(drops, capDrop{reason: authz.ReasonGrantPairingNotEligible, detail: w})
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
		s.auditUserPolicyDrops(ctx, r, drops)
	}
	return spec, warns
}

// capDrop is one thing a capability took away from a member's inline policy:
// the reason it went (one of the values OPERATIONS lists under authz.denied)
// and the detail that names WHICH thing — a host, a secret name, or the
// pairing warning the member is also shown in Review.
type capDrop struct {
	reason authz.Reason
	detail string
}

// narrowUserInlinePolicy bounds a MEMBER's own inline policy by the
// capability grants that member holds. It runs after composer.Clamp and
// filterUserGrants, and the difference between them is the whole doctrine:
// the clamp bounds a member to what the OPERATOR authorized deployment-wide,
// this bounds what survived to what THIS member was granted personally. A
// stored-secret pairing therefore has to clear BOTH — operator-eligible AND
// granted here — because either one alone is a hole.
//
// It touches only the MEMBER-AUTHORED spec. Everything admin-authored — a
// stored policy, the workspace's requirements, the scan's seeded hosts, the
// model provider's own egress, the grants re-added at launch by
// applyWorkspaceRequirements — is folded in by the callers
// AFTER this returns, and is deliberately left alone: narrowing what an admin
// already authorized would brick workspace runs at scale, and a member who
// cannot be trusted with a workspace should not be granted the workspace.
//
// Drops, never rejects, exactly as filterUserGrants does — with a warning per
// drop, so preflight/Review names what will not be there before launch, and a
// capDrop so the audit stream records it, except an ungranted workspace_repos entry
// (#1259, errUngrantedWorkspaceRepo): a second door onto req.workspace_id's own REFUSAL.
// An otherwise-ungranted allowlist still gets a run with no member-authored egress, not
// a 403: the run's admin-authored egress is still there and is what the task usually needs.
//
// Under an operator ceiling of allow_all_egress the allowlist is not the gate
// at all (composer.Clamp leaves AllowAllEgress set and the proxy allows any
// non-denied public host), so egress_host narrowing does nothing there. That is
// the operator's own posture, named here so nobody reads a green switch as a
// bound that deployment does not have.
func (s *Server) narrowUserInlinePolicy(ctx context.Context, owner string, spec *types.RunPolicySpec) ([]string, []capDrop, error) {
	var warns []string
	var drops []capDrop

	// One resolution for the whole spec, not one per value. The egress loop
	// below walks spec.AllowedDomains, which is the REQUEST BODY's list —
	// uncapped and un-deduplicated on this path — so a per-value resolution
	// would let a member's own body decide how many sequential Postgres round
	// trips the handler performs. See capBatch. Installed as the resolution's
	// memo, so any one-value door asked further down shares this snapshot.
	ctx = withCapBatch(ctx)
	cap := s.capBatchFor(ctx)

	// One resolution per DISTINCT host, not per entry. The list is the request
	// body's, and nothing on this path de-duplicates it: validatePolicySpec has
	// no allowed_domains arm and composer.Clamp's intersection keeps every
	// duplicate that passes (and is skipped outright under an allow_all_egress
	// ceiling), so `["a","a",…,"a"]` bought one full grant-set match per copy.
	// Memoized rather than de-duplicated: every entry still gets its own warning
	// and its own capDrop, so preflight's output and the audit stream are
	// unchanged byte for byte — only the repeated work is gone. The answer is
	// deterministic within a batch (grants, enforcement and the group-deny
	// memo are all snapshotted by capBatch), so a cached one is the same answer.
	seen := make(map[string]bool, len(spec.AllowedDomains))
	keptDomains := spec.AllowedDomains[:0:0]
	for _, d := range spec.AllowedDomains {
		ok, cached := seen[d]
		if !cached {
			var err error
			ok, err = cap.allowed(ctx, capEgressHost, d)
			if err != nil {
				return nil, nil, err
			}
			seen[d] = ok
		}
		if !ok {
			warns = append(warns, fmt.Sprintf("dropped egress host %q: not granted to you", d))
			drops = append(drops, capDrop{reason: authz.ReasonCapabilityEgressHost, detail: d})
			continue
		}
		keptDomains = append(keptDomains, d)
	}
	spec.AllowedDomains = keptDomains

	keptGrants := spec.EligibleGrants[:0:0]
	for _, g := range spec.EligibleGrants {
		// filterUserGrants 422s an undecodable stored-secret scope — and, since
		// the pairing switch closed, an unknown kind too — before this runs, and
		// it is the only order that exists. The error is still HONORED here
		// rather than discarded: relying on that ordering is what let the old
		// open default arm through TWO gates instead of one, and an unreadable
		// pairing has no secretRef to check, so keeping it would be a free pass.
		_, secretRef, knownHostsRef, covered, derr := storedSecretGrantPairing(g)
		if covered && derr != nil {
			warns = append(warns, fmt.Sprintf("dropped %s grant: %v", g.Kind, derr))
			drops = append(drops, capDrop{reason: authz.ReasonCapabilitySecret, detail: string(g.Kind)})
			continue
		}
		if !covered {
			keptGrants = append(keptGrants, g) // github_token, cloud_sts name no stored secret
			continue
		}
		// Both refs, because an ssh_key grant's known_hosts_secret_ref resolves
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
			if s.ownsSecretMemoized(ctx, owner, ref) {
				continue
			}
			ok, err := cap.allowed(ctx, capSecret, ref)
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
			drops = append(drops, capDrop{reason: authz.ReasonCapabilitySecret, detail: refused})
			continue
		}
		keptGrants = append(keptGrants, g)
	}
	spec.EligibleGrants = keptGrants

	// Workspace repos. denyUserRequest gates the req.workspace_id door, but an
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
				ok, err := cap.allowed(ctx, capWorkspace, ws.ID.String())
				if err != nil {
					return nil, nil, err
				}
				if !ok {
					// Refused, not dropped (#1259) — see boundUserSpec.
					return nil, nil, &errUngrantedWorkspaceRepo{repo: wr.Repo, wsID: ws.ID.String()}
				}
			}
			keptRepos = append(keptRepos, wr)
		}
		spec.WorkspaceRepos = keptRepos
	}

	return warns, drops, nil
}

// auditUserPolicyDrops records what a member's inline policy LOST — one event
// per REASON, not one per dropped item. A spec naming twenty ungranted hosts is
// one authorization outcome, not twenty, and a stream that flooded on it is the
// first thing an operator would filter away. Grouping by reason (rather than
// blending every drop into one event) keeps `reason` the single value every
// authz.denied consumer already reads, with the affected values beside it.
func (s *Server) auditUserPolicyDrops(ctx context.Context, r *http.Request, drops []capDrop) {
	byReason := map[authz.Reason][]string{}
	for _, d := range drops {
		byReason[d.reason] = append(byReason[d.reason], d.detail)
	}
	for _, reason := range slices.Sorted(maps.Keys(byReason)) { // stable order for the stream
		s.recordRefusal(ctx, r, authz.Drop(reason, "runs.inline_policy", byReason[reason]))
	}
}
