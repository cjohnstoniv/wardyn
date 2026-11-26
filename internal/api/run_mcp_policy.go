// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The policy an MCP server's own sandbox runs under, one of three modes. "same" is the run's egress, plus
// the server's own hosts, less every host the agent reaches only through a broker; "narrower" is "same"
// met with the server's overlay; "own" is the server's authored spec, clamped to the creator's ceiling as
// an inline run policy is. Every mode ends in one finish (mcpPolicyFinish), so no MCP policy holds a host
// rule with effect hold and none is wider than the run's outer layers allow.

// The size of an MCP sandbox when nothing sets one: 0.5 CPU and 512 MiB, each capped by the creator's
// per-sandbox caps, which clamp and never refuse.
const (
	mcpSandboxDefaultCPUMillis = 500
	mcpSandboxDefaultMemoryMiB = 512
)

// mcpPolicyBase is what a run's MCP sandbox policies are derived from.
type mcpPolicyBase struct {
	// run is P_run: the run's policy after its seal at dispatch and revive, the folded policy at Review.
	run   types.RunPolicySpec
	comps runComponents
	// layers are the run's outer layers (outerLayers), nil at Review; deny is the ceiling's own deny list
	// (dispatchCeiling.deny), nil at Review.
	layers []sealLayer
	deny   []string
	// ceiling is the creator's ceiling: "own" is clamped to it, and its limits cap every sandbox's size.
	// ungoverned says the creator's runs are not clamped (runUngoverned).
	ceiling    governanceCeiling
	ungoverned bool
}

// mcpPolicy is one server's MCP sandbox policy.
type mcpPolicy struct {
	mode string
	spec types.RunPolicySpec
	// holds is how many host rules with effect hold the finish made deny; warns is what the narrower meet
	// or the own clamp dropped rather than widen.
	holds int
	warns []string
}

// mcpPolicyModeOf is a definition's sandbox policy mode, same when it names none.
func mcpPolicyModeOf(def types.ComponentDefinition) string {
	if def.MCP == nil || def.MCP.Sandbox == nil || def.MCP.Sandbox.Policy.Mode == "" {
		return types.ComponentMCPSandboxSame
	}
	return def.MCP.Sandbox.Policy.Mode
}

// mcpBrokeredHosts is every host the agent reaches only through the MCP broker: each attached remote MCP
// server's host, bare, so a deny of it covers every port.
func mcpBrokeredHosts(comps runComponents) []string {
	var out []string
	for _, a := range comps.attached {
		if !a.snapshot.Definition.RemoteMCP() {
			continue
		}
		if d, err := types.MCPURLDestination(a.snapshot.Definition.MCP.URL); err == nil && d.Host != "" {
			out = append(out, d.Host)
		}
	}
	return out
}

// mcpSamePolicy is "same" for server i: a fresh copy of run's egress knobs (allowed and denied domains,
// open egress, IP literals, methods, host rules, the first-use posture and the hold cap) with its class and
// lifetime, the server's own hosts added and the brokered hosts denied. The git broker's hosts are already
// denies in a dispatched run's policy (confineGitBrokerEgress), so the copy carries them. Every other knob
// is empty: there is no harness, terminal, desktop, repo or credential of the run's inside.
func mcpSamePolicy(run types.RunPolicySpec, comps runComponents, i int) types.RunPolicySpec {
	p := types.RunPolicySpec{
		AllowedDomains: slices.Clone(run.AllowedDomains), DeniedDomains: slices.Clone(run.DeniedDomains),
		AllowAllEgress: run.AllowAllEgress, RefuseIPLiterals: run.RefuseIPLiterals,
		FirstUseApproval: run.FirstUseApproval, FirstUseHoldSeconds: run.FirstUseHoldSeconds, MaxHolds: run.MaxHolds,
		AllowedMethods: slices.Clone(run.AllowedMethods), HostRules: types.CloneHostRuleSets(run.HostRules),
		MinConfinementClass: run.MinConfinementClass, AutoStopAfterSec: run.AutoStopAfterSec,
	}
	unionAllowedDomains(&p, comps.attached[i].snapshot.Definition.Hosts)
	unionDeniedDomains(&p, mcpBrokeredHosts(comps))
	return p
}

