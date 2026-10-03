// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/runner/sizing"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// maxGrantTTLSeconds mirrors the broker ceiling (credentials live at most 1h); a proposal can never exceed it.
const maxGrantTTLSeconds = 3600

// confinementRank ranks isolation strength (CC1<CC2<CC3); normalizes case/whitespace since composer input can be untrusted JSON.
func confinementRank(c types.ConfinementClass) int {
	return types.ConfinementClass(strings.ToUpper(strings.TrimSpace(string(c)))).Rank()
}

// EffectiveConfinementFloor caps the per-run floor (the Getting Started default tier, sent
// raise-only) at the strongest class this host's runner can enforce, then returns
// max(policyMin, cappedFloor). The cap degrades only the per-run floor, never policyMin — an
// unenforceable policy minimum still fails closed at launch (invariant 5); degrading it here would
// make compose the one path that bypasses an operator security control. cap=="" means "do not cap".
// Feeding the result into Clamp's ceiling reuses its existing "confinement raised" warning.
func EffectiveConfinementFloor(policyMin, floor, cap types.ConfinementClass) types.ConfinementClass {
	if confinementRank(cap) > 0 && confinementRank(floor) > confinementRank(cap) {
		floor = cap
	}
	eff := policyMin
	if confinementRank(floor) > confinementRank(eff) {
		eff = floor
	}
	return eff
}

