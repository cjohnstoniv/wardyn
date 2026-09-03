// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// maxGrantTTLSeconds mirrors the broker ceiling (minted credentials live at most
// 1h). A proposal can never exceed it.
const maxGrantTTLSeconds = 3600

// confinementRank ranks isolation strength (higher = stronger). CC1<CC2<CC3.
// Composer input can be untrusted JSON, so normalise case/whitespace before the
// exact-match rank in types.
func confinementRank(c types.ConfinementClass) int {
	return types.ConfinementClass(strings.ToUpper(strings.TrimSpace(string(c)))).Rank()
}

// EffectiveConfinementFloor combines the operator policy's minimum confinement
// class with the operator's per-run compose floor (the Getting Started default
// tier, sent RAISE-ONLY on the request) and returns the class to floor the
// proposal at: the PER-RUN floor is first capped at the strongest class this
// host's runner can actually enforce, THEN max(policyMin, cappedFloor). The cap
// degrades the PER-RUN request floor ONLY — it keeps a CC3 default tier on a
// Fence-only host from flooring a composed run into a launch-time 422 at the
// confinement gate (internal/api/runs.go). It must NEVER lower the operator's
// configured policy minimum: an unenforceable POLICY min still fails closed at
// launch (the manual create-run path 422s it — invariant 5), and silently
// degrading it here would make compose the one path that bypasses an operator
// security control. cap=="" (unknown runner caps) means "do not cap".
// Feeding the result as Clamp's ceiling MinConfinementClass makes the raise flow
// through Clamp's EXISTING "confinement raised ..." warning — no new channel, so
// the review's "Tightened by policy" panel renders it with zero new UI.
func EffectiveConfinementFloor(policyMin, floor, cap types.ConfinementClass) types.ConfinementClass {
	// Cap the PER-RUN floor to what the host can enforce (degrades a too-strong
	// default tier); never touches policyMin, applied before the raise-only max.
	if confinementRank(cap) > 0 && confinementRank(floor) > confinementRank(cap) {
		floor = cap
	}
	eff := policyMin
	if confinementRank(floor) > confinementRank(eff) {
		eff = floor
	}
	return eff
}