// mcpNarrowerPolicy is "narrower": same met with the server's overlay, the resolve-time reading, which drops
// what same no longer permits rather than widen, with a warning for each drop. An overlay no meet can
// satisfy leaves the server no egress at all, with a warning: dispatch never fails a run on it.
func mcpNarrowerPolicy(same types.RunPolicySpec, ov *types.CeilingOverlay) (types.RunPolicySpec, []string) {
	if ov == nil {
		return same, nil
	}
	met, warns, err := composer.ApplyOverlay(composer.Authority{Ceiling: same}, composer.Overlay{Ceiling: *ov})
	if err != nil {
		same.AllowedDomains, same.AllowAllEgress = []string{}, false
		return same, []string{"mcp.sandbox.policy.overlay: " + err.Error() + "; the server's sandbox reaches no host"}
	}
	return met.Ceiling, warns
}

// mcpOwnPolicy is "own": the server's authored spec, clamped to the creator's ceiling as an inline run policy
// is (composer.ClampMember, then the egress_host door, narrowUserInlinePolicy, which reads the caller on ctx),
// unless the creator's runs are not clamped. The knobs a sandbox never holds go first (mcpOnlyKnobs), so the
// door reads no workspace and, with no grant, no secret owner: the server's own secrets reach its sandbox by
// name.
func (s *Server) mcpOwnPolicy(ctx context.Context, b mcpPolicyBase, authored types.RunPolicySpec) (types.RunPolicySpec, []string, error) {
	authored = mcpOnlyKnobs(authored, b.run)
	if b.ungoverned {
		return authored, nil, nil
	}
	domains, warns := dropUncoveredPrivateLiterals(authored.AllowedDomains, b.ceiling.Spec)
	authored.AllowedDomains = domains
	clamped, clampWarns := composer.ClampMember(authored, b.ceiling.Spec, b.ceiling.Limits, b.ceiling.authoredSize(), b.ceiling.memberSize())
	capWarns, _, err := s.narrowUserInlinePolicy(ctx, "", &clamped)
	if err != nil {
		return types.RunPolicySpec{}, nil, err
	}
	return clamped, append(append(warns, clampWarns...), capWarns...), nil
}

// mcpPolicyFinish is every mode's last step, in this order: the seal against the run's outer layers (the last
// step that can narrow what a widening step added), the ceiling's own denies (reassertCeilingDenies' union;
// the server's own secrets are withheld against the same list where dispatch assembles them), the deny list
// spelled and ordered as the seal's meet spells it, every host rule with effect hold made deny (copied from
// the run, appended by the clamp from its ceiling, or carried in by a layer through the seal), then one
// types.AppendHostRuleSets over the result, so two sets the rewrite made equal collapse into one. Run again
// on its own output it changes nothing, digest included. A hold would make the MCP proxy raise a host rule
// approval, which its token may not raise, so it is denied outright instead. It answers how many hold rules
// it rewrote.
func mcpPolicyFinish(spec *types.RunPolicySpec, layers []sealLayer, deny []string) int {
	sealPolicy(spec, layers)
	unionCeilingDenies(spec, deny)
	spec.DeniedDomains = mcpCanonicalDenies(spec.DeniedDomains)
	spec.HostRules = types.CloneHostRuleSets(spec.HostRules)
	n := 0
	for i := range spec.HostRules {
		for j := range spec.HostRules[i].Rules {
			if r := &spec.HostRules[i].Rules[j]; r.Effect == types.HostRuleHold {
				r.Effect = types.HostRuleDeny
				n++
			}
		}
	}
	spec.HostRules = types.AppendHostRuleSets(spec.HostRules)
	return n
}

// mcpCanonicalDenies is deny through the meet's own deny union with nothing added: each entry spelled as
// the matcher reads it, sorted, each once. Without it a second seal would reorder the ceiling's denies the
// first one appended.
func mcpCanonicalDenies(deny []string) []string {
	met, _, err := composer.ApplyOverlay(composer.Authority{Ceiling: types.RunPolicySpec{DeniedDomains: deny}},
		composer.Overlay{Ceiling: types.CeilingOverlay{DeniedDomains: &[]string{}}})
	if err != nil {
		return deny
	}
	return met.Ceiling.DeniedDomains
}