// Clamp tightens a proposed RunPolicySpec to the operator's policy ceiling so the composer can never
// propose something more permissive than the operator allows, returning the clamped spec and a
// warning for every tightening performed. Defense-in-depth alongside the deterministic risk grade:
// the grade informs the human, this enforces the operator's hard limits regardless of what an
// untrusted-input-driven analyzer or a member's own inline_policy proposed.
//
// The returned spec owns its memory (see cloneProposal): it shares no backing array or pointee with
// either argument, so a later mutation of either side can never move an already-enforced ceiling.
//
// limits is the acting principal's GovernanceLimits; only its size caps (MaxCPUMillis, MaxMemoryMiB,
// MaxEphemeralDiskMiB; 0 = unlimited) are read here. Disk is applied via CapDiskMiB, shared with
// dispatch's own ephemeral-disk clamp so preview and the actual run agree, and only clamps a non-zero
// request since 0 means unbounded scratch. CPU and memory fold into the resource ceiling below.
func Clamp(proposed, ceiling types.RunPolicySpec, limits types.GovernanceLimits) (types.RunPolicySpec, []string) {
	out := cloneProposal(proposed)
	var warns []string

	if cr := confinementRank(ceiling.MinConfinementClass); cr > 0 && confinementRank(out.MinConfinementClass) < cr {
		warns = append(warns, fmt.Sprintf("confinement raised from %q to operator minimum %q",
			out.MinConfinementClass, ceiling.MinConfinementClass))
		out.MinConfinementClass = ceiling.MinConfinementClass
	}

	warns = clampOperatorSwitches(&out, ceiling, warns)

	// Per-tool effects: a proposal may narrow the operator's rules, never widen.
	warns = clampToolRules(&out, ceiling, warns)

	// Allowed domains: intersect to the ceiling's allowlist unless it allows all. An empty ceiling
	// allowlist means default-deny — don't skip this block when empty, or the strictest posture leaks.
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

	// First-use approval must never be weaker than the ceiling: a wait_for_review proposal under an
	// always_deny ceiling must not reopen that escalation. Always normalize the output, even absent a raise.
	effectiveFUA, ceilingFUA := out.FirstUseApproval.Normalize(), ceiling.FirstUseApproval.Normalize()
	if firstUseApprovalRank(ceilingFUA) > firstUseApprovalRank(effectiveFUA) {
		warns = append(warns, fmt.Sprintf("first_use_approval raised from %q to operator minimum %q", effectiveFUA, ceilingFUA))
		effectiveFUA = ceilingFUA
	}
	out.FirstUseApproval = effectiveFUA

	// LLM inspection: nil means OFF. An omitted/weaker proposal or a ceiling that sets NONE is an
	// explicit "turn off the guardrail" escalation, not "no opinion" — inherit the ceiling unconditionally
	// (never merge) so it clears any member-authored llm_inspection, symmetric with workspace_mounts below.
	if ceiling.LLMInspection != nil {
		warns = append(warns, "llm_inspection set to the operator's configured mode: "+ceiling.LLMInspection.Mode)
		cp := *ceiling.LLMInspection
		// Never carries resolved secret VALUES (names only); deep-copy so this never aliases the ceiling's slices.
		cp.WorkspaceSecretNames = append([]string(nil), ceiling.LLMInspection.WorkspaceSecretNames...)
		cp.WorkspaceSecretValues = nil
		cp.ClassifiedMarkers = append([]string(nil), ceiling.LLMInspection.ClassifiedMarkers...)
		out.LLMInspection = &cp
		// Union the sidecar host into allowed_domains so it passes validateLLMInspection's own check —
		// this doesn't widen egress (the sidecar dial bypasses the sandbox allowlist).
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

	// Allowed methods: empty means "all" here (unlike AllowedDomains' default-deny), so an empty
	// proposal must adopt the ceiling's restriction rather than silently allowing any method.
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

	out.EligibleGrants = clampGrants(out.EligibleGrants, ceiling, &warns)

	warns = clampResources(&out, ceiling, limits, warns)

	// Auto-stop: cap at the ceiling's positive maximum. 0 (platform default) and negative (never reap —
	// most permissive) both rank more permissive than an explicit cap and are capped the same way. When
	// the ceiling sets no positive maximum, nothing happens: lifecycle's reaper treats any value <=0 as
	// never-reaped, so 0 and -1 are the same outcome and rewriting one to the other changes nothing.
	if ceiling.AutoStopAfterSec > 0 && (out.AutoStopAfterSec <= 0 || out.AutoStopAfterSec > ceiling.AutoStopAfterSec) {
		warns = append(warns, fmt.Sprintf("auto_stop_after_sec capped to operator maximum %ds", ceiling.AutoStopAfterSec))
		out.AutoStopAfterSec = ceiling.AutoStopAfterSec
	}

	// Workspace mounts are never composer-introduced: drop any the proposal carried (host paths are operator-authored only).
	if len(out.WorkspaceMounts) > 0 {
		warns = append(warns, fmt.Sprintf("dropped %d proposed workspace mount(s): host mounts are operator-authored, never composer-proposed", len(out.WorkspaceMounts)))
		out.WorkspaceMounts = nil
	}

	warns = clampPushRules(&out, ceiling, warns)

	return out, warns
}

// clampResources is Clamp's resource step, split out to keep Clamp readable.
func clampResources(out *types.RunPolicySpec, ceiling types.RunPolicySpec, limits types.GovernanceLimits, warns []string) []string {
	// Resources: cap each set field at the ceiling's; an unset ceiling is not "no opinion" (Clamp is the
	// operator-ceiling authority for every caller), so fall back to
	// (sizing.EffectiveLimits(): the deployment's configured default, else
	// sizing.Default{CPUMillis,MemoryMiB,PidsLimit}) rather than leave it unbounded. DiskMiB has no
	// such default and stays skip-when-both-unset.
	effCeilingResources := ceiling.Resources
	if effCeilingResources == nil {
		eff := sizing.EffectiveLimits()
		effCeilingResources = &types.ResourceLimits{
			CPUMillis: int(eff.CPUMillis),
			MemoryMiB: int(eff.MemoryMiB),
			PidsLimit: int(eff.PidsLimit),
		}
	}
	// The profile's own CPU/memory maximum is one more ceiling on the same fields, so a fill
	// from the ceiling and a cut of a request both stop at it.
	if limits.MaxCPUMillis > 0 || limits.MaxMemoryMiB > 0 {
		eff := sizing.EffectiveLimits()
		capped := *effCeilingResources
		capped.CPUMillis = capToLimit(capped.CPUMillis, limits.MaxCPUMillis, int(eff.CPUMillis))
		capped.MemoryMiB = capToLimit(capped.MemoryMiB, limits.MaxMemoryMiB, int(eff.MemoryMiB))
		effCeilingResources = &capped
	}
	{
		before := out.Resources
		cr := types.ResourceLimits{}
		if before != nil {
			cr = *before
		}
		// An unset (<=0) field is FILLED from the ceiling silently: the policy
		// asked for no particular size, so nothing it asked for was cut. Only a
		// positive request above the ceiling is a cap, and only a cap warns.
		exceeded := false
		capField := func(proposed *int, ceil int) {
			if ceil <= 0 {
				return
			}
			if *proposed > ceil {
				exceeded = true
			}
			if *proposed <= 0 || *proposed > ceil {
				*proposed = ceil
			}
		}
		capField(&cr.CPUMillis, effCeilingResources.CPUMillis)
		capField(&cr.MemoryMiB, effCeilingResources.MemoryMiB)
		capField(&cr.PidsLimit, effCeilingResources.PidsLimit)
		capField(&cr.DiskMiB, effCeilingResources.DiskMiB)
		// Governance limit folded into the same warning; bounds a non-zero request only (see CapDiskMiB).
		if capped := CapDiskMiB(cr.DiskMiB, limits.MaxEphemeralDiskMiB); capped != cr.DiskMiB {
			cr.DiskMiB, exceeded = capped, true
		}
		if before == nil || cr != *before {
			out.Resources = &cr
		}
		if exceeded {
			warns = append(warns, WarnResourcesCapped)
		}
	}
	return warns
}

// clampPushRules only narrows push_rules: a silent ceiling passes the proposal through unclamped;
// when the ceiling sets one, an unset proposal inherits it wholesale, a set proposal gets the
// ceiling's deny_paths unioned in (see unionPaths), max_inspect_pack_mib and max_file_size_mib capped
// (a proposal can neither raise a ceiling's limit nor turn it off with 0), and deny_new_executables kept on.
//
// IsSet treats an all-zero-but-non-nil PushRulesSpec (push_rules: {} on the wire) as "no opinion"
// like nil, so an empty ceiling doesn't get inherited wholesale and false-warn.
func clampPushRules(out *types.RunPolicySpec, ceiling types.RunPolicySpec, warns []string) []string {
	// deny_new_executables is not in IsSet (nothing enforces it yet) but a ceiling's true must
	// still reach the merged spec, so a proposal can never switch it off.
	if !ceiling.PushRules.IsSet() && (ceiling.PushRules == nil || !ceiling.PushRules.DenyNewExecutables) {
		return warns
	}
	if out.PushRules == nil {
		warns = append(warns, "push_rules inherited from the operator's policy")
		cp := *ceiling.PushRules
		cp.DenyPaths = append([]string(nil), ceiling.PushRules.DenyPaths...)
		cp.RequireReviewPaths = append([]string(nil), ceiling.PushRules.RequireReviewPaths...)
		out.PushRules = &cp
		return warns
	}
	merged := *out.PushRules
	if len(ceiling.PushRules.DenyPaths) > 0 {
		merged.DenyPaths = unionPaths(merged.DenyPaths, ceiling.PushRules.DenyPaths)
	}
	if len(ceiling.PushRules.RequireReviewPaths) > 0 {
		merged.RequireReviewPaths = unionPaths(merged.RequireReviewPaths, ceiling.PushRules.RequireReviewPaths)
	}
	if ceil := ceiling.PushRules.MaxInspectPackMiB; ceil > 0 && (merged.MaxInspectPackMiB <= 0 || merged.MaxInspectPackMiB > ceil) {
		warns = append(warns, fmt.Sprintf("push_rules.max_inspect_pack_mib capped to operator maximum %d", ceil))
		merged.MaxInspectPackMiB = ceil
	}
	if ceil := ceiling.PushRules.MaxFileSizeMiB; ceil > 0 && (merged.MaxFileSizeMiB <= 0 || merged.MaxFileSizeMiB > ceil) {
		warns = append(warns, fmt.Sprintf("push_rules.max_file_size_mib capped to operator maximum %d", ceil))
		merged.MaxFileSizeMiB = ceil
	}
	merged.DenyNewExecutables = merged.DenyNewExecutables || ceiling.PushRules.DenyNewExecutables
	if ceil := ceiling.PushRules.HoldSeconds; ceil > 0 && (merged.HoldSeconds <= 0 || merged.HoldSeconds > ceil) {
		merged.HoldSeconds = ceil // the shorter of two authored holds, silently: it widens nothing
	}
	out.PushRules = &merged
	return warns
}

// unionPaths is union's exact-string counterpart for deny_paths: union's case-folding would let a
// member's re-typed rule silently displace the ceiling's spelling, weakening it. Dedupe by exact byte string only.
func unionPaths(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, lists := range [][]string{a, b} {
		for _, s := range lists {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	sort.Strings(out)
	return out
}

// cloneProposal takes ownership of the caller's proposal: the returned spec shares no backing array
// or pointee with it, so a later mutation on either side can never move an already-enforced ceiling.
// A shallow `out := proposed` would alias the caller's slices and *ResourceLimits.
//
// Uses slices.Clone (not append([]T(nil),...)) to preserve nil vs non-nil-empty, and types.RunPolicySpec.Clone
// isn't used since it only copies GrantSpec.Scope/WorkspaceMount.ReadOnly one level deep.
func cloneProposal(s types.RunPolicySpec) types.RunPolicySpec {
	out := s
	out.AllowedDomains = slices.Clone(s.AllowedDomains)
	out.DeniedDomains = slices.Clone(s.DeniedDomains)
	out.AllowedMethods = slices.Clone(s.AllowedMethods)
	out.UIApps = slices.Clone(s.UIApps)
	out.ToolRules = slices.Clone(s.ToolRules)
	out.WorkspaceRepos = slices.Clone(s.WorkspaceRepos)
	out.WorkspaceMounts = slices.Clone(s.WorkspaceMounts)
	for i := range out.WorkspaceMounts {
		if ro := s.WorkspaceMounts[i].ReadOnly; ro != nil {
			v := *ro
			out.WorkspaceMounts[i].ReadOnly = &v
		}
	}
	out.EligibleGrants = slices.Clone(s.EligibleGrants)
	for i := range out.EligibleGrants {
		out.EligibleGrants[i].Scope = slices.Clone(s.EligibleGrants[i].Scope)
	}
	if s.Resources != nil {
		r := *s.Resources
		out.Resources = &r
	}
	if s.LLMInspection != nil {
		li := *s.LLMInspection
		li.WorkspaceSecretNames = slices.Clone(s.LLMInspection.WorkspaceSecretNames)
		li.WorkspaceSecretValues = slices.Clone(s.LLMInspection.WorkspaceSecretValues)
		li.ClassifiedMarkers = slices.Clone(s.LLMInspection.ClassifiedMarkers)
		out.LLMInspection = &li
	}
	out.AzureDevOpsCapabilities = slices.Clone(s.AzureDevOpsCapabilities)
	if s.PushRules != nil {
		pr := *s.PushRules
		pr.DenyPaths = slices.Clone(s.PushRules.DenyPaths)
		pr.RequireReviewPaths = slices.Clone(s.PushRules.RequireReviewPaths)
		out.PushRules = &pr
	}
	return out
}

// clampUIApps bounds a proposal's UI apps: the ceiling's list is an allowlist of (name, port) pairs
// when it sets one, no opinion otherwise. Unlike allowed_methods, an empty ceiling is never adopted —
// empty ui_apps means "no UI apps" (RunPolicySpec.UIApps' own doc), so adopting would grant relay
// access never asked for.
//
// Matched on name AND port, since name alone would let a proposal point the ceiling's app at any
// port in the sandbox; path is left as proposed.
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

// firstUseApprovalRank ranks FirstUseMode strictness (always_deny strictest, wait_for_review loosest);
// Normalize() first so empty/garbage ranks as always_deny, its fail-closed default.
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

// ClampRunConfinement raises a proposed run's confinement class to the policy floor (never lowers)
// so the composer can't emit a run that handleCreateRun would 422 for advertising a weaker class
// than its own inline_policy floor (invariant 5); empty/unknown ranks 0 and is also raised.
// WarnResourcesCapped is shared with dispatch's own ephemeral-disk clamp (runs_dispatch_ceiling.go)
// so the same fact about the same number isn't hand-typed twice and drift apart.
const WarnResourcesCapped = "resources capped to operator maximum"

// CapDiskMiB bounds a non-zero ephemeral-disk size by one operator ceiling; 0 on either side means
// "no bound". Shared by Clamp (preview) and api.ephemeralDiskFor (dispatch) so both ceilings agree —
// previously each applied a different ceiling (profile vs org), so a 100000 MiB policy under a 4096
// MiB org max previewed 100000 but ran on 4096; pinned by TestPreflightAndDispatchAgreeOnEphemeralDisk.
// A zero disk stays zero: a ceiling bounds a request, it never invents one.
func CapDiskMiB(disk, ceil int) int {
	if disk > 0 && ceil > 0 && ceil < disk {
		return ceil
	}
	return disk
}

// capToLimit folds a profile's CPU or memory maximum into a ceiling value: the lower of the two,
// where an unset ceiling stands at def (the deployment's size) rather than at "no bound". max <= 0
// leaves the ceiling alone.
func capToLimit(ceil, max, def int) int {
	if max <= 0 {
		return ceil
	}
	if ceil <= 0 {
		ceil = def
	}
	return min(ceil, max)
}

// CapResources bounds a run's CPU and memory by the profile's maximums and reports whether it
// changed anything. It is the resource-only half of Clamp, for the member launch that carries no
// policy: its spec is the profile's own ceiling, which Clamp must not process because it drops
// workspace mounts. A zero field means "the deployment's default size" (sizing.EffectiveLimits),
// so it is cut only when that default is itself above the maximum. Returns a FRESH block, never an
// in-place write, so one shared spec cannot re-size every later run that reads it.
func CapResources(r *types.ResourceLimits, limits types.GovernanceLimits) (*types.ResourceLimits, bool) {
	if limits.MaxCPUMillis <= 0 && limits.MaxMemoryMiB <= 0 {
		return r, false
	}
	var out types.ResourceLimits
	if r != nil {
		out = *r
	}
	eff := sizing.EffectiveLimits()
	cpu, mem := out.CPUMillis, out.MemoryMiB
	if limits.MaxCPUMillis > 0 {
		if cur := cmp.Or(cpu, int(eff.CPUMillis)); cur > limits.MaxCPUMillis {
			out.CPUMillis = limits.MaxCPUMillis
		}
	}
	if limits.MaxMemoryMiB > 0 {
		if cur := cmp.Or(mem, int(eff.MemoryMiB)); cur > limits.MaxMemoryMiB {
			out.MemoryMiB = limits.MaxMemoryMiB
		}
	}
	if out.CPUMillis == cpu && out.MemoryMiB == mem {
		return r, false
	}
	return &out, true
}

func ClampRunConfinement(runClass string, floor types.ConfinementClass) (string, string) {
	if fr := confinementRank(floor); fr > 0 && confinementRank(types.ConfinementClass(runClass)) < fr {
		return string(floor), fmt.Sprintf("run confinement raised from %q to policy floor %q", runClass, floor)
	}
	return runClass, ""
}

// normalizeClampTTL resolves a TTL to actual mint seconds: 0 and negative both mean "broker maximum",
// matching internal/api's normalizeGrantTTLSeconds so the two sides stay comparable — testing `==0`
// alone would let ttl_seconds=-1 pass a 300s ceiling that the write-time comparator refuses.
func normalizeClampTTL(ttl int) int {
	if ttl <= 0 || ttl > maxGrantTTLSeconds {
		return maxGrantTTLSeconds
	}
	return ttl
}

// CeilingGrantsCovering returns the ceiling grants whose identity covers g (same kind, and for a
// stored-secret kind the same host/secret/known_hosts pairing) — exported so api's write-time
// comparator selects from the same set the runtime clamp bounds against, one definition not two.
// Identity only, never bounds: an undecodable proposal scope is covered by nothing (fail closed).
func CeilingGrantsCovering(g types.GrantSpec, ceiling []types.GrantSpec) []types.GrantSpec {
	want, covered, ok := grantPairingOf(g)
	var out []types.GrantSpec
	for _, cg := range ceiling {
		if cg.Kind != g.Kind {
			continue
		}
		if !covered {
			out = append(out, cg)
			continue
		}
		if !ok {
			continue
		}
		got, cgCovered, cgOK := grantPairingOf(cg)
		if cgCovered && cgOK && samePairing(got, want) {
			out = append(out, cg)
		}
	}
	return out
}

// grantDominatedBy reports whether ceiling grant cg bounds g on every axis a clamp can narrow, the
// same three questions governanceGrantWithinCeiling asks — if one dominates, the clamp returns g unchanged.
func grantDominatedBy(g, cg types.GrantSpec) bool {
	if cg.RequiresApproval && !g.RequiresApproval {
		return false
	}
	if normalizeClampTTL(g.TTLSeconds) > normalizeClampTTL(cg.TTLSeconds) {
		return false
	}
	if g.Kind == types.GrantGitHubToken && GitHubScopeWithin(g.Scope, cg.Scope) != nil {
		return false
	}
	return true
}

// clampGrants narrows each proposed grant to the bound of the ceiling grant(s) that cover it, and
// drops any whose kind the ceiling doesn't carry.
//
// Selects covering grants by identity (CeilingGrantsCovering), never a kind-keyed map — the original
// defect let the LAST same-kind entry supply approval/TTL for a proposal naming the FIRST one's
// pairing. If one covering grant dominates on every remaining axis, bound against that one alone
// (matching the write-time comparator, so an already-permitted proposal passes through unchanged);
// otherwise meet across all candidates (min TTL, approval forced on if any requires it, github scope
// intersected) — order-independent, narrow-only.
//
// A proposal whose pairing no ceiling entry names falls back to the meet of every same-kind entry and
// is still kept — the pairing gate is filterUserGrants' job; the write-time comparator refuses such a
// grant outright, the one deliberate asymmetry between the two.
func clampGrants(grants []types.GrantSpec, ceiling types.RunPolicySpec, warns *[]string) []types.GrantSpec {
	if len(grants) == 0 {
		return grants
	}
	var out []types.GrantSpec
	for _, g := range grants {
		bounds := CeilingGrantsCovering(g, ceiling.EligibleGrants)
		if len(bounds) == 0 {
			bounds = ceilingGrantsBounding(g, ceiling.EligibleGrants)
		} else if i := slices.IndexFunc(bounds, func(cg types.GrantSpec) bool { return grantDominatedBy(g, cg) }); i >= 0 {
			// The comparator accepts g against this grant, so bounding here is a no-op (barring an unset TTL);
			// which dominating grant is picked can't matter since domination means g is already inside all of them.
			bounds = bounds[i : i+1]
		}
		if len(bounds) == 0 {
			*warns = append(*warns, fmt.Sprintf("dropped grant %q: not in operator's eligible grants", g.Kind))
			continue
		}
		// GitHub: intersect repos+permissions down to every bounding ceiling grant (sequential intersection
		// as meet); warnings deduped since the same removal seen against several entries is one fact.
		if g.Kind == types.GrantGitHubToken {
			var scopeWarns []string
			for _, cg := range bounds {
				g.Scope = clampGitHubScope(g.Scope, cg.Scope, &scopeWarns)
			}
			*warns = append(*warns, dedupeStrings(scopeWarns)...)
		}
		// TTL cap: the strictest bound; a ceiling TTL <=0 bounds nothing, leaving the broker maximum.
		max := maxGrantTTLSeconds
		for _, cg := range bounds {
			if cg.TTLSeconds > 0 && cg.TTLSeconds < max {
				max = cg.TTLSeconds
			}
		}
		if g.TTLSeconds <= 0 || g.TTLSeconds > max {
			if g.TTLSeconds > max {
				*warns = append(*warns, fmt.Sprintf("grant %q TTL capped to %ds", g.Kind, max))
			}
			g.TTLSeconds = max
		}
		// Approval only tightens: if any bounding ceiling grant requires it, force it on.
		if !g.RequiresApproval && slices.ContainsFunc(bounds,
			func(cg types.GrantSpec) bool { return cg.RequiresApproval }) {
			*warns = append(*warns, fmt.Sprintf("grant %q forced to require approval (operator policy)", g.Kind))
			g.RequiresApproval = true
		}
		// owner_only tightens the same way: a proposal can't drop a flag the operator set.
		if !g.OwnerOnly && slices.ContainsFunc(bounds, func(cg types.GrantSpec) bool { return cg.OwnerOnly }) {
			g.OwnerOnly = true
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

// clampGitHubScope intersects a proposed github scope's repos+permissions down to the ceiling's:
// permissions never raised above the ceiling's, repos absent from the ceiling dropped.
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
	// SECURITY (RBAC-bypass): an empty ceiling repo list is deny-all, not "any repo". A hand-authored
	// inline_policy has no grounding step, so skipping this intersection under the shipped default.json
	// ceiling ("repos": []) would let a member's arbitrary repo list survive verbatim.
	allowed := toSet(c.Repos)
	kept, dropped := partition(p.Repos, func(r string) bool { return allowed[strings.ToLower(strings.TrimSpace(r))] })
	if len(dropped) > 0 {
		*warns = append(*warns, fmt.Sprintf("github grant: dropped %d repo(s) outside operator scope", len(dropped)))
	}
	p.Repos = kept
	// Deny-by-default (M6): an absent/empty ceiling permission map grants none; every proposed permission is dropped, never raised.
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

// GitHubScopeWithin reports whether a proposed github_token scope stays inside a ceiling's, by the
// same rules clampGitHubScope enforces (ceiling repo list is an allowlist, empty = deny-all; an
// absent permission is ungrantable; a present one bounds the level by permRank) — one shared copy so
// accept-here can't drift from widen-there.
//
// Differs in one way: an undecodable scope is an error here, not empty, since a comparator reading
// "no constraint" would fail open (the clamp instead intersects toward empty, fail-closed).
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

// clampOperatorSwitches forces off boolean controls only an operator may widen: allow-all egress and
// git_push_any_branch. Without this a member could disable push confinement by posting the field.
// Split out of Clamp to keep its branch count under the gocyclo gate.
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

// toolStrictness orders the three effects so a clamp can take the stricter: allow < hold < deny.
func toolStrictness(e types.ToolEffect) int {
	switch e {
	case types.ToolDeny:
		return 2
	case types.ToolHold:
		return 1
	}
	return 0
}

// ceilingToolEffect mirrors proxy.Policy.ToolEffectFor's lookup order (exact rule, else "*" default,
// else hold) so the clamp and enforcement agree.
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

// clampToolRules keeps a member's inline tool_rules from widening the operator's: per tool the
// stricter effect wins, and the ceiling's own rules carry into the result so a tool the operator
// denies stays denied when the proposal is silent. Without this a member could post
// `{"tool":"*","effect":"allow"}` and turn a supervised run autonomous.
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