// Clamp tightens a proposed RunPolicySpec to the operator's policy CEILING so the
// composer can never propose something more permissive than the operator allows.
// It returns the clamped spec and a human-readable warning for every tightening
// it performed. This is defense-in-depth on top of the deterministic risk grade:
// the grade informs the human, the clamp enforces the operator's hard limits
// regardless of what the (untrusted-input-driven) analyzer OR a member's own
// hand-authored inline_policy proposed.
//
// Clamps applied:
//   - confinement raised to the operator's minimum class if the proposal is weaker;
//   - allow_all_egress forced off unless the ceiling permits it;
//   - allowed_domains intersected down to the ceiling's allowlist (unless the
//     ceiling itself allows all egress);
//   - the ceiling's denied_domains unioned in (deny always wins);
//   - first_use_approval raised to the STRICTER of the two (always_deny >
//     deny_with_review > wait_for_review) — an unset ceiling ranks as
//     always_deny, its own documented fail-closed default (FirstUseMode.Normalize);
//   - llm_inspection: when the ceiling sets one, the proposal unconditionally
//     inherits it — nil means OFF, so an omitted/weaker proposal is exactly the
//     "disable the operator's prompt-inspection guardrail" escalation, not "no
//     opinion". A ceiling that sets NONE is likewise the FLOOR (nil), not "no
//     opinion" — symmetric with the workspace_mounts drop below: a member's own
//     hand-authored llm_inspection (detector_sidecar_url, intercept_tls, ...)
//     must never survive a nil ceiling unclamped. The inherited copy's
//     workspace_secret_values is always zeroed — a proposal/audit event never
//     needs resolved secret values, only the names (see types.LLMInspectionSpec);
//   - allowed_methods: empty means "all" (unlike allowed_domains' default-deny),
//     so an empty proposal ADOPTS the ceiling's list when the ceiling sets one,
//     and a non-empty proposal is intersected down to it;
//   - ui_apps: intersected down to the ceiling's (name, port) pairs when the
//     ceiling declares any, and untouched when it declares none — an empty
//     proposal is the NARROWEST state for this field, so it never adopts;
//   - resources: each of cpu/memory/pids/disk capped at the ceiling's when the
//     ceiling sets one — an unset (zero) proposed field is the PERMISSIVE state
//     here (filled in by the driver's own default later), so it is capped down
//     exactly like an explicit value that exceeds the ceiling;
//   - auto_stop_after_sec capped at the ceiling's maximum when the ceiling sets
//     one — 0 (platform default) and negative (never reap) both rank as MORE
//     permissive than a real cap and are capped down too;
//   - grants of a kind the ceiling does not list are dropped; github permissions
//     intersected down to the ceiling's github permissions; TTL capped; and
//     requires_approval forced on when the ceiling requires it;
//   - workspace_mounts dropped entirely — host mounts are operator-authored and a
//     composer (fed untrusted input) must never be able to introduce one.
func Clamp(proposed, ceiling types.RunPolicySpec) (types.RunPolicySpec, []string) {
	out := proposed
	var warns []string

	// Confinement floor.
	if cr := confinementRank(ceiling.MinConfinementClass); cr > 0 && confinementRank(out.MinConfinementClass) < cr {
		warns = append(warns, fmt.Sprintf("confinement raised from %q to operator minimum %q",
			out.MinConfinementClass, ceiling.MinConfinementClass))
		out.MinConfinementClass = ceiling.MinConfinementClass
	}

	// Allow-all egress.
	warns = clampOperatorSwitches(&out, ceiling, warns)

	// Per-tool effects: a proposal may narrow the operator's rules, never widen.
	warns = clampToolRules(&out, ceiling, warns)

	// Allowed domains: intersect down to the ceiling unless the ceiling allows all.
	// An empty ceiling allowlist means default-deny (mirrors clampGrants/egress
	// package semantics) — do NOT skip this block just because it's empty, or the
	// strictest operator posture silently lets a proposal's domains through.
	if !ceiling.AllowAllEgress {
		allowed := toSet(ceiling.AllowedDomains)
		kept, dropped := partition(out.AllowedDomains, func(d string) bool { return allowed[strings.ToLower(strings.TrimSpace(d))] })
		if len(dropped) > 0 {
			warns = append(warns, fmt.Sprintf("dropped %d egress domain(s) not in operator allowlist: %s", len(dropped), strings.Join(dropped, ",")))
			out.AllowedDomains = kept
		}
	}

	// Denied domains: union the ceiling's deny-list (deny always wins).
	if len(ceiling.DeniedDomains) > 0 {
		out.DeniedDomains = union(out.DeniedDomains, ceiling.DeniedDomains)
	}

	// First-use approval: never let the proposal be WEAKER than the ceiling — a
	// proposal asking wait_for_review under an operator ceiling of always_deny
	// (the field's own fail-closed default when unset — Normalize()) must not
	// silently reopen the exact escalation the ceiling exists to close. The
	// output is ALWAYS normalized (never a bare "" that only ranks correctly by
	// accident), even when no raise happens, so the clamped spec's own JSON is
	// never ambiguous about which of the three modes is actually in effect.
	effectiveFUA, ceilingFUA := out.FirstUseApproval.Normalize(), ceiling.FirstUseApproval.Normalize()
	if firstUseApprovalRank(ceilingFUA) > firstUseApprovalRank(effectiveFUA) {
		warns = append(warns, fmt.Sprintf("first_use_approval raised from %q to operator minimum %q", effectiveFUA, ceilingFUA))
		effectiveFUA = ceilingFUA
	}
	out.FirstUseApproval = effectiveFUA

	// LLM inspection: nil means OFF (RunPolicySpec's own doc: "the safe
	// default"), so an omitted/weaker proposal is not "no opinion" the way an
	// unset AllowedDomains ceiling is — it is an explicit "turn off the
	// operator's guardrail" whenever the ceiling turns one on. Unconditional
	// inherit, not a merge: this is a visibility/detection control, not
	// something a member's own choice should ever weaken.
	//
	// W12-A-1 (CRIT) / W14-S1-1: the mirror case matters just as much — a
	// ceiling that sets NONE (the shipped default.json's own posture) is the
	// FLOOR for this field too, not "no opinion". Before this fix the block
	// below only fired when the ceiling had an opinion, so under a nil ceiling
	// a member's own hand-authored inline_policy.llm_inspection passed straight
	// through unclamped — able to point detector_sidecar_url at any URL the
	// wardyn-proxy process can reach (a surface the sandbox's OWN confinement
	// class never bounds — see contentscan/sidecar.go) or flip intercept_tls,
	// with zero operator opinion in the way. Symmetric with the
	// workspace_mounts drop below: drop, don't pass through.
	if ceiling.LLMInspection != nil {
		warns = append(warns, "llm_inspection set to the operator's configured mode: "+ceiling.LLMInspection.Mode)
		cp := *ceiling.LLMInspection
		// W12-A-3: the inherited copy never carries resolved secret VALUES — a
		// compose/profile proposal is advisory output handed straight back to
		// the caller (and, before this fix, also embedded verbatim in the
		// run.compose audit event), and neither ever needs more than the
		// NAMES a reviewer needs to see which secrets are covered. Only
		// dispatch ever resolves names -> values, in memory, for the proxy
		// sidecar (runs_dispatch.go). Deep-copy the slices so this clamp never
		// aliases the ceiling's own backing arrays (types.RunPolicySpec.Clone's
		// same discipline).
		cp.WorkspaceSecretNames = append([]string(nil), ceiling.LLMInspection.WorkspaceSecretNames...)
		cp.WorkspaceSecretValues = nil
		cp.ClassifiedMarkers = append([]string(nil), ceiling.LLMInspection.ClassifiedMarkers...)
		out.LLMInspection = &cp
		// bug-policy-1: the AllowedDomains intersection above already ran and
		// narrowed out.AllowedDomains to the PROPOSAL's own (narrower) list —
		// it never had a reason to keep the ceiling's detector_sidecar_url
		// host, since AT THAT POINT the ceiling hadn't inherited yet. But
		// validateLLMInspection (internal/api/policy.go) requires the
		// inherited sidecar's host to be an EXACT entry on THIS SAME
		// (post-clamp) spec's own allowed_domains — so every compose/
		// profile/inline_policy run under an operator ceiling that sets
		// detector_sidecar_url would self-reject unless the proposal
		// happened to already allowlist that exact host. Union it in now,
		// the same way DeniedDomains is already unioned in above — it does
		// not widen egress by itself (the sidecar dial never goes through
		// the sandbox's own allowlist, per validateLLMInspection's own
		// comment), it only lets the ceiling's own inherited config pass
		// its own validation.
		if !out.AllowAllEgress {
			if u, uerr := url.Parse(strings.TrimSpace(ceiling.LLMInspection.DetectorSidecarURL)); uerr == nil && u.Hostname() != "" {
				out.AllowedDomains = union(out.AllowedDomains, []string{u.Hostname()})
			}
		}
	} else if out.LLMInspection != nil {
		warns = append(warns, "llm_inspection dropped: operator policy sets none, so a proposal/inline llm_inspection "+
			"(incl. detector_sidecar_url/intercept_tls) can never survive the clamp")
		out.LLMInspection = nil
	}

	// Allowed methods: empty means "all" for THIS field (unlike AllowedDomains'
	// default-deny empty), so an empty proposal must ADOPT the ceiling's
	// restriction rather than silently keeping "any method" under a ceiling that
	// sets one; a non-empty proposal is intersected down, same as domains.
	if len(ceiling.AllowedMethods) > 0 {
		if len(out.AllowedMethods) == 0 {
			warns = append(warns, fmt.Sprintf("allowed_methods restricted to operator's: %s", strings.Join(ceiling.AllowedMethods, ",")))
			out.AllowedMethods = append([]string(nil), ceiling.AllowedMethods...)
		} else {
			allowed := toSet(ceiling.AllowedMethods)
			kept, dropped := partition(out.AllowedMethods, func(m string) bool { return allowed[strings.ToLower(strings.TrimSpace(m))] })
			if len(dropped) > 0 {
				warns = append(warns, fmt.Sprintf("dropped %d method(s) not in operator allowlist: %s", len(dropped), strings.Join(dropped, ",")))
				out.AllowedMethods = kept
			}
		}
	}

	out.UIApps = clampUIApps(out.UIApps, ceiling.UIApps, &warns)

	// Grants: drop unknown kinds, intersect github perms, cap TTL, force approval.
	out.EligibleGrants = clampGrants(out.EligibleGrants, ceiling, &warns)

	// Resources: cap each set field at the ceiling's, when the ceiling sets one.
	// W14-S1-3: an UNSET ceiling used to opine nothing at all — skipped
	// entirely — which left a proposal's own CPU/memory/pids request
	// completely uncapped under the shipped default.json ceiling (it sets no
	// Resources block). Clamp is the operator-ceiling authority for every
	// caller that reaches it (a member's inline_policy, any compose/profile
	// proposal); it must never hand back an unbounded sandbox just because
	// the operator never bothered to opine. Fall back to the same
	// conservative platform defaults CreateSandbox itself applies when a
	// Resources field is zero (runner.Default{CPUMillis,MemoryMiB,PidsLimit})
	// — the ceiling, and this clamp, then agree with what the sandbox would
	// enforce anyway. DiskMiB has no platform default (CreateSandbox leaves
	// it opt-in), so it stays skip-when-both-unset exactly as before.
	effCeilingResources := ceiling.Resources
	if effCeilingResources == nil {
		effCeilingResources = &types.ResourceLimits{
			CPUMillis: int(runner.DefaultCPUMillis),
			MemoryMiB: int(runner.DefaultMemoryMiB),
			PidsLimit: int(runner.DefaultPidsLimit),
		}
	}
	{
		before := out.Resources
		cr := types.ResourceLimits{}
		if before != nil {
			cr = *before
		}
		capField := func(proposed *int, ceil int) {
			if ceil > 0 && (*proposed <= 0 || *proposed > ceil) {
				*proposed = ceil
			}
		}
		capField(&cr.CPUMillis, effCeilingResources.CPUMillis)
		capField(&cr.MemoryMiB, effCeilingResources.MemoryMiB)
		capField(&cr.PidsLimit, effCeilingResources.PidsLimit)
		capField(&cr.DiskMiB, effCeilingResources.DiskMiB)
		if before == nil || cr != *before {
			warns = append(warns, "resources capped to operator maximum")
			out.Resources = &cr
		}
	}

	// Auto-stop: cap at the ceiling's maximum when the ceiling sets a real
	// (positive) one. 0 (platform default, filled in later, unrelated to any
	// ceiling) and a negative value (never reap — the MOST permissive: a
	// compromised/runaway agent lives forever) both rank as more permissive than
	// an explicit cap and are capped down exactly like an excessive positive one.
	if ceiling.AutoStopAfterSec > 0 && (out.AutoStopAfterSec <= 0 || out.AutoStopAfterSec > ceiling.AutoStopAfterSec) {
		warns = append(warns, fmt.Sprintf("auto_stop_after_sec capped to operator maximum %ds", ceiling.AutoStopAfterSec))
		out.AutoStopAfterSec = ceiling.AutoStopAfterSec
	} else if ceiling.AutoStopAfterSec <= 0 && out.AutoStopAfterSec < 0 {
		// W14-S1-3: the ceiling sets no real cap, but a proposal choosing a
		// NEGATIVE auto_stop is asking for "never reap" — the single most
		// permissive value there is, strictly worse than simply inheriting
		// the platform default (0, filled in later). A member/proposal may
		// not opt a run OUT of the reaper entirely; clamp to 0.
		warns = append(warns, "auto_stop_after_sec: negative (never reap) is not allowed; reset to the platform default")
		out.AutoStopAfterSec = 0
	}

	// Workspace mounts: NEVER composer-introduced. Operators author mounts on a
	// stored policy; an analyzer fed untrusted input must not be able to mount a
	// host path. Drop any the proposal carried.
	if len(out.WorkspaceMounts) > 0 {
		warns = append(warns, fmt.Sprintf("dropped %d proposed workspace mount(s): host mounts are operator-authored, never composer-proposed", len(out.WorkspaceMounts)))
		out.WorkspaceMounts = nil
	}

	return out, warns
}

