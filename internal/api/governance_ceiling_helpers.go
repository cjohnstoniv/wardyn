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

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// omission warnings

// governanceOmissionWarnings names what the DEPLOYMENT ceiling carries that
// this profile does not.
//
// A STANDALONE governance profile replaces Config.DefaultPolicy for the principals it binds
// (composing through composer.Clamp is not a lattice meet), so it narrows and
// widens BY OMISSION, silently: forgetting the deployment's denied_domains
// un-walls every host that list protected. A composed profile inherits instead, so the write
// door does not run this for it. ADVISORY ONLY, never a refusal: both
// directions are legitimate, and refusing would make DefaultPolicy a floor this
// feature deliberately does not have. Twelve checks, NOT exhaustive over
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
	return append(warns, governanceControlWarnings(ceiling, deployment, limits)...)
}

// governanceControlWarnings is the second half of governanceOmissionWarnings: the controls whose
// omission or weaker value on a standalone profile changes what a member can do, not what a member
// can reach. Only a standalone root can leave these out, so the write door never runs it for a
// composed profile (an overlay cannot turn local_self_defined_components on, and every other row
// inherits).
//
// local_self_defined_components is the one GovernanceLimits zero value that means deny. The rest
// compare the profile with the deployment default: desktop directions the default blocks, a
// recording level below the default's (or none, which drops the minimum), tool rules and an idle auto-stop the default carries,
// and a weaker llm_inspection mode.
func governanceControlWarnings(ceiling, deployment types.RunPolicySpec, limits types.GovernanceLimits) []string {
	var warns []string
	if !limits.LocalSelfDefinedComponents {
		warns = append(warns,
			"this profile leaves limits.local_self_defined_components off — members under it cannot start "+
				"a local run with a component they define themselves. Leaving it off is fine if that is intended; "+
				"to allow such runs, set it to true in this standalone profile's own limits (an overlay cannot turn it on)")
	}
	own, dep := ceiling.Desktop.Effective(), deployment.Desktop.Effective()
	var opened []string
	for _, d := range []struct {
		name     string
		own, dep types.TransferRule
	}{
		{"paste_in", own.PasteIn, dep.PasteIn}, {"copy_out", own.CopyOut, dep.CopyOut},
		{"upload", own.Upload, dep.Upload}, {"download", own.Download, dep.Download},
	} {
		if types.TransferStrictness(d.own) < types.TransferStrictness(d.dep) {
			opened = append(opened, d.name)
		}
	}
	if len(opened) > 0 {
		warns = append(warns, fmt.Sprintf(
			"this profile's desktop rules allow %s, which the deployment default blocks — "+
				"members under it can move data across the desktop's edge in that direction",
			strings.Join(opened, ", ")))
	}
	switch {
	case ceiling.Recording == "" && deployment.Recording.Rank() > types.RecordingOff.Rank():
		warns = append(warns, fmt.Sprintf(
			"this profile sets no recording level while the deployment default's is %q — it sets no minimum, "+
				"so a run under it can be recorded at a lower level", deployment.Recording))
	case ceiling.Recording != "" && ceiling.Recording.Rank() < deployment.Recording.Rank():
		warns = append(warns, fmt.Sprintf(
			"this profile's recording level %q is LOWER than the deployment default's %q",
			ceiling.Recording, deployment.Recording))
	}
	var dropped []string
	for _, rule := range deployment.ToolRules {
		if !slices.ContainsFunc(ceiling.ToolRules, func(r types.ToolRule) bool { return r.Tool == rule.Tool }) &&
			!slices.Contains(dropped, rule.Tool) {
			dropped = append(dropped, rule.Tool)
		}
	}
	if len(dropped) > 0 {
		warns = append(warns, fmt.Sprintf(
			"this profile omits %d tool rule(s) the deployment default carries (%s) — "+
				"members under it are not held to them",
			len(dropped), strings.Join(dropped, ", ")))
	}
	if deployment.AutoStopAfterSec > 0 && ceiling.AutoStopAfterSec <= 0 {
		warns = append(warns, fmt.Sprintf(
			"this profile sets no auto_stop_after_sec while the deployment default stops idle sandboxes after %d s — "+
				"this profile sets no idle cap, so a run stops only if its own policy or pool says so",
			deployment.AutoStopAfterSec))
	}
	inspection := func(spec *types.LLMInspectionSpec) (mode string, rank int) {
		switch {
		case spec != nil && spec.Mode == "block":
			return "block", 2
		case spec != nil && spec.Mode == "alert":
			return "alert", 1
		}
		return "off", 0
	}
	ownMode, ownRank := inspection(ceiling.LLMInspection)
	if depMode, depRank := inspection(deployment.LLMInspection); ownRank < depRank {
		warns = append(warns, fmt.Sprintf(
			"this profile's llm_inspection mode %q is WEAKER than the deployment default's %q", ownMode, depMode))
	}
	return warns
}

// governanceADOCeilingWarnings says which Azure DevOps provider rows a profile's explicit
// azure_devops_capabilities list reaches past: dispatch refuses a run naming a capability outside its
// row's capability_ceiling, and nothing else tells the person who wrote the list. res is the profile's
// EFFECTIVE content, so a composed profile's meet is what is compared. One warning per row; ADVISORY
// only, since rows are per organisation and editable. A site config that cannot be read, or a profile
// with no list, says nothing. Only the Azure DevOps list is compared: the GitHub twin is inert.
func (s *Server) governanceADOCeilingWarnings(ctx context.Context, res *ResolvedUnit) []string {
	if res == nil || len(res.Ceiling.AzureDevOpsCapabilities) == 0 || s.cfg.Store == nil {
		return nil
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return nil
	}
	var warns []string
	for _, row := range gitProviderRows(sc) {
		if row.Kind != types.GitProviderAzureDevOps || row.Entra == nil {
			continue
		}
		outside := adoCapsOutside(res.Ceiling.AzureDevOpsCapabilities, row.Entra.Ceiling())
		if len(outside) == 0 {
			continue
		}
		labels := make([]string, len(outside))
		for i, c := range outside {
			labels[i] = adoscope.ShortLabel(c)
		}
		warns = append(warns, fmt.Sprintf(
			"this profile lists %d Azure DevOps capability(ies) above the capability_ceiling of provider row %q (%s) — "+
				"a member who launches with one of them is refused; the row allows %d",
			len(outside), row.ID, strings.Join(labels, ", "), len(row.Entra.Ceiling())))
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

// ceilingMemo is one request's resolved ceiling, resolved at most once per context choice
// (contextChoiceFrom): two choices in one request are two resolves, each answered once.
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
	mu      sync.Mutex
	answers map[string]ceilingAnswer
}

// ceilingAnswer is one resolve's result, error included.
type ceilingAnswer struct {
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
	key := contextChoiceFrom(ctx).key()
	m.mu.Lock()
	defer m.mu.Unlock()
	if a, ok := m.answers[key]; ok {
		return a.value, a.err
	}
	var a ceilingAnswer
	a.value, a.err = resolve(ctx)
	if m.answers == nil {
		m.answers = map[string]ceilingAnswer{}
	}
	m.answers[key] = a
	return a.value, a.err
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
