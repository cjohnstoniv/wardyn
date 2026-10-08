// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"cmp"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/runner/sizing"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Leq reports whether policy a, under limits la, is at most as permissive as
// policy b under limits lb: every request, grant, size and duration a admits, b
// admits too.
//
// It is CONSERVATIVE. It reads values the way the runtime does (the same
// normalisers as the overlay meet: a zero that means a finite default is
// replaced by that default before two values are compared), and it answers false
// for anything it cannot prove narrower. A false negative costs an extra
// approval; a false positive is a silent widening, because GOV4 exempts a change
// Leq proves narrowing from second-person approval. So a field no rule below
// covers must not differ at all (see leq_fallback.go), and the coverage guard
// fails a field with no rule here.
//
// Each rule is the inverse of the meet's for the same field: ApplyOverlay(b, o)
// is at most as permissive as b, so Leq(ApplyOverlay(b, o), b) holds. eligible_grants
// are judged by GrantWithin, the one grant-dominance contract.
func Leq(a, b types.RunPolicySpec, la, lb types.GovernanceLimits) bool {
	return leqEgress(a, b) && leqPosture(a, b) && leqPush(a.PushRules, b.PushRules) &&
		leqLimits(la, lb) && leqRunLimits(la, lb) &&
		equalOutsideCeilingRules(a, b) && equalOutsideLimitRules(la, lb)
}

// notLooser reports whether bound a is at least as tight as bound b, where a
// value <= 0 is no bound.
func notLooser(a, b int) bool { return !looser(a, b) }

// leqSize compares two size fields where a zero means the deployment's default
// size def (0 where zero is unbounded).
func leqSize(a, b, def int) bool {
	norm := func(x int) int {
		if x <= 0 {
			return def
		}
		return x
	}
	return notLooser(norm(a), norm(b))
}

func leqEgress(a, b types.RunPolicySpec) bool {
	if a.AllowAllEgress && !b.AllowAllEgress {
		return false
	}
	if firstUseApprovalRank(a.FirstUseApproval) < firstUseApprovalRank(b.FirstUseApproval) ||
		normFirstUseHold(a.FirstUseHoldSeconds) > normFirstUseHold(b.FirstUseHoldSeconds) ||
		normMaxHolds(a.MaxHolds) > normMaxHolds(b.MaxHolds) {
		return false
	}
	return leqAllowedDomains(a.AllowedDomains, b.AllowedDomains) &&
		leqDeniedDomains(a.DeniedDomains, b.DeniedDomains) &&
		leqSubsetWhenSet(normMethods(a.AllowedMethods), normMethods(b.AllowedMethods))
}

// leqSubsetWhenSet is allowed_methods' shape: an empty b is the unrestricted
// reading, so anything is within it; otherwise a must name a non-empty subset of b.
func leqSubsetWhenSet(a, b []string) bool {
	if len(b) == 0 {
		return true
	}
	return len(a) > 0 && !slices.ContainsFunc(a, func(s string) bool { return !slices.Contains(b, s) })
}

// leqCapabilities is azure_devops_capabilities and github_capabilities: an empty b is the provider
// row's default profile, which a list cannot be proven within, so only an empty
// a is within it; otherwise a must name a non-empty subset of b.
func leqCapabilities(a, b []string) bool {
	if len(b) == 0 {
		return len(a) == 0
	}
	return leqSubsetWhenSet(a, b)
}

// leqAllowedDomains holds when every entry of a is covered by an entry of b,
// under the proxy's own matcher. An entry the matcher cannot read is unproven.
// A b with allow_all_egress never covers an entry (an exact entry also reaches
// private literal IPs and enrols credential injection), so b's own list is the
// only thing a is measured against.
func leqAllowedDomains(a, b []string) bool {
	var base []domEntry
	for _, s := range b {
		if e, ok := parseDomEntry(s); ok {
			base = append(base, e)
		}
	}
	for _, s := range a {
		e, ok := parseDomEntry(s)
		if !ok || !slices.ContainsFunc(base, func(f domEntry) bool { return f.covers(e) }) {
			return false
		}
	}
	return true
}