// clampUIApps bounds a proposal's UI apps. The ceiling's list is an ALLOWLIST of (name, port) pairs when it
// sets one, and no opinion at all when it does not.
//
// The asymmetry is allowed_methods' above — a ceiling that sets the field
// bounds the proposal, a silent ceiling leaves it — but the ADOPT half of that
// arm deliberately does NOT carry over, and the difference is the field's own
// empty semantics. An empty allowed_methods means "every method", so adopting
// the ceiling's list narrows; an empty ui_apps means "this run has no UI apps"
// (RunPolicySpec.UIApps' own doc), so adopting would HAND a run relay access it
// never asked for — a widening, performed by the clamp, in the name of a
// ceiling that was trying to bound it.
//
// Matched on name AND port. A UIApp reaches the gateway as a declared loopback
// port it will relay to a browser, so keeping an entry by name alone would let
// a proposal name the ceiling's app and point it at any port in the sandbox.
// Path is the landing path only and is left as proposed.
func clampUIApps(proposed, ceiling []types.UIApp, warns *[]string) []types.UIApp {
	if len(ceiling) == 0 || len(proposed) == 0 {
		return proposed
	}
	var kept []types.UIApp
	var dropped []string
	for _, a := range proposed {
		if slices.ContainsFunc(ceiling, func(c types.UIApp) bool { return c.Name == a.Name && c.Port == a.Port }) {
			kept = append(kept, a)
			continue
		}
		dropped = append(dropped, fmt.Sprintf("%s:%d", a.Name, a.Port))
	}
	if len(dropped) == 0 {
		return proposed
	}
	*warns = append(*warns, fmt.Sprintf("dropped %d ui_app(s) not in operator allowlist: %s", len(dropped), strings.Join(dropped, ",")))
	return kept
}

