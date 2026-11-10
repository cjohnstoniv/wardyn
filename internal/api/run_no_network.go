// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A run launched with the built-in No network wall preset (types.WallPresetNoNetwork) gets no network at
// all. Create, Review's preflight and the policy preview refuse it while anything they can see would
// add a host (stepNoNetwork). Whatever only dispatch adds, the seal removes (wallPresetSealLayer); the
// run's proxy refuses every request (proxy.Config.NoNetwork, set at dispatch and replayed at revive);
// and every egress approval for the run is refused (wallsAdmitApproval).

// noNetworkPreset reports whether p is the built-in No network preset.
func noNetworkPreset(p *types.WallPresetOnRun) bool {
	return p != nil && p.Scope == types.WallPresetScopeBuiltin && p.ID == types.WallPresetNoNetwork
}

// stepNoNetwork refuses a No network launch that would need a host (422 no_network_needs_egress), naming
// each source. It runs after the tool policy, the last step that adds a host to the spec, in every door,
// so create, Review and the preview give the same answer.
func (s *Server) stepNoNetwork(f *runFold, rec *foldRecorder) bool {
	if !noNetworkPreset(f.source.wallPreset) {
		return true
	}
	sources := noNetworkSources(f, rec)
	if len(sources) == 0 {
		return true
	}
	return !runError(http.StatusUnprocessableEntity, reasonNoNetworkNeedsEgress,
		"This run uses the No network wall preset, and these need a network host: "+strings.Join(sources, ", ")+
			". Remove them, or launch without No network.").write(s, f.w, f.r)
}

// noNetworkSources names, sorted and once each, every source the fold can see that would give the run a
// host: the kinds deriveRunEgress adds after the mint (from a copy with an empty allowlist, so each kind
// reports every host it would add), a direct GitHub clone, each component, the Azure DevOps sign-in,
// the harness's model and each chosen model provider, and whatever is still on the spec's
// allowlist, by the source the fold recorded for it.
func noNetworkSources(f *runFold, rec *foldRecorder) []string {
	var out []string
	add := func(s string) {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	probe := f.spec.Clone()
	probe.AllowedDomains = nil
	wired := lanesWiredFor(f.scmSite, f.req.Agent, repoLocatorsOf(f.spec.WorkspaceRepos), f.spec.EligibleGrants)
	for _, d := range deriveRunEgress(&probe, f.wsRefs, f.req.Repo, f.scmSite, wired, f.ceiling.Spec.AllowedDomains) {
		if len(d.added) > 0 {
			add(noNetworkKind(d.kind))
		}
	}
	if len(f.directGitHubAdded) > 0 {
		add("github_direct")
	}
	for i, c := range f.comps.attached {
		add(noNetworkComponent(c.snapshot.Name, i))
	}
	if f.adoGrade.org != "" {
		add("azure_devops_sign_in")
	}
	if _, needsModel := agentLLMProvider(f.req.Agent); needsModel && createDoorIsModelRun(*f.req) {
		add("the " + f.req.Agent + " harness's model")
	}
	if f.mpChoice.chosen {
		add("model provider " + noNetworkProvider(f.mpChoice.provider))
	}
	for _, hc := range f.harnessChoices {
		if hc.choice.chosen {
			add("model provider " + noNetworkProvider(hc.choice.provider))
		}
	}
	for _, h := range f.spec.AllowedDomains {
		add(noNetworkHostSource(rec, h))
	}
	if f.spec.AllowAllEgress {
		add("allow_all_egress")
	}
	slices.Sort(out)
	return out
}

// noNetworkKind is a derived kind as the refusal names it: a workspace's own hosts and its clone hosts
// are both the workspace's, which is how the dry doors, which add the first before the requirements
// fold, record them too.
func noNetworkKind(kind string) string {
	if kind == derivedWorkspaceClone {
		return derivedWorkspace
	}
	return kind
}

func noNetworkComponent(name string, ordinal int) string {
	if name == "" {
		return "component #" + strconv.Itoa(ordinal+1)
	}
	return "component " + name
}

func noNetworkProvider(p types.ModelProvider) string {
	if p.Name != "" {
		return p.Name
	}
	return p.ID
}

// noNetworkHostSource names the source the fold recorded adding host, or the run's policy when none did.
func noNetworkHostSource(rec *foldRecorder, host string) string {
	for _, row := range rec.rows {
		if row.Field != fieldAllowed || row.Value != host || row.Effect != provEffectAdded {
			continue
		}
		switch row.Source.Kind {
		case provKindWorkspace:
			return derivedWorkspace
		case provKindComponent:
			return "component " + row.Source.Name
		case provKindModelProvider:
			return "model provider " + row.Source.Name
		case provKindHarness:
			return "harness " + row.Source.Name
		case provKindPosture:
			return "the harness's tool policy"
		case provKindPerson:
			return "your request"
		}
	}
	return "the run's policy"
}
