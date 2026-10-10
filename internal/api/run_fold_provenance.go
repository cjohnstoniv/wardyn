// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"slices"
	"strconv"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// The closed vocabularies of a provenance row. The console renders a chip from
// the source kind and the effect; a kind it does not know renders no chip.
const (
	provKindPolicy        = "policy"         // the saved, default or custom policy the run started from
	provKindCeiling       = "ceiling"        // the caller's governance ceiling
	provKindWorkspace     = "workspace"      // an attached workspace (name)
	provKindComponent     = "component"      // an attached component (name)
	provKindModelProvider = "model_provider" // the run's model provider (name)
	provKindPerson        = "person"         // the caller's own request or override
)

// provEffect is what a source did to the entry.
type provEffect string

const (
	provEffectAdded    provEffect = "added"    // the entry is in the resolved spec because of this source
	provEffectClamped  provEffect = "clamped"  // the source asked for it and the ceiling removed it
	provEffectNarrowed provEffect = "narrowed" // the entry stands, reduced (write to read), or a capability was taken away
	provEffectRemoved  provEffect = "removed"  // the person's override dropped it
)

// provSource says who is responsible for a row. ID and Name are empty when the
// kind has none (the ceiling) or the source has none (the default policy's id).
type provSource struct {
	Kind string `json:"kind"`
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// provenanceRow is one entry of the policy preview's and Review's provenance.
// Field is a RunPolicySpec json name and Value a redaction-safe entry key (the
// keys explainRunPolicy uses): a host, a grant as "kind:host", a mount's
// target, a repository as "repo@ref", a capability.
type provenanceRow struct {
	Field  string     `json:"field"`
	Value  string     `json:"value"`
	Source provSource `json:"source"`
	Effect provEffect `json:"effect"`
}

// foldRecorder collects what each step of the shared fold contributed. Every
// step receives it; the doors read its rows from runFold.prov.
type foldRecorder struct {
	rows []provenanceRow
}

func (rec *foldRecorder) add(field, value string, src provSource, effect provEffect) {
	rec.rows = append(rec.rows, provenanceRow{Field: field, Value: value, Source: src, Effect: effect})
}

// result is the rows once each, in a fixed order: field order of the policy,
// then value, source and effect. Never nil, so the wire carries [] not null.
func (rec *foldRecorder) result() []provenanceRow {
	out := slices.Clone(rec.rows)
	if out == nil {
		return []provenanceRow{}
	}
	slices.SortStableFunc(out, func(a, b provenanceRow) int {
		return cmp.Or(
			cmp.Compare(fieldRank(a.Field), fieldRank(b.Field)), cmp.Compare(a.Field, b.Field),
			cmp.Compare(a.Value, b.Value), cmp.Compare(a.Source.Kind, b.Source.Kind),
			cmp.Compare(a.Source.Name, b.Source.Name), cmp.Compare(a.Source.ID, b.Source.ID),
			cmp.Compare(a.Effect, b.Effect),
		)
	})
	return slices.Compact(out)
}

func fieldRank(field string) int {
	if i := slices.Index(policyFieldOrder, field); i >= 0 {
		return i
	}
	return len(policyFieldOrder)
}

// specEntry is one (field, value) of a spec in the keys policyFieldSets uses.
type specEntry struct{ field, value string }

func specEntries(sp types.RunPolicySpec) []specEntry {
	sets := policyFieldSets(sp)
	var out []specEntry
	for _, field := range policyFieldOrder {
		for _, v := range sets[field] {
			if e := (specEntry{field, v}); !slices.Contains(out, e) {
				out = append(out, e)
			}
		}
	}
	return out
}

// recordPolicyFold is what the policy chokepoint did to the policy the run
// started from: an entry that survived is the policy's; one that is only in the
// resolved spec was put there by the ceiling; one the policy asked for and the
// resolved spec lacks was clamped.
//
// A clamped row names an entry the resolved spec no longer holds, so it is
// recorded only for an inline policy, whose entries the caller typed. A saved or
// default policy is the operator's, and what the ceiling removed from it is not
// the caller's to read (previewSafeWarnings keeps the same line).
func recordPolicyFold(rec *foldRecorder, authored, resolved types.RunPolicySpec, policy provSource, callerAuthored bool) {
	// An unset first_use_approval is the fail-closed mode once composer.Clamp ran.
	authored.FirstUseApproval, resolved.FirstUseApproval = authored.FirstUseApproval.Normalize(), resolved.FirstUseApproval.Normalize()
	was, now := specEntries(authored), specEntries(resolved)
	for _, e := range now {
		src := policy
		if !slices.Contains(was, e) {
			src = provSource{Kind: provKindCeiling}
		}
		rec.add(e.field, e.value, src, provEffectAdded)
	}
	for _, e := range was {
		if callerAuthored && !slices.Contains(now, e) {
			rec.add(e.field, e.value, policy, provEffectClamped)
		}
	}
}

func policyProvSource(rec policySourceRecord) provSource {
	src := provSource{Kind: provKindPolicy, Name: rec.Name}
	if rec.PolicyID != nil {
		src.ID = rec.PolicyID.String()
	}
	if src.Name == "" {
		src.Name = map[string]string{policyKindInline: "custom", policyKindDefault: "default", policyKindProfile: "default"}[rec.Kind]
	}
	return src
}

func workspaceProvSource(ws types.Workspace) provSource {
	return provSource{Kind: provKindWorkspace, ID: ws.ID.String(), Name: ws.Name}
}

// componentProvSource names a component by the id its componentFact row carries.
func componentProvSource(c attachedComponent, ordinal int) provSource {
	id := "inline:" + strconv.Itoa(ordinal)
	if c.snapshot.ComponentID != nil {
		id = c.snapshot.ComponentID.String()
	}
	return provSource{Kind: provKindComponent, ID: id, Name: c.snapshot.Name}
}

// recordWorkspaceSources attributes the mounts and repositories of the spec
// that one attached workspace's sources supply.
func recordWorkspaceSources(rec *foldRecorder, spec types.RunPolicySpec, ws types.Workspace) {
	from := func(typ types.WorkspaceSourceType, source string) bool {
		return slices.ContainsFunc(ws.Sources, func(s types.WorkspaceSource) bool {
			return s.Type == typ && (s.Path == source || s.Source == source)
		})
	}
	var mine types.RunPolicySpec
	for _, m := range spec.WorkspaceMounts {
		if from(types.WorkspaceSourceTypeLocalDir, m.Source) {
			mine.WorkspaceMounts = append(mine.WorkspaceMounts, m)
		}
	}
	for _, r := range spec.WorkspaceRepos {
		if from(types.WorkspaceSourceTypeRepo, r.Repo) {
			mine.WorkspaceRepos = append(mine.WorkspaceRepos, r)
		}
	}
	recordSpecAdds(rec, types.RunPolicySpec{}, mine, workspaceProvSource(ws))
}

// recordSpecAdds records every entry of after that before lacks as added by src.
func recordSpecAdds(rec *foldRecorder, before, after types.RunPolicySpec, src provSource) {
	was := specEntries(before)
	for _, e := range specEntries(after) {
		if !slices.Contains(was, e) {
			rec.add(e.field, e.value, src, provEffectAdded)
		}
	}
}

// recordRequirementFold is what one workspace's requirements contract did to
// the spec: what it added, and the writes it narrowed.
func recordRequirementFold(rec *foldRecorder, before, after types.RunPolicySpec, ws types.Workspace, sel client.WorkspaceSelection) {
	recordSpecAdds(rec, before, after, workspaceProvSource(ws))
	recordWriteNarrowing(rec, before.WorkspaceMounts, after.WorkspaceMounts, ws, sel)
}

// directGitHubSource is who asked for a direct GitHub clone: the workspace that
// holds the repository, else the person who named it on the request.
func directGitHubSource(req createRunRequest, spec types.RunPolicySpec, wsRefs []types.Workspace) provSource {
	if !directGitHubRepo(req.Repo) {
		for _, ws := range wsRefs {
			if slices.ContainsFunc(spec.WorkspaceRepos, func(r types.WorkspaceRepo) bool {
				return directGitHubRepo(r.Repo) && slices.ContainsFunc(ws.Sources, func(s types.WorkspaceSource) bool {
					return s.Type == types.WorkspaceSourceTypeRepo && s.Source == r.Repo
				})
			}) {
				return workspaceProvSource(ws)
			}
		}
	}
	return provSource{Kind: provKindPerson}
}

// recordComponents records what each attached component put on the spec: the
// hosts it widened the allowlist by and the grant of each of its secrets.
func recordComponents(rec *foldRecorder, comps runComponents) {
	for i, c := range comps.attached {
		src := componentProvSource(c, i)
		for _, h := range c.addedHosts {
			rec.add(fieldAllowed, h, src, provEffectAdded)
		}
		for _, sec := range c.snapshot.Definition.Secrets {
			rec.add(fieldGrants, grantKey(componentGrant(sec)), src, provEffectAdded)
		}
	}
}

// recordProviderHost records the key host the chosen model provider serves the
// agent through, the host dispatch adds to the allowlist. Bedrock's hosts follow
// the credential's region at dispatch and are not known here. The source carries
// the provider's name and never its id: the preview does not publish provider
// ids (TestPolicyPreviewProviderAuthorizationWithoutLiveness).
func recordProviderHost(rec *foldRecorder, choice runProviderChoice, agent string) {
	if !choice.chosen {
		return
	}
	if lane, ok := providerKeyLaneFor(choice.provider, agent); ok {
		rec.add(fieldAllowed, lane.host, provSource{Kind: provKindModelProvider, Name: choice.provider.Name}, provEffectAdded)
	}
}

// recordADONarrowing records each capability the member's standing took away.
func recordADONarrowing(rec *foldRecorder, dropped []adoscope.Capability) {
	for _, c := range dropped {
		rec.add(fieldADOCaps, string(c), provSource{Kind: provKindCeiling}, provEffectNarrowed)
	}
}

// recordWriteNarrowing records each mount the requirements fold turned from
// writable to read-only (applyWriteNarrowing changes the flag in place, so the
// two lists line up). It is the person's when their own read-only selection
// narrowed a write the contract granted, and the workspace's otherwise.
func recordWriteNarrowing(rec *foldRecorder, before, after []types.WorkspaceMount, ws types.Workspace, sel client.WorkspaceSelection) {
	for i := range min(len(before), len(after)) {
		if before[i].ReadOnlyOrDefault() || !after[i].ReadOnlyOrDefault() {
			continue
		}
		src := workspaceProvSource(ws)
		key := "write:" + before[i].Source
		if req, ok := effectiveRequirements(ws)[key]; ok && (req.Level == "required" || slices.Contains(sel.EnabledOptional, key)) &&
			sel.ReadOnly != nil && *sel.ReadOnly {
			src = provSource{Kind: provKindPerson}
		}
		rec.add(fieldMounts, before[i].Target, src, provEffectNarrowed)
	}
}