// firstUseApprovalRank ranks FirstUseMode strictness — always_deny (never lets
// an unknown domain through without a human) is STRICTEST, wait_for_review
// (transparently completes once approved) is loosest. Normalize() first so an
// empty/garbage value ranks as always_deny (its own fail-closed default), never
// as an unranked value a real mode could accidentally beat.
func firstUseApprovalRank(m types.FirstUseMode) int {
	switch m.Normalize() {
	case types.FirstUseAlwaysDeny:
		return 3
	case types.FirstUseDenyWithReview:
		return 2
	default: // types.FirstUseWaitForReview
		return 1
	}
}

// ClampRunConfinement raises a proposed run's confinement class up to the clamped
// policy floor so the composer never emits a self-inconsistent proposal: a run
// advertising a WEAKER class than its inline_policy's MinConfinementClass would be
// rejected 422 by handleCreateRun (invariant 5, fail closed). It ONLY strengthens —
// a run that legitimately asked for a class STRONGER than the floor is left as-is —
// and an empty/unknown run class ranks 0, so it too is raised to the floor. Returns
// the (possibly raised) class and a non-empty warning when it tightened.
func ClampRunConfinement(runClass string, floor types.ConfinementClass) (string, string) {
	if fr := confinementRank(floor); fr > 0 && confinementRank(types.ConfinementClass(runClass)) < fr {
		return string(floor), fmt.Sprintf("run confinement raised from %q to policy floor %q", runClass, floor)
	}
	return runClass, ""
}

