// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"reflect"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The final seal: the last policy step on every path that dispatches or revives
// a run. Each late host addition (a workspace's or a component's egress, an SCM
// lane, a provider's endpoint) is bounded by its own source and by no outer
// layer, so the seal meets the assembled policy with every outer layer once all
// of them have run. run_policy_seal_census_test.go lists those additions and
// fails on a new one, or on a widening that runs after the seal.

// sealLayer is one outer limit on a run's egress, and the record a knob it
// decides carries in the run's provenance. egress holds the three egress
// knobs, each nil when the layer does not bound it (a restriction's layer also
// carries its tool, push and recording rules, which sealPolicy meets): AllowedDomains is an
// allowlist (an empty one allows nothing), AllowAllEgress false turns open
// egress off and keeps the run's own allowlist, and DeniedDomains is added. A
// layer that only denies leaves the first two nil. grants is the credential
// grants the layer lets a run be given, nil for no limit (an empty list allows
// none); the seal never reads it, the withholding after it does. desktop is
// the desktop directions the layer blocks, nil when it bounds none.
type sealLayer struct {
	rec     policyref.Provenance
	egress  types.CeilingOverlay
	grants  *[]types.GrantSpec
	desktop *types.DesktopControls
	// dataOut is the least strict data_out rule the layer allows a run, nil when it names none.
	dataOut *types.TransferRule
	// withheldGrants is the credentials the layer keeps from a run (a restriction's withheld_grants).
	withheldGrants []types.GrantSpec
	// denyAllPushes is the layer's refusal of every push (push_rules.deny_all): a person's walls or a
	// wall preset that set it. The seal never reads it; a revive re-asserts it (reassertDenyAllPushes).
	denyAllPushes bool
	// adoCap and githubCap are the Azure DevOps and GitHub capabilities the layer lets a run hold, nil for no
	// cap; the seal never reads them, the cap helpers do (vcs_capability_cap.go).
	adoCap    *[]adoscope.Capability
	githubCap *[]ghscope.Capability
	// unread marks the layer standing in for a boundary or restriction that could not be read. Its caps list
	// nothing, so a dispatch fails closed; a credential resolve reads it as an unreadable run instead
	// (runADOCap), so a transient read error is retried and revokes nothing.
	unread bool
}

// sealDecision is one knob a layer narrowed.
type sealDecision struct {
	knob  string
	layer policyref.Provenance
}

// outerLayers is the ordered outer layers a run is sealed against, outermost
// first: the Wardyn boundary, then the restrictions the run captured (restrictionSealLayers),
// then the run owner's own walls (personalWallsSealLayer), then the run's wall preset
// (wallPresetSealLayer).
// Unit walls are inner layers, applied at resolve, never here. In no_limits there
// is no layer, so the seal is the identity. A boundary that cannot be read seals
// the run to no egress, no credential and no desktop direction at all: an unread
// boundary is never no boundary.
func (s *Server) outerLayers(ctx context.Context, run types.AgentRun) []sealLayer {
	b, err := s.governanceBoundary(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: read the Wardyn boundary at the seal; the run is sealed to no egress",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
		off := false
		none := types.DesktopControls{PasteIn: types.TransferBlocked, CopyOut: types.TransferBlocked, Upload: types.TransferBlocked, Download: types.TransferBlocked}
		held := types.TransferBlocked
		return []sealLayer{{rec: boundaryProvenance, egress: types.CeilingOverlay{AllowedDomains: &[]string{}, AllowAllEgress: &off},
			grants: &[]types.GrantSpec{}, desktop: &none, dataOut: &held, denyAllPushes: true,
			adoCap: &[]adoscope.Capability{}, githubCap: &[]ghscope.Capability{}, unread: true}}
	}
	var layers []sealLayer
	if b.HardLimits() {
		layers = append(layers, sealLayer{rec: boundaryProvenance, egress: boundarySealEgress(b.Spec), grants: b.Spec.EligibleGrants, desktop: b.Spec.Desktop,
			dataOut: b.Spec.DataOut, denyAllPushes: dataOutBlocks(b.Spec.DataOut),
			adoCap: b.Spec.AzureDevOpsCapabilities, githubCap: b.Spec.GitHubCapabilities})
	}
	layers = append(layers, s.restrictionSealLayers(ctx, run)...)
	layers = append(layers, s.personalWallsSealLayer(ctx, run)...)
	return append(layers, wallPresetSealLayer(run)...)
}

// dataOutBlocks reports whether rule is blocked, which refuses every push too.
func dataOutBlocks(rule *types.TransferRule) bool {
	return rule != nil && *rule == types.TransferBlocked
}