// leqDeniedDomains holds when every denial of b is still in force in a: some
// entry of a covers it. An unreadable b entry must reappear as typed.
func leqDeniedDomains(a, b []string) bool {
	var held []domEntry
	var raw []string
	for _, s := range a {
		if e, ok := parseDomEntry(s); ok {
			held = append(held, e)
		} else {
			raw = append(raw, strings.ToLower(strings.TrimSpace(s)))
		}
	}
	for _, s := range b {
		if f, ok := parseDomEntry(s); ok {
			if !slices.ContainsFunc(held, func(e domEntry) bool { return e.covers(f) }) {
				return false
			}
		} else if s = strings.ToLower(strings.TrimSpace(s)); s != "" && !slices.Contains(raw, s) {
			return false
		}
	}
	return true
}

func leqPosture(a, b types.RunPolicySpec) bool {
	if confinementRank(a.MinConfinementClass) < confinementRank(b.MinConfinementClass) ||
		!notLooser(a.AutoStopAfterSec, b.AutoStopAfterSec) ||
		(a.GitPushAnyBranch && !b.GitPushAnyBranch) ||
		!leqCapabilities(capStrings(a.AzureDevOpsCapabilities), capStrings(b.AzureDevOpsCapabilities)) ||
		!leqCapabilities(capStrings(a.GitHubCapabilities), capStrings(b.GitHubCapabilities)) {
		return false
	}
	for _, g := range a.EligibleGrants {
		if GrantWithin(g, b.EligibleGrants) != nil {
			return false
		}
	}
	return leqMounts(a.WorkspaceMounts, b.WorkspaceMounts) &&
		leqWorkspaceRepos(a.WorkspaceRepos, b.WorkspaceRepos) &&
		leqLLMInspection(a.LLMInspection, b.LLMInspection) &&
		leqUIApps(a.UIApps, b.UIApps) &&
		leqResources(a.Resources, b.Resources) &&
		leqToolRules(a.ToolRules, b.ToolRules)
}

// leqMounts holds when every mount of a is a (source, target) mount of b, and
// read-only wherever b's is.
func leqMounts(a, b []types.WorkspaceMount) bool {
	for _, m := range a {
		i := slices.IndexFunc(b, func(x types.WorkspaceMount) bool { return x.Source == m.Source && x.Target == m.Target })
		if i < 0 || (b[i].ReadOnlyOrDefault() && !m.ReadOnlyOrDefault()) {
			return false
		}
	}
	return true
}

func leqWorkspaceRepos(a, b []types.WorkspaceRepo) bool {
	return !slices.ContainsFunc(a, func(r types.WorkspaceRepo) bool { return !slices.Contains(b, r) })
}

// leqLLMInspection holds when a inspects wherever b does and does so in the
// same way: dropping an inspection b names is a loosening, and two differing
// inspections are not comparable.
func leqLLMInspection(a, b *types.LLMInspectionSpec) bool {
	switch {
	case b == nil:
		return true
	case a == nil:
		return false
	}
	return sameJSON(a, b)
}

// leqUIApps holds when every app of a is a (name, port) app of b serving the
// same path.
func leqUIApps(a, b []types.UIApp) bool {
	for _, app := range a {
		if !slices.ContainsFunc(b, func(x types.UIApp) bool {
			return x.Name == app.Name && x.Port == app.Port && x.PathOrRoot() == app.PathOrRoot()
		}) {
			return false
		}
	}
	return true
}

func leqResources(a, b *types.ResourceLimits) bool {
	var ra, rb types.ResourceLimits
	if a != nil {
		ra = *a
	}
	if b != nil {
		rb = *b
	}
	eff := sizing.EffectiveLimits()
	return leqSize(ra.CPUMillis, rb.CPUMillis, int(eff.CPUMillis)) &&
		leqSize(ra.MemoryMiB, rb.MemoryMiB, int(eff.MemoryMiB)) &&
		leqSize(ra.PidsLimit, rb.PidsLimit, int(eff.PidsLimit)) &&
		leqSize(ra.DiskMiB, rb.DiskMiB, 0)
}

// leqToolRules holds when, for every tool either side names and for the "*"
// default, a's effect is at least as strict as b's.
func leqToolRules(a, b []types.ToolRule) bool {
	ea, eb := toolEffects(a), toolEffects(b)
	check := func(tool string) bool {
		return toolStrictness(effectFor(ea, tool)) >= toolStrictness(effectFor(eb, tool))
	}
	if !check("*") {
		return false
	}
	for t := range ea {
		if !check(t) {
			return false
		}
	}
	for t := range eb {
		if !check(t) {
			return false
		}
	}
	return true
}