// clampGrants narrows each proposed grant to the bound of the ceiling grant that
// covers it, and drops any whose KIND the ceiling does not carry.
//
// WHICH ceiling grant supplies the bound is ceilingGrantsBounding's answer, not
// a kind-keyed map's — see grantbound.go for why that map was the defect: with
// two same-kind ceiling entries it let the LAST one supply the approval posture
// and TTL for a proposal naming the FIRST one's pairing, which both stripped an
// operator-mandated requires_approval and made the result depend on the order of
// a set.
//
// The bounds are MET across whatever ceilingGrantsBounding returns (one grant
// when the pairing matched, every same-kind grant otherwise): the TTL cap is the
// minimum, requires_approval is forced on if ANY of them requires it, and a
// github scope is intersected against each in turn. Meeting can only narrow, so
// no path through this function can hand a run a wider bound than some ceiling
// grant actually wrote.
func clampGrants(grants []types.GrantSpec, ceiling types.RunPolicySpec, warns *[]string) []types.GrantSpec {
	if len(grants) == 0 {
		return grants
	}
	var out []types.GrantSpec
	for _, g := range grants {
		bounds := ceilingGrantsBounding(g, ceiling.EligibleGrants)
		if len(bounds) == 0 {
			*warns = append(*warns, fmt.Sprintf("dropped grant %q: not in operator's eligible grants", g.Kind))
			continue
		}
		// GitHub: intersect repos+permissions down to EVERY bounding ceiling
		// grant's. Sequential intersection is the meet here — each pass can only
		// remove a repo or lower a permission — and the warnings are deduped
		// because the same removal seen against three ceiling entries is one
		// fact, not three.
		if g.Kind == types.GrantGitHubToken {
			var scopeWarns []string
			for _, cg := range bounds {
				g.Scope = clampGitHubScope(g.Scope, cg.Scope, &scopeWarns)
			}
			*warns = append(*warns, dedupeStrings(scopeWarns)...)
		}
		// TTL cap: the STRICTEST bound. A ceiling TTL of 0 (or negative) bounds
		// nothing and leaves the broker maximum standing, exactly as before.
		max := maxGrantTTLSeconds
		for _, cg := range bounds {
			if cg.TTLSeconds > 0 && cg.TTLSeconds < max {
				max = cg.TTLSeconds
			}
		}
		if g.TTLSeconds == 0 || g.TTLSeconds > max {
			if g.TTLSeconds > max {
				*warns = append(*warns, fmt.Sprintf("grant %q TTL capped to %ds", g.Kind, max))
			}
			g.TTLSeconds = max
		}
		// Approval: the operator can only TIGHTEN — if ANY bounding ceiling
		// grant requires approval, force it on.
		if !g.RequiresApproval && slices.ContainsFunc(bounds,
			func(cg types.GrantSpec) bool { return cg.RequiresApproval }) {
			*warns = append(*warns, fmt.Sprintf("grant %q forced to require approval (operator policy)", g.Kind))
			g.RequiresApproval = true
		}
		out = append(out, g)
	}
	return out
}