// mcpSandboxResources is the size of a server's sandbox: what "own" authored, else the default, each field at
// most bound's ("own" passes its clamp's size, so the creator's ceiling bounds CPU, memory and pids; the other
// modes pass nil and author nothing) and capped by the creator's per-sandbox caps. Its disk is bound's, which
// the clamp already sized against the ceiling, capped by MaxEphemeralDiskMiB as a dispatch caps it.
func mcpSandboxResources(authored, bound *types.ResourceLimits, limits types.GovernanceLimits) *types.ResourceLimits {
	r := types.ResourceLimits{CPUMillis: mcpSandboxDefaultCPUMillis, MemoryMiB: mcpSandboxDefaultMemoryMiB}
	if authored != nil {
		if authored.CPUMillis > 0 {
			r.CPUMillis = authored.CPUMillis
		}
		if authored.MemoryMiB > 0 {
			r.MemoryMiB = authored.MemoryMiB
		}
		r.PidsLimit = authored.PidsLimit
	}
	if bound != nil {
		atMost := func(v, b int) int { // an unset v (0, the runner's default) takes b
			if b > 0 && (v <= 0 || v > b) {
				return b
			}
			return v
		}
		r.CPUMillis = atMost(r.CPUMillis, bound.CPUMillis)
		r.MemoryMiB = atMost(r.MemoryMiB, bound.MemoryMiB)
		r.PidsLimit = atMost(r.PidsLimit, bound.PidsLimit)
		r.DiskMiB = bound.DiskMiB
	}
	if limits.MaxCPUMillis > 0 {
		r.CPUMillis = min(r.CPUMillis, limits.MaxCPUMillis)
	}
	if limits.MaxMemoryMiB > 0 {
		r.MemoryMiB = min(r.MemoryMiB, limits.MaxMemoryMiB)
	}
	r.DiskMiB = composer.CapDiskMiB(r.DiskMiB, limits.MaxEphemeralDiskMiB)
	return &r
}

// mcpOnlyKnobs is spec with every knob an MCP sandbox never holds emptied (mounts, repos, inspection, UI apps,
// tool and push rules, any-branch pushes, the Azure DevOps and GitHub capabilities, the inner walls, the
// desktop, the recording and grants), its class at least the run's and its lifetime never longer than the run's.
func mcpOnlyKnobs(spec types.RunPolicySpec, run types.RunPolicySpec) types.RunPolicySpec {
	out := types.RunPolicySpec{
		AllowedDomains: spec.AllowedDomains, DeniedDomains: spec.DeniedDomains, AllowAllEgress: spec.AllowAllEgress,
		RefuseIPLiterals: spec.RefuseIPLiterals, FirstUseApproval: spec.FirstUseApproval,
		FirstUseHoldSeconds: spec.FirstUseHoldSeconds, MaxHolds: spec.MaxHolds, AllowedMethods: spec.AllowedMethods,
		HostRules: spec.HostRules, MinConfinementClass: spec.MinConfinementClass, AutoStopAfterSec: spec.AutoStopAfterSec,
		Resources: spec.Resources, DataOut: spec.DataOut,
	}
	if out.MinConfinementClass.Rank() < run.MinConfinementClass.Rank() {
		out.MinConfinementClass = run.MinConfinementClass
	}
	if run.AutoStopAfterSec > 0 && (out.AutoStopAfterSec <= 0 || out.AutoStopAfterSec > run.AutoStopAfterSec) {
		out.AutoStopAfterSec = run.AutoStopAfterSec
	}
	return out
}

// mcpSandboxPolicy derives server i's MCP sandbox policy from b, in its mode, and ends it in the finish.
func (s *Server) mcpSandboxPolicy(ctx context.Context, b mcpPolicyBase, i int) (mcpPolicy, error) {
	def := b.comps.attached[i].snapshot.Definition
	out := mcpPolicy{mode: mcpPolicyModeOf(def)}
	var authored, bound *types.ResourceLimits
	switch out.mode {
	case types.ComponentMCPSandboxNarrower: // an overlay's size is not read: narrower takes the default, capped
		out.spec, out.warns = mcpNarrowerPolicy(mcpSamePolicy(b.run, b.comps, i), def.MCP.Sandbox.Policy.Overlay)
	case types.ComponentMCPSandboxOwn:
		var own types.RunPolicySpec
		if sp := def.MCP.Sandbox.Policy.Spec; sp != nil {
			own = sp.Clone() // the snapshot's own lists are never written
			authored = sp.Resources
		}
		var err error
		if out.spec, out.warns, err = s.mcpOwnPolicy(ctx, b, own); err != nil {
			return mcpPolicy{}, err
		}
		bound = out.spec.Resources
	default:
		out.spec = mcpSamePolicy(b.run, b.comps, i)
	}
	out.spec = mcpOnlyKnobs(out.spec, b.run)
	out.spec.Resources = mcpSandboxResources(authored, bound, b.ceiling.Limits)
	out.holds = mcpPolicyFinish(&out.spec, b.layers, b.deny)
	return out, nil
}

