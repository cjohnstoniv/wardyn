// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"bytes"
	"cmp"
	"encoding/json"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/runner/sizing"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// meetPosture composes every ceiling field that is not network reach or push
// content: the confinement floor, grants, lifecycle, what is mounted, what the
// tools may do, and the sandbox size.
func (m *meeter) meetPosture(o types.CeilingOverlay) {
	c := &m.out.Ceiling
	if o.MinConfinementClass != nil {
		m.meetConfinement(*o.MinConfinementClass)
	}
	if o.EligibleGrants != nil {
		// The overlay's grants pass through: re-intersecting them against the
		// resolved base is the resolver's job, step by step, with the one grant
		// comparator (composer cannot see which base a chain step resolved).
		c.EligibleGrants = cloneProposal(types.RunPolicySpec{EligibleGrants: *o.EligibleGrants}).EligibleGrants
	}
	if o.AutoStopAfterSec != nil {
		b, v := c.AutoStopAfterSec, *o.AutoStopAfterSec
		if looser(v, b) {
			m.widen("auto_stop_after_sec", "%d does not stop an idle sandbox as soon as the base's %d", v, b)
		}
		c.AutoStopAfterSec = autoStopMeet(b, v)
	}
	if o.WorkspaceMounts != nil {
		c.WorkspaceMounts = m.meetMounts(c.WorkspaceMounts, *o.WorkspaceMounts)
	}
	if o.WorkspaceRepos != nil {
		c.WorkspaceRepos = m.meetRepos(c.WorkspaceRepos, *o.WorkspaceRepos)
	}
	if o.LLMInspection != nil {
		m.meetLLM(o.LLMInspection)
	}
	if o.UIApps != nil {
		c.UIApps = m.meetUIApps(c.UIApps, *o.UIApps)
	}
	if o.Resources != nil {
		m.meetResources(o.Resources)
	}
	if o.ToolRules != nil {
		c.ToolRules = m.meetToolRules(c.ToolRules, *o.ToolRules)
	}
	if o.GitPushAnyBranch != nil {
		if *o.GitPushAnyBranch && !c.GitPushAnyBranch {
			m.widen("git_push_any_branch", "the base keeps push branch-namespace confinement on, and an overlay can only turn it on")
		}
		c.GitPushAnyBranch = c.GitPushAnyBranch && *o.GitPushAnyBranch
	}
	if o.AzureDevOpsCapabilities != nil {
		m.meetCapabilities(*o.AzureDevOpsCapabilities)
	}
}

func (m *meeter) meetConfinement(v types.ConfinementClass) {
	c := &m.out.Ceiling
	switch br, vr := confinementRank(c.MinConfinementClass), confinementRank(v); {
	case vr > br:
		c.MinConfinementClass = types.ConfinementClass(strings.ToUpper(strings.TrimSpace(string(v))))
	case vr < br:
		m.widen("min_confinement_class", "%q is below the base's %q", v, c.MinConfinementClass)
	}
}

// autoStopMeet is the smaller positive of two auto-stop bounds. A value <= 0
// reaps nothing, so two of them stay unbounded, and the smaller spelling wins
// so the result does not depend on which side said it.
func autoStopMeet(a, b int) int {
	if a <= 0 && b <= 0 {
		return min(a, b)
	}
	return tightest(a, b)
}

type mountKey struct{ source, target string }