// dedupeStrings keeps the first occurrence of each value, preserving order.
func dedupeStrings(in []string) []string {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]bool, len(in))
	out := in[:0:0]
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// clampGitHubScope intersects a proposed github scope's repos+permissions down to
// the ceiling scope. A permission level is never raised above the ceiling's, and
// repos not present in the ceiling are dropped.
func clampGitHubScope(proposed, ceiling json.RawMessage, warns *[]string) json.RawMessage {
	type ghScope struct {
		Repos       []string          `json:"repos"`
		Permissions map[string]string `json:"permissions"`
	}
	var p, c ghScope
	_ = json.Unmarshal(proposed, &p)
	if len(ceiling) > 0 {
		_ = json.Unmarshal(ceiling, &c)
	}
	// Repos: intersect to the ceiling. W23-S1-3 (RBAC-bypass): an EMPTY ceiling
	// repo list is DENY-ALL, not "any repo" — it used to skip this block
	// entirely on the theory that the proposal's repos are already grounded to
	// the workspace's ACTUAL detected remote by groundGitHubGrants before the
	// clamp, so an empty ceiling only ever restricts by PERMISSIONS (below).
	// That holds for the composer/profile pipelines, which DO ground a
	// proposal's repos to something real (an actually-detected git remote, an
	// operator-selected git workspace, or a prior run's already-clamped grant)
	// before calling Clamp — and which now widen their OWN ceiling copy to
	// that grounded set first (widenCeilingRepoAllowlist, internal/api/
	// compose.go) so this unconditional intersection never drops their
	// legitimate access. It does NOT hold for a hand-authored spec (a member's
	// own inline_policy, clamped with no grounding step ahead of it): under
	// the shipped default.json ceiling (github_token with "repos": []) the old
	// skip let a member's own arbitrary repo list survive verbatim — an
	// unbounded RBAC escalation. Always intersecting closes that gap: an empty
	// ceiling now denies everything for anything nothing has grounded.
	allowed := toSet(c.Repos)
	kept, dropped := partition(p.Repos, func(r string) bool { return allowed[strings.ToLower(strings.TrimSpace(r))] })
	if len(dropped) > 0 {
		*warns = append(*warns, fmt.Sprintf("github grant: dropped %d repo(s) outside operator scope", len(dropped)))
	}
	p.Repos = kept
	// Permissions: keep only those the ceiling allows, never above the ceiling
	// level. Deny-by-default (M6): an absent/empty ceiling permission map grants NO
	// permissions, so every proposed permission (incl. write/admin) is dropped —
	// previously guarded by `c.Permissions != nil`, which let them pass untouched.
	// This is the real M6 fix: the LLM can never obtain a permission the operator
	// ceiling doesn't bless, even when the ceiling places no repo-level allowlist.
	for perm, lvl := range p.Permissions {
		cl, ok := c.Permissions[perm]
		if !ok {
			*warns = append(*warns, fmt.Sprintf("github grant: dropped permission %q (not in operator policy)", perm))
			delete(p.Permissions, perm)
			continue
		}
		if permRank(lvl) > permRank(cl) {
			*warns = append(*warns, fmt.Sprintf("github grant: permission %q clamped %s→%s", perm, lvl, cl))
			p.Permissions[perm] = cl
		}
	}
	b, err := json.Marshal(p)
	if err != nil {
		return proposed
	}
	return b
}