// reassertDenyAllPushes sets push_rules.deny_all on a revived run's proxy config when an outer layer
// refuses every push, so a revive or a restart applies the owner's walls and the run's wall preset
// again, deny_all included, whatever config it replays. The rules go where the config carries them:
// every per-entry set and the base beside them, or the policy's own block (proxy.Config refuses both).
// It only ever adds the refusal.
func reassertDenyAllPushes(cfg *proxy.Config, layers []sealLayer) {
	if !slices.ContainsFunc(layers, func(l sealLayer) bool { return l.denyAllPushes }) {
		return
	}
	if len(cfg.PushRuleSets) == 0 {
		rules := types.PushRulesSpec{}
		if cfg.Policy.PushRules != nil {
			rules = *cfg.Policy.PushRules
		}
		rules.DenyAll = true
		cfg.Policy.PushRules = &rules
		return
	}
	sets := slices.Clone(cfg.PushRuleSets)
	for i := range sets {
		sets[i].Rules.DenyAll = true
	}
	cfg.PushRuleSets = sets
	base := types.PushRulesSpec{}
	if cfg.PushRuleBase != nil {
		base = *cfg.PushRuleBase
	}
	base.DenyAll = true
	cfg.PushRuleBase = &base
}

// sealPerEntryRules seals the rules a stored proxy config carries per entry: every push rule set and the
// base beside them, and every harness's tool rules. sealRunPolicy met a restriction's push and tool rules
// into cfg.Policy, the run-wide block, which a config that carries the per-entry form refuses beside it
// (proxy.Config), so the layers are met into each per-entry rule instead and the run-wide block is left off,
// as sidecarPolicy leaves it at dispatch. Each rule only ever narrows. A config with no per-entry rules is
// untouched, and its run-wide block stays as sealRunPolicy left it.
func sealPerEntryRules(cfg *proxy.Config, layers []sealLayer) {
	if len(cfg.PushRuleSets) > 0 {
		cfg.Policy.PushRules = nil
		sets := slices.Clone(cfg.PushRuleSets)
		for i := range sets {
			if r := sealedPushRules(&sets[i].Rules, layers); r != nil {
				sets[i].Rules = *r
			}
		}
		cfg.PushRuleSets = sets
		// A base the run never had is made when a layer gives it rules: an entry with no set is held to them.
		if r := sealedPushRules(cfg.PushRuleBase, layers); r.IsSet() {
			cfg.PushRuleBase = r
		}
	}
	if len(cfg.HarnessToolRules) > 0 {
		cfg.Policy.ToolRules = nil
		sets := slices.Clone(cfg.HarnessToolRules)
		for i := range sets {
			sets[i] = sealedHarnessToolRules(sets[i], layers)
		}
		cfg.HarnessToolRules = sets
	}
}

// sealedPushRules is rules met with every layer's push rules by sealPolicy. rules is not written through.
func sealedPushRules(rules *types.PushRulesSpec, layers []sealLayer) *types.PushRulesSpec {
	var spec types.RunPolicySpec
	if rules != nil {
		c := *rules
		c.DenyPaths = slices.Clone(c.DenyPaths)
		c.RequireReviewPaths = slices.Clone(c.RequireReviewPaths)
		spec.PushRules = &c
	}
	sealPolicy(&spec, layers)
	return spec.PushRules
}

// sealedHarnessToolRules is one harness's set met with every layer's tool rules by sealPolicy, the set read as
// the flat list it was compiled from ("*" is its default).
func sealedHarnessToolRules(set types.HarnessToolRules, layers []sealLayer) types.HarnessToolRules {
	spec := types.RunPolicySpec{ToolRules: slices.Clone(set.Rules)}
	if set.Default != "" {
		spec.ToolRules = append(spec.ToolRules, types.ToolRule{Tool: "*", Effect: set.Default})
	}
	sealPolicy(&spec, layers)
	out := types.HarnessToolRules{Harness: set.Harness}
	for _, r := range spec.ToolRules {
		if r.Tool == "*" {
			out.Default = r.Effect
			continue
		}
		out.Rules = append(out.Rules, r)
	}
	return out
}

// boundarySealEgress is the boundary's egress as a seal layer: its allowlist when it names one and
// does not also allow all egress; allow_all_egress as named, or false beside an allowlist, as the
// resolve meet reads it (types.BoundarySpec.Overlay); its denies either way. An explicit false with
// no allowlist turns open egress off and keeps the run's own allowlist, so a run that was open when
// it was frozen is closed at its revive. Naming neither leaves a deny-only layer.
func boundarySealEgress(sp types.BoundarySpec) types.CeilingOverlay {
	var out types.CeilingOverlay
	open := sp.AllowAllEgress != nil && *sp.AllowAllEgress
	if sp.AllowedDomains != nil && !open {
		list := slices.Clone(*sp.AllowedDomains)
		if list == nil {
			list = []string{}
		}
		out.AllowedDomains = &list
	}
	switch {
	case sp.AllowAllEgress != nil:
		v := *sp.AllowAllEgress
		out.AllowAllEgress = &v
	case out.AllowedDomains != nil:
		off := false
		out.AllowAllEgress = &off
	}
	if sp.DeniedDomains != nil && len(*sp.DeniedDomains) > 0 {
		deny := slices.Clone(*sp.DeniedDomains)
		out.DeniedDomains = &deny
	}
	return out
}

