// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Ceiling helpers beside governance.go's CRUD and resolver: the ADVISORY
// omission warnings a profile write returns (what the DEPLOYMENT ceiling
// carries that the profile does not), and the per-request memo that makes every
// ceiling-bounded seam in one request answer from a single resolve.
//
// Its own file by seam (the file-size gate); no behaviour lives here that
// governance.go's own doc does not describe.
package api

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// omission warnings

// governanceOmissionWarnings names what the DEPLOYMENT ceiling carries that
// this profile does not.
//
// A governance profile REPLACES Config.DefaultPolicy for the principals it binds
// (composing through composer.Clamp is not a lattice meet), so it narrows and
// widens BY OMISSION, silently: forgetting the deployment's denied_domains
// un-walls every host that list protected. ADVISORY ONLY, never a refusal: both
// directions are legitimate, and refusing would make DefaultPolicy a floor this
// feature deliberately does not have. Six checks, NOT exhaustive over
// RunPolicySpec: only fields where an omission changes what a member can reach.
func governanceOmissionWarnings(ceiling, deployment types.RunPolicySpec, limits types.GovernanceLimits) []string {
	var warns []string
	if missing := missingEntries(deployment.DeniedDomains, ceiling.DeniedDomains); len(missing) > 0 {
		warns = append(warns, fmt.Sprintf(
			"this profile omits %d denied domain(s) the deployment default denies (%s) — "+
				"members under it are NOT walled from them",
			len(missing), strings.Join(missing, ", ")))
	}
	if missing := missingEntries(deployment.AllowedDomains, ceiling.AllowedDomains); len(missing) > 0 {
		warns = append(warns, fmt.Sprintf(
			"this profile omits %d allowed domain(s) the deployment default allows (%s) — "+
				"members under it lose access to them",
			len(missing), strings.Join(missing, ", ")))
	}
	if missing := missingGrantKinds(deployment.EligibleGrants, ceiling.EligibleGrants); len(missing) > 0 {
		warns = append(warns, fmt.Sprintf(
			"this profile omits %d eligible grant kind(s) the deployment default carries (%s) — "+
				"members under it cannot request those credentials",
			len(missing), strings.Join(missing, ", ")))
	}
	if ceiling.MinConfinementClass.Rank() < deployment.MinConfinementClass.Rank() {
		warns = append(warns, fmt.Sprintf(
			"this profile's min_confinement_class %q is WEAKER than the deployment default's %q",
			ceiling.MinConfinementClass, deployment.MinConfinementClass))
	}
	if ceiling.AllowAllEgress && !deployment.AllowAllEgress {
		warns = append(warns,
			"this profile sets allow_all_egress while the deployment default does not — "+
				"members under it reach any non-denied public host")
	}
	if w := omitResourcesWarning(ceiling.Resources, deployment.Resources, limits); w != "" {
		warns = append(warns, w)
	}
	return warns
}

// omitResourcesWarning says what a profile that leaves out its sandbox CPU or
// memory (no resources block, or a zero field) hands its members instead: the
// deployment's value, DefaultPolicy.Resources else runner.EffectiveLimits(),
// the same order inheritDeploymentResources fills it in. Only the missing
// field is named; both missing reads "sandbox size". When the profile's own
// max_cpu_millis / max_memory_mib cut the inherited value, the sentence says
// so. Empty when the profile sets both fields. Wording is mock packet M10's.
func omitResourcesWarning(own, deployment *types.ResourceLimits, limits types.GovernanceLimits) string {
	var ownCPU, ownMem, depCPU, depMem int
	if own != nil {
		ownCPU, ownMem = own.CPUMillis, own.MemoryMiB
	}
	if deployment != nil {
		depCPU, depMem = deployment.CPUMillis, deployment.MemoryMiB
	}
	eff := runner.EffectiveLimits()
	cpu := cmp.Or(depCPU, int(eff.CPUMillis))
	mem := cmp.Or(depMem, int(eff.MemoryMiB))
	var what, got, capped []string
	if ownCPU == 0 {
		what = append(what, "sandbox CPU")
		got = append(got, fmt.Sprintf("%dm CPU", cpu))
		if limits.MaxCPUMillis > 0 && limits.MaxCPUMillis < cpu {
			capped = append(capped, fmt.Sprintf("%dm CPU", limits.MaxCPUMillis))
		}
	}
	if ownMem == 0 {
		what = append(what, "sandbox memory")
		got = append(got, fmt.Sprintf("%d MiB memory", mem))
		if limits.MaxMemoryMiB > 0 && limits.MaxMemoryMiB < mem {
			capped = append(capped, fmt.Sprintf("%d MiB memory", limits.MaxMemoryMiB))
		}
	}
	if len(what) == 0 {
		return ""
	}
	if len(what) == 2 {
		what = []string{"sandbox size"}
	}
	out := fmt.Sprintf("this profile sets no %s — runs under it get the deployment's %s",
		what[0], strings.Join(got, " and "))
	if len(capped) > 0 {
		out += fmt.Sprintf(", capped at %s by this profile's limits", strings.Join(capped, " and "))
	}
	return out
}