// GitHubScopeWithin reports whether a PROPOSED github_token scope stays inside a
// CEILING's, by the exact rules clampGitHubScope enforces above: the ceiling's
// repo list is an ALLOWLIST (an empty one is deny-all, W23-S1-3), a permission
// absent from the ceiling map is not grantable at any level (M6), and a present
// one bounds the level by permRank.
//
// It lives HERE, beside the clamp it must agree with, and calls that clamp's own
// permRank and toSet — because its caller is a comparator that ACCEPTS a scope
// the clamp will later honor at mint. A second copy of these rules in the
// caller's package drifts in exactly the direction that matters: accept there,
// widen here. There is nothing to keep in sync because there is one copy.
//
// It differs from the clamp in one deliberate way: an UNDECODABLE scope is an
// error, not an empty one. The clamp tolerates it because it intersects toward
// empty (fail-closed); a comparator reading it as "no constraint" fails OPEN.
func GitHubScopeWithin(proposed, ceiling json.RawMessage) error {
	type ghScope struct {
		Repos       []string          `json:"repos"`
		Permissions map[string]string `json:"permissions"`
	}
	var p, c ghScope
	if len(proposed) > 0 {
		if err := json.Unmarshal(proposed, &p); err != nil {
			return fmt.Errorf("github scope is not decodable: %w", err)
		}
	}
	if len(ceiling) > 0 {
		if err := json.Unmarshal(ceiling, &c); err != nil {
			return fmt.Errorf("the deployment ceiling's github scope is not decodable: %w", err)
		}
	}
	allowed := toSet(c.Repos)
	for _, r := range p.Repos {
		if !allowed[strings.ToLower(strings.TrimSpace(r))] {
			return fmt.Errorf("github repo %q is outside the deployment ceiling's repo scope", r)
		}
	}
	for perm, lvl := range p.Permissions {
		cl, ok := c.Permissions[perm]
		if !ok {
			return fmt.Errorf("github permission %q is not in the deployment ceiling's permissions", perm)
		}
		if permRank(lvl) > permRank(cl) {
			return fmt.Errorf("github permission %q at %q is above the deployment ceiling's %q", perm, lvl, cl)
		}
	}
	return nil
}