// leqPush compares push_rules. Path lists must keep every path of b (by exact
// spelling: a differently spelled path that happens to cover it is unproven),
// and each bound is compared after the broker's own default is applied.
func leqPush(a, b *types.PushRulesSpec) bool {
	var pa, pb types.PushRulesSpec
	if a != nil {
		pa = *a
	}
	if b != nil {
		pb = *b
	}
	contains := func(have, want []string) bool {
		return !slices.ContainsFunc(want, func(p string) bool { return !slices.Contains(have, p) })
	}
	packCap := func(p *types.PushRulesSpec) int { // 0 = no inspection, so no cap
		switch {
		case !p.IsSet():
			return 0
		case p.MaxInspectPackMiB > 0:
			return p.MaxInspectPackMiB
		}
		return defaultInspectPackMiB
	}
	return contains(pa.DenyPaths, pb.DenyPaths) &&
		contains(pa.RequireReviewPaths, pb.RequireReviewPaths) &&
		(pa.DenyNewExecutables || !pb.DenyNewExecutables) &&
		notLooser(pa.MaxFileSizeMiB, pb.MaxFileSizeMiB) &&
		normPushHold(pa.HoldSeconds) <= normPushHold(pb.HoldSeconds) &&
		notLooser(packCap(&pa), packCap(&pb))
}

func leqLimits(a, b types.GovernanceLimits) bool {
	for _, p := range [][2]bool{
		{a.DenyTaskModeExec, b.DenyTaskModeExec}, {a.DenyInteractive, b.DenyInteractive},
		{a.DenyUIApps, b.DenyUIApps}, {a.DenyUserDrive, b.DenyUserDrive},
	} {
		if p[1] && !p[0] { // a denial the base keeps must stay
			return false
		}
	}
	for _, p := range [][2]int{
		{a.MaxConcurrentRuns, b.MaxConcurrentRuns}, {a.MaxCPUMillis, b.MaxCPUMillis},
		{a.MaxMemoryMiB, b.MaxMemoryMiB}, {a.MaxEphemeralDiskMiB, b.MaxEphemeralDiskMiB},
		{a.MaxDriveSizeMiB, b.MaxDriveSizeMiB},
	} {
		if !notLooser(p[0], p[1]) {
			return false
		}
	}
	return leqRubric(a.AutonomyRubric, b.AutonomyRubric)
}

// leqRubric holds when a caps every posture b caps, at the same level or lower.
func leqRubric(a, b *types.AutonomyRubric) bool {
	levels := func(r *types.AutonomyRubric) []types.AutonomyLevel {
		if r == nil {
			return make([]types.AutonomyLevel, 9)
		}
		return []types.AutonomyLevel{
			r.EgressOpen, r.EgressReviewed, r.EgressSealed,
			r.SecretsPowerful, r.SecretsBaseline, r.SecretsNone,
			r.ConfinementCC1, r.ConfinementCC2, r.ConfinementCC3,
		}
	}
	la, lb := levels(a), levels(b)
	for i := range la {
		switch {
		case lb[i] == "": // b caps nothing here
		case la[i] == "" || la[i].Rank() > lb[i].Rank():
			return false
		}
	}
	return true
}

// leqRunLimits compares the run-lifetime bounds the way TightenRunLimits reads
// them. A default is compared as the maximum when it is 0.
func leqRunLimits(a, b types.GovernanceLimits) bool {
	return notLooser(a.MaxEndAheadSec, b.MaxEndAheadSec) &&
		notLooser(a.MaxWaitSec, b.MaxWaitSec) &&
		notLooser(a.PauseIdleAfterSec, b.PauseIdleAfterSec) &&
		(!a.AllowNoEnd || b.AllowNoEnd) &&
		(!a.UserChangesLimits || b.UserChangesLimits) &&
		notLooser(cmp.Or(a.DefaultEndSec, a.MaxEndAheadSec), cmp.Or(b.DefaultEndSec, b.MaxEndAheadSec)) &&
		notLooser(cmp.Or(a.DefaultWaitSec, a.MaxWaitSec), cmp.Or(b.DefaultWaitSec, b.MaxWaitSec))
}