// sealRunPolicy seals policy against the run's outer layers and records which
// layer decided each knob it narrowed: in prov when the path keeps a provenance
// record (dispatch; nil on a revive), and in the log. Where several layers narrow
// one knob, the last of them, the innermost, is the one recorded. It returns the
// layers it read, so the credentials the path withholds next are judged against
// the same read (withholding).
func (s *Server) sealRunPolicy(ctx context.Context, run types.AgentRun, policy *types.RunPolicySpec, prov knobProvenance) []sealLayer {
	layers := s.outerLayers(ctx, run)
	for _, d := range sealPolicy(policy, layers) {
		if prov != nil {
			prov[d.knob] = d.layer
		}
		slog.InfoContext(ctx, "wardynd: the final seal narrowed the run's policy",
			slog.String("run_id", run.ID.String()), slog.String("knob", d.knob), slog.String("layer", d.layer.Layer))
	}
	return layers
}

// sealPolicy meets spec with each layer in order, by the governance meet
// (composer.ApplyOverlay): the allowlist is intersected, allow_all_egress
// survives only where every layer allows it, and the deny lists are merged. A
// restriction's layer is met as a create meets it (composer.ApplyRestrictionOverlay),
// so an open run is closed to the restriction's allowlist, not to nothing; at the
// boundary's, allow-all is a late widening and keeps only the run's own list. A
// layer that carries tool, push or recording rules (a restriction's) meets those
// too, and each is written back only when it narrows, so a policy the layer
// already holds keeps its spelling. The desktop directions a layer blocks are
// blocked (types.DesktopControls.Meet), and its data_out rule is the stricter value. Only those knobs are read, so nothing
// else in spec can change. With no layers spec is untouched.
func sealPolicy(spec *types.RunPolicySpec, layers []sealLayer) []sealDecision {
	var decided []sealDecision
	for _, l := range layers {
		in := types.RunPolicySpec{AllowedDomains: spec.AllowedDomains, DeniedDomains: spec.DeniedDomains, AllowAllEgress: spec.AllowAllEgress,
			ToolRules: spec.ToolRules, PushRules: spec.PushRules, Recording: spec.Recording, HostRules: spec.HostRules}
		meet := composer.ApplyOverlay
		if l.rec.Layer == policyref.LayerRestriction {
			meet = composer.ApplyRestrictionOverlay
		}
		met, _, err := meet(composer.Authority{Ceiling: in}, composer.Overlay{Ceiling: l.egress})
		if err != nil {
			// The egress knobs alone have no unsatisfiable meet; should one appear, nothing is reachable and
			// the rules stay as they were.
			met.Ceiling = in
			met.Ceiling.AllowedDomains, met.Ceiling.AllowAllEgress = []string{}, false
		}
		out := met.Ceiling
		// Compared canonical, as the provenance record compares, so a re-spelling decides nothing.
		before, after := canonicalKnobs(in), canonicalKnobs(out)
		if !slices.Equal(before.AllowedDomains, after.AllowedDomains) {
			decided = append(decided, sealDecision{"allowed_domains", l.rec})
		}
		if before.AllowAllEgress != after.AllowAllEgress {
			decided = append(decided, sealDecision{"allow_all_egress", l.rec})
		}
		if !slices.Equal(before.DeniedDomains, after.DeniedDomains) {
			decided = append(decided, sealDecision{"denied_domains", l.rec})
		}
		spec.AllowedDomains, spec.DeniedDomains, spec.AllowAllEgress = out.AllowedDomains, out.DeniedDomains, out.AllowAllEgress
		if !slices.Equal(before.ToolRules, after.ToolRules) {
			spec.ToolRules = out.ToolRules
			decided = append(decided, sealDecision{"tool_rules", l.rec})
		}
		if !reflect.DeepEqual(before.PushRules, after.PushRules) {
			spec.PushRules = out.PushRules
			decided = append(decided, sealDecision{"push_rules", l.rec})
		}
		// A layer's host rule sets are appended, never merged (types.AppendHostRuleSets).
		if !reflect.DeepEqual(in.HostRules, out.HostRules) {
			spec.HostRules = out.HostRules
			decided = append(decided, sealDecision{"host_rules", l.rec})
		}
		if before.Recording != after.Recording {
			spec.Recording = out.Recording
			decided = append(decided, sealDecision{"recording", l.rec})
		}
		if l.desktop != nil {
			// Written only when it narrows, so a run the layer already holds keeps its spelling.
			if d := spec.Desktop.Meet(l.desktop); d != spec.Desktop.Effective() {
				spec.Desktop = &d
				decided = append(decided, sealDecision{"desktop", l.rec})
			}
		}
		if l.dataOut != nil {
			if v := types.StricterTransfer(spec.DataOut.DataOut(), l.dataOut.DataOut()); v != spec.DataOut.DataOut() {
				spec.DataOut = v
				decided = append(decided, sealDecision{"data_out", l.rec})
			}
		}
	}
	return decided
}