func permRank(level string) int {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "admin":
		return 3
	case "write":
		return 2
	case "read":
		return 1
	default:
		return 0
	}
}

func toSet(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[strings.ToLower(strings.TrimSpace(x))] = true
	}
	return m
}

func partition(xs []string, keep func(string) bool) (kept, dropped []string) {
	for _, x := range xs {
		if keep(x) {
			kept = append(kept, x)
		} else {
			dropped = append(dropped, x)
		}
	}
	return kept, dropped
}

func union(a, b []string) []string {
	seen := toSet(a)
	out := append([]string(nil), a...)
	for _, x := range b {
		if !seen[strings.ToLower(strings.TrimSpace(x))] {
			out = append(out, x)
			seen[strings.ToLower(strings.TrimSpace(x))] = true
		}
	}
	sort.Strings(out)
	return out
}

// clampOperatorSwitches forces off the boolean controls only an operator may
// widen: allow-all egress, and the push branch-namespace opt-out
// (git_push_any_branch). A member's inline_policy may not switch either on
// unless the ceiling already has — without the second clamp a member could
// disable the operator's push confinement for their own run by posting the
// field. Split out of Clamp so its branch count stays under the gocyclo gate.
func clampOperatorSwitches(out *types.RunPolicySpec, ceiling types.RunPolicySpec, warns []string) []string {
	if out.AllowAllEgress && !ceiling.AllowAllEgress {
		warns = append(warns, "allow_all_egress disabled: operator policy does not permit allow-all egress")
		out.AllowAllEgress = false
	}
	if out.GitPushAnyBranch && !ceiling.GitPushAnyBranch {
		warns = append(warns, "git_push_any_branch disabled: operator policy keeps push branch-namespace confinement on")
		out.GitPushAnyBranch = false
	}
	return warns
}

// toolStrictness orders the three effects so a clamp can take the stricter:
// allow < hold < deny.
func toolStrictness(e types.ToolEffect) int {
	switch e {
	case types.ToolDeny:
		return 2
	case types.ToolHold:
		return 1
	}
	return 0
}

// ceilingToolEffect is what the operator ceiling says about one tool: its
// exact rule, else its "*" default, else hold — the proxy's own lookup order
// (proxy.Policy.ToolEffectFor), so the clamp and the enforcement agree.
func ceilingToolEffect(ceiling types.RunPolicySpec, tool string) types.ToolEffect {
	def := types.ToolHold
	for _, r := range ceiling.ToolRules {
		if r.Tool == tool {
			return r.Effect
		}
		if r.Tool == "*" {
			def = r.Effect
		}
	}
	return def
}

// clampToolRules keeps a member's inline tool_rules from widening the
// operator's. Per tool the stricter effect wins, and the ceiling's own rules
// are carried into the result so a tool the operator denies stays denied when
// the proposal is silent about it. ToolRules' contract is "it narrows; it
// never widens" — that holds because the field is operator-authored, and the
// inline_policy seam is exactly where someone else authors it: without this
// clamp a member could post `{"tool":"*","effect":"allow"}` and turn a
// supervised run autonomous.
func clampToolRules(out *types.RunPolicySpec, ceiling types.RunPolicySpec, warns []string) []string {
	if len(out.ToolRules) == 0 {
		if len(ceiling.ToolRules) > 0 {
			out.ToolRules = append([]types.ToolRule(nil), ceiling.ToolRules...)
		}
		return warns
	}
	kept := make([]types.ToolRule, 0, len(out.ToolRules)+len(ceiling.ToolRules))
	seen := map[string]bool{}
	for _, r := range out.ToolRules {
		if floor := ceilingToolEffect(ceiling, r.Tool); toolStrictness(r.Effect) < toolStrictness(floor) {
			warns = append(warns, fmt.Sprintf("tool_rules: %q raised from %s to the operator's %s", r.Tool, r.Effect, floor))
			r.Effect = floor
		}
		kept = append(kept, r)
		seen[r.Tool] = true
	}
	for _, r := range ceiling.ToolRules {
		if !seen[r.Tool] {
			kept = append(kept, r)
		}
	}
	out.ToolRules = kept
	return warns
}