// mcpHoldsRewritten is, for each server of the run that starts from a command, the count of host rules with
// effect hold its MCP policy's finish makes deny, keyed by server key, as Review judges the folded policy:
// with no outer layer read, so a hold that arrives only at the dispatch seal is not counted here (the
// run.mcp_policy.resolve row records it). A server whose policy cannot be derived is left out.
func (s *Server) mcpHoldsRewritten(ctx context.Context, in accessInputs) map[string]int {
	out := map[string]int{}
	keys := mcpServerKeys(in.comps.attached)
	b := mcpPolicyBase{run: in.spec, comps: in.comps, ungoverned: s.runUngoverned(ctx)}
	ceilingRead := false
	for i, a := range in.comps.attached {
		if !a.snapshot.Definition.LocalMCP() {
			continue
		}
		if !ceilingRead && !b.ungoverned && mcpPolicyModeOf(a.snapshot.Definition) == types.ComponentMCPSandboxOwn {
			c, err := s.effectiveCeiling(ctx)
			if err != nil {
				slog.WarnContext(ctx, "api: read the ceiling for an MCP sandbox policy at Review", slog.Any("err", err))
				return out
			}
			b.ceiling, ceilingRead = c, true
		}
		p, err := s.mcpSandboxPolicy(ctx, b, i)
		if err != nil {
			slog.WarnContext(ctx, "api: derive an MCP sandbox policy at Review", slog.String("server", keys[i]), slog.Any("err", err))
			continue
		}
		out[keys[i]] = p.holds
	}
	return out
}

// mcpNarrowerRefusal is the fold's step 6: a "narrower" overlay that names anything "same" does not
// permit, judged against "same" from the folded policy, is refused with the overlay's field. No component
// save can judge it, since there is no run to read "same" from.
func mcpNarrowerRefusal(spec types.RunPolicySpec, comps runComponents) *runRefusal {
	keys := mcpServerKeys(comps.attached)
	for i, a := range comps.attached {
		def := a.snapshot.Definition
		if !def.LocalMCP() || mcpPolicyModeOf(def) != types.ComponentMCPSandboxNarrower || def.MCP.Sandbox.Policy.Overlay == nil {
			continue
		}
		err := composer.ValidateOverlay(composer.Authority{Ceiling: mcpSamePolicy(spec, comps, i)},
			composer.Overlay{Ceiling: *def.MCP.Sandbox.Policy.Overlay})
		if err == nil {
			continue
		}
		field := ""
		if oe := (*composer.OverlayError)(nil); errors.As(err, &oe) {
			field = oe.Field
		}
		return runError(http.StatusUnprocessableEntity, reasonComponentDefinitionInvalid,
			"mcp.sandbox.policy.overlay."+field+": "+mcpSandboxSentence(mcpNarrowerWidensSentence, keys[i], ""))
	}
	return nil
}

// auditMCPPolicyResolve writes run.mcp_policy.resolve for one server of the run, before its sandbox is created:
// the mode, the digest of the policy it runs under and the finish's decisions (how many hold rules became
// deny, and each overlay entry the run's policy no longer permitted).
func (s *Server) auditMCPPolicyResolve(ctx context.Context, runID uuid.UUID, ordinal int, key string, p mcpPolicy) {
	data := map[string]any{
		"ordinal": ordinal, "mcp_sandbox": key, "mode": p.mode, "mcp_policy_digest": p.spec.Digest(),
		"holds_rewritten": p.holds, "warnings": slices.Concat([]string{}, p.warns),
	}
	if r := p.spec.Resources; r != nil {
		data["cpu_millis"], data["memory_mib"] = r.CPUMillis, r.MemoryMiB
	}
	s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.mcp_policy.resolve", key, "success", mustJSON(data)))
}