// meetMounts keeps the (source, target) pairs both sides name, read-only if
// either side is.
func (m *meeter) meetMounts(base, ov []types.WorkspaceMount) []types.WorkspaceMount {
	var out []types.WorkspaceMount
	seen := map[mountKey]bool{}
	for _, o := range ov {
		k := mountKey{o.Source, o.Target}
		i := slices.IndexFunc(base, func(b types.WorkspaceMount) bool { return mountKey{b.Source, b.Target} == k })
		switch {
		case i < 0:
			m.widen("workspace_mounts", "%s:%s is not a mount of the base", o.Source, o.Target)
			continue
		case base[i].ReadOnlyOrDefault() && !o.ReadOnlyOrDefault():
			m.widen("workspace_mounts", "%s:%s is read-only in the base", o.Source, o.Target)
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		ro := base[i].ReadOnlyOrDefault() || o.ReadOnlyOrDefault()
		out = append(out, types.WorkspaceMount{Source: o.Source, Target: o.Target, ReadOnly: &ro})
	}
	slices.SortFunc(out, func(a, b types.WorkspaceMount) int {
		return strings.Compare(a.Source+"\x00"+a.Target, b.Source+"\x00"+b.Target)
	})
	return out
}

// meetRepos keeps the repos both sides name, by identity (repo, target, ref).
func (m *meeter) meetRepos(base, ov []types.WorkspaceRepo) []types.WorkspaceRepo {
	var out []types.WorkspaceRepo
	for _, o := range ov {
		if !slices.Contains(base, o) {
			m.widen("workspace_repos", "%s is not a repo of the base", o.Repo)
			continue
		}
		if !slices.Contains(out, o) {
			out = append(out, o)
		}
	}
	slices.SortFunc(out, func(a, b types.WorkspaceRepo) int {
		return strings.Compare(a.Repo+"\x00"+a.Target+"\x00"+a.Ref, b.Repo+"\x00"+b.Target+"\x00"+b.Ref)
	})
	return out
}

// meetLLM takes whichever side names an inspection, and refuses two that
// differ: neither is stricter than the other in any order a meet can state.
func (m *meeter) meetLLM(o *types.LLMInspectionSpec) {
	c := &m.out.Ceiling
	cp := cloneProposal(types.RunPolicySpec{LLMInspection: o}).LLMInspection
	if c.LLMInspection == nil {
		c.LLMInspection = cp
		return
	}
	if !sameJSON(c.LLMInspection, cp) {
		m.fail(ReasonOverlayInvalid, "llm_inspection", "the overlay's inspection differs from the base's; set it on one side only")
	}
}

func sameJSON(a, b any) bool {
	x, errA := json.Marshal(a)
	y, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(x, y)
}

// meetUIApps keeps the (name, port) pairs both sides name. An empty list is
// "no UI apps", never "no opinion", so a base without any admits none.
func (m *meeter) meetUIApps(base, ov []types.UIApp) []types.UIApp {
	var out []types.UIApp
	for _, o := range ov {
		i := slices.IndexFunc(base, func(b types.UIApp) bool { return b.Name == o.Name && b.Port == o.Port })
		if i < 0 {
			m.widen("ui_apps", "%s:%d is not a UI app of the base", o.Name, o.Port)
			continue
		}
		app := types.UIApp{Name: o.Name, Port: o.Port, Path: min(base[i].Path, o.Path)}
		if !slices.ContainsFunc(out, func(x types.UIApp) bool { return x.Name == app.Name && x.Port == app.Port }) {
			out = append(out, app)
		}
	}
	slices.SortFunc(out, func(a, b types.UIApp) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), cmp.Compare(a.Port, b.Port))
	})
	return out
}

// meetResources composes the sandbox size per field. A zero CPU, memory or pids
// limit means the deployment's default size, so it is replaced by that value
// before the smaller is taken; a zero disk limit is unbounded.
func (m *meeter) meetResources(o *types.ResourcesOverlay) {
	c := &m.out.Ceiling
	var r types.ResourceLimits
	if c.Resources != nil {
		r = *c.Resources
	}
	eff := sizing.EffectiveLimits()
	m.meetSize("resources.cpu_millis", &r.CPUMillis, o.CPUMillis, int(eff.CPUMillis))
	m.meetSize("resources.memory_mib", &r.MemoryMiB, o.MemoryMiB, int(eff.MemoryMiB))
	m.meetSize("resources.pids_limit", &r.PidsLimit, o.PidsLimit, int(eff.PidsLimit))
	m.meetSize("resources.disk_mib", &r.DiskMiB, o.DiskMiB, 0)
	if r == (types.ResourceLimits{}) {
		c.Resources = nil
		return
	}
	c.Resources = &r
}