// missingEntries returns the entries of have that want does not carry, compared
// case-insensitively (host lists are matched that way everywhere else). Order
// follows have so the message is stable across calls.
func missingEntries(have, want []string) []string {
	if len(have) == 0 {
		return nil
	}
	present := make(map[string]bool, len(want))
	for _, v := range want {
		present[strings.ToLower(strings.TrimSpace(v))] = true
	}
	var missing []string
	for _, v := range have {
		if key := strings.ToLower(strings.TrimSpace(v)); key != "" && !present[key] {
			missing = append(missing, v)
		}
	}
	return missing
}

// missingGrantKinds returns the grant KINDS the deployment carries that the
// profile does not. Kinds, not whole GrantSpecs: a profile narrowing one
// pairing of a kind it still carries is the ordinary case and needs no warning,
// while dropping a kind entirely removes a whole credential lane.
func missingGrantKinds(have, want []types.GrantSpec) []string {
	var kinds []string
	for _, g := range have {
		if slices.ContainsFunc(want, func(w types.GrantSpec) bool { return w.Kind == g.Kind }) {
			continue
		}
		if !slices.Contains(kinds, string(g.Kind)) {
			kinds = append(kinds, string(g.Kind))
		}
	}
	return kinds
}

// the per-request ceiling memo

// ceilingMemoKey carries the per-request ceiling memo. A pointer holder rather
// than the value itself, because context.WithValue cannot be written to after
// the fact and the memo has to be FILLED by whichever site asks first.
type ceilingMemoKey struct{}

// ceilingMemo is one request's resolved ceiling, resolved at most once.
//
// It stores the ERROR too, deliberately: a resolver failure must be answered
// identically by every site in the request. Re-resolving after a failure could
// SUCCEED on the retry and hand a later site a ceiling the earlier one refused
// on — which is the disagreement this exists to remove, in its most dangerous
// direction (a refusal followed by a pass).
//
// Guarded by a mutex because a single request can fan out (dispatch runs inline
// but on a WithoutCancel copy that shares these values), and a memo that raced
// would reintroduce exactly the divergence it removes.
type ceilingMemo struct {
	mu    sync.Mutex
	done  bool
	value governanceCeiling
	err   error
}

// do is the memo's whole contract: resolve at most once, and hand every caller
// that one answer.
//
// Single-flight, with the lock held across the resolve, deliberately: releasing
// it between check and fill lets two callers both resolve and get DIFFERENT
// profiles, so a profile edit mid-request could clamp a run's egress under one
// ceiling and filter its grants under another. A waiter's own context is not
// consulted: the answer is about the PRINCIPAL, not the waiter's deadline. Safe
// because the scope is one request — the memo lives on the request context
// (ceilingMemoKey, installed by the auth middleware), a request fans out to at
// most a couple of goroutines, resolveEffectiveCeiling never re-enters this
// method, and a memoized failure dies with the request.
func (m *ceilingMemo) do(ctx context.Context, resolve func(context.Context) (governanceCeiling, error)) (governanceCeiling, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.done {
		return m.value, m.err
	}
	m.value, m.err = resolve(ctx)
	m.done = true
	return m.value, m.err
}

// withCeilingMemo installs an empty memo. Called once per authenticated request
// by the auth middleware, which is the only place that sees every routed call
// exactly once.
func withCeilingMemo(ctx context.Context) context.Context {
	return context.WithValue(ctx, ceilingMemoKey{}, &ceilingMemo{})
}

// ceilingMemoFromContext returns the request's memo, or nil for a caller that
// has none (a background job, or a unit test driving a resolver directly). A nil
// memo means "resolve normally", so absence is the pre-memo behaviour rather
// than a failure.
func ceilingMemoFromContext(ctx context.Context) *ceilingMemo {
	m, _ := ctx.Value(ceilingMemoKey{}).(*ceilingMemo)
	return m
}
