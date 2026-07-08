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
// regardless of what the (untrusted-input-driven) analyzer proposed.
//
// Clamps applied:
//   - confinement raised to the operator's minimum class if the proposal is weaker;
//   - allow_all_egress forced off unless the ceiling permits it;
//   - allowed_domains intersected down to the ceiling's allowlist (unless the
//     ceiling itself allows all egress);
//   - the ceiling's denied_domains unioned in (deny always wins);
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

	// Grants: drop unknown kinds, intersect github perms, cap TTL, force approval.
	out.EligibleGrants = clampGrants(out.EligibleGrants, ceiling, &warns)

	// Workspace mounts: NEVER composer-introduced. Operators author mounts on a
	// stored policy; an analyzer fed untrusted input must not be able to mount a
	// host path. Drop any the proposal carried.
	if len(out.WorkspaceMounts) > 0 {
		warns = append(warns, fmt.Sprintf("dropped %d proposed workspace mount(s): host mounts are operator-authored, never composer-proposed", len(out.WorkspaceMounts)))
		out.WorkspaceMounts = nil
	}

	return out, warns
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
	// Repos: intersect to the ceiling if the ceiling lists any.
	if len(c.Repos) > 0 {
		allowed := toSet(c.Repos)
		kept, dropped := partition(p.Repos, func(r string) bool { return allowed[strings.ToLower(strings.TrimSpace(r))] })
		if len(dropped) > 0 {
			*warns = append(*warns, fmt.Sprintf("github grant: dropped %d repo(s) outside operator scope", len(dropped)))
		}
		p.Repos = kept
	}
	// Permissions: drop any not allowed by the ceiling; never exceed the ceiling level.
	if c.Permissions != nil {
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