// meetSize meets one size field. def is the runtime's value for an unset field
// (0 where unset means unbounded).
func (m *meeter) meetSize(field string, base *int, ov *int, def int) {
	if ov == nil {
		return
	}
	norm := func(x int) int {
		if x <= 0 {
			return def
		}
		return x
	}
	b, v := norm(*base), norm(*ov)
	if looser(v, b) {
		m.widen(field, "%d is larger than the base's %d", v, b)
	}
	*base = tightest(b, v)
}

// toolEffects reads a rule list the way the proxy compiles it: a tool maps to
// its (last) effect, and "*" is the default for an unnamed tool.
func toolEffects(rules []types.ToolRule) map[string]types.ToolEffect {
	out := make(map[string]types.ToolEffect, len(rules))
	for _, r := range rules {
		out[r.Tool] = r.Effect
	}
	return out
}

// effectFor is a rule list's effect on tool: the named rule, else the "*"
// default, else hold (the toolgate's own fallback).
func effectFor(rules map[string]types.ToolEffect, tool string) types.ToolEffect {
	if e, ok := rules[tool]; ok {
		return e
	}
	if e, ok := rules["*"]; ok {
		return e
	}
	return types.ToolHold
}

// meetToolRules takes, for every tool either side names and for the "*" default,
// the stricter effect, and spells the result out so no tool falls back to a
// default the other side would have read differently.
func (m *meeter) meetToolRules(base, ov []types.ToolRule) []types.ToolRule {
	if len(base) == 0 && len(ov) == 0 {
		return nil
	}
	b, o := toolEffects(base), toolEffects(ov)
	tools := map[string]bool{"*": true}
	for t := range b {
		tools[t] = true
	}
	for t := range o {
		tools[t] = true
	}
	var out []types.ToolRule
	for t := range tools {
		be, oe := effectFor(b, t), effectFor(o, t)
		if toolStrictness(oe) < toolStrictness(be) {
			m.widen("tool_rules", "tool %q is %s in the overlay and %s in the base", t, oe, be)
		}
		e := be
		if toolStrictness(oe) > toolStrictness(be) {
			e = oe
		}
		out = append(out, types.ToolRule{Tool: t, Effect: e})
	}
	slices.SortFunc(out, func(x, y types.ToolRule) int { return strings.Compare(x.Tool, y.Tool) })
	return out
}

// meetCapabilities intersects two capability lists where empty means "the
// provider row's default profile", which a list cannot be proven within: an
// overlay list under an empty base is a widening and leaves the base empty. A
// disjoint pair has no representable result.
func (m *meeter) meetCapabilities(ov []adoscope.Capability) {
	c := &m.out.Ceiling
	want := capStrings(ov)
	base := capStrings(c.AzureDevOpsCapabilities)
	if len(base) == 0 {
		// An empty base is the provider row's default profile, which a list
		// cannot be proven within, so the base's reading is kept.
		m.widen("azure_devops_capabilities", "the base carries the provider row's default profile, which a list cannot be proven within")
		return
	}
	var both []string
	for _, s := range want {
		if slices.Contains(base, s) {
			both = append(both, s)
		}
	}
	switch {
	case len(both) == 0:
		m.fail(ReasonOverlayUnsatisfiable, "azure_devops_capabilities", "no capability is in both the base (%s) and the overlay (%s)", trimJoin(base), trimJoin(want))
	case len(both) < len(want):
		m.widen("azure_devops_capabilities", "the base does not carry %s", trimJoin(slicesDiff(want, both)))
	}
	c.AzureDevOpsCapabilities = toCaps(both)
}

func capStrings(in []adoscope.Capability) []string {
	out := make([]string, 0, len(in))
	for _, c := range in {
		out = append(out, string(c))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func toCaps(in []string) []adoscope.Capability {
	out := make([]adoscope.Capability, 0, len(in))
	for _, s := range in {
		out = append(out, adoscope.Capability(s))
	}
	return out
}
