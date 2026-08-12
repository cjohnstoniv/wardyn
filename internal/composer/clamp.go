// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

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
//     opinion";
//   - allowed_methods: empty means "all" (unlike allowed_domains' default-deny),
//     so an empty proposal ADOPTS the ceiling's list when the ceiling sets one,
//     and a non-empty proposal is intersected down to it;
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
	if out.AllowAllEgress && !ceiling.AllowAllEgress {
		warns = append(warns, "allow_all_egress disabled: operator policy does not permit allow-all egress")
		out.AllowAllEgress = false
	}

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
	if ceiling.LLMInspection != nil {
		warns = append(warns, "llm_inspection set to the operator's configured mode: "+ceiling.LLMInspection.Mode)
		cp := *ceiling.LLMInspection
		out.LLMInspection = &cp
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

	// Grants: drop unknown kinds, intersect github perms, cap TTL, force approval.
	out.EligibleGrants = clampGrants(out.EligibleGrants, ceiling, &warns)

	// Resources: cap each set field at the ceiling's, when the ceiling sets one.
	// An unset ceiling opines nothing — skip entirely (matches the majority of
	// existing policies, which never touch this field).
	if ceiling.Resources != nil {
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
		capField(&cr.CPUMillis, ceiling.Resources.CPUMillis)
		capField(&cr.MemoryMiB, ceiling.Resources.MemoryMiB)
		capField(&cr.PidsLimit, ceiling.Resources.PidsLimit)
		capField(&cr.DiskMiB, ceiling.Resources.DiskMiB)
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

func clampGrants(grants []types.GrantSpec, ceiling types.RunPolicySpec, warns *[]string) []types.GrantSpec {
	if len(grants) == 0 {
		return grants
	}
	// Index ceiling grants by kind for permission/approval ceilings.
	ceilByKind := map[types.GrantKind]types.GrantSpec{}
	for _, cg := range ceiling.EligibleGrants {
		ceilByKind[cg.Kind] = cg
	}
	var out []types.GrantSpec
	for _, g := range grants {
		cg, ok := ceilByKind[g.Kind]
		if !ok {
			*warns = append(*warns, fmt.Sprintf("dropped grant %q: not in operator's eligible grants", g.Kind))
			continue
		}
		// GitHub: intersect permissions down to the ceiling's permissions.
		if g.Kind == types.GrantGitHubToken {
			g.Scope = clampGitHubScope(g.Scope, cg.Scope, warns)
		}
		// TTL cap.
		max := maxGrantTTLSeconds
		if cg.TTLSeconds > 0 && cg.TTLSeconds < max {
			max = cg.TTLSeconds
		}
		if g.TTLSeconds == 0 || g.TTLSeconds > max {
			if g.TTLSeconds > max {
				*warns = append(*warns, fmt.Sprintf("grant %q TTL capped to %ds", g.Kind, max))
			}
			g.TTLSeconds = max
		}
		// Approval: the operator can only TIGHTEN — if the ceiling requires
		// approval, force it on.
		if cg.RequiresApproval && !g.RequiresApproval {
			*warns = append(*warns, fmt.Sprintf("grant %q forced to require approval (operator policy)", g.Kind))
			g.RequiresApproval = true
		}
		out = append(out, g)
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
	// Repos: intersect to the ceiling ONLY when the ceiling lists repos. An empty
	// ceiling repo list is "no repo ALLOWLIST" (any repo), not deny-all — the
	// proposal's repos are already ground to the workspace's ACTUAL detected remote
	// by groundGitHubGrants before the clamp, so the repo identity is controlled by
	// detection, and the operator restricts by PERMISSIONS (below), not by repo list.
	// (Denying on an empty repo list would drop the legitimately-detected repo.)
	if len(c.Repos) > 0 {
		allowed := toSet(c.Repos)
		kept, dropped := partition(p.Repos, func(r string) bool { return allowed[strings.ToLower(strings.TrimSpace(r))] })
		if len(dropped) > 0 {
			*warns = append(*warns, fmt.Sprintf("github grant: dropped %d repo(s) outside operator scope", len(dropped)))
		}
		p.Repos = kept
	}
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
