// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// overrideEdits is what applyRunOverrides did to the policy the run started
// from, before the clamp. rows are the person's provenance, effect `added`
// until the clamp has had its say (recordPolicySource downgrades the ones the
// resolved spec lacks to `clamped`); folded is the spec the clamp then read.
type overrideEdits struct {
	diff   *client.RunOverridesDiff
	rows   []provenanceRow
	folded types.RunPolicySpec
	// The entries the person added, which the ceiling must be able to bound
	// even where the policy they started from is not clamped as a whole.
	addedHosts  []string
	addedGrants []types.GrantSpec
	// The run-wide scalars the person changed and what the policy held before, so a
	// value the ceiling refuses can be put back where the policy had it.
	setFirstUse, setAllowAll bool
	prevFirstUse             types.FirstUseMode
	prevAllowAll             bool
}

func (e *overrideEdits) add(field, value string, effect provEffect) {
	e.rows = append(e.rows, provenanceRow{Field: field, Value: value, Source: provSource{Kind: provKindPerson}, Effect: effect})
}

// owns reports whether the person's edits decided this entry, so the policy's
// own provenance does not claim it as well.
func (e *overrideEdits) owns(field, value string) bool {
	return e != nil && slices.ContainsFunc(e.rows, func(r provenanceRow) bool { return r.Field == field && r.Value == value })
}

// settle records the person's rows, and returns the diff with the edits the
// resolved spec no longer holds moved to Clamped. Nothing is recorded for a
// request without edits.
func (e *overrideEdits) settle(rec *foldRecorder, resolved types.RunPolicySpec) {
	if e == nil {
		return
	}
	now := specEntries(resolved)
	for _, row := range e.rows {
		if row.Effect == provEffectAdded && !slices.Contains(now, specEntry{row.Field, row.Value}) {
			row.Effect = provEffectClamped
		}
		rec.add(row.Field, row.Value, row.Source, row.Effect)
	}
}

// finish stamps the diff on the run's policy source once the clamp and the
// grant pipeline have run, moving the added edits they dropped to Clamped.
func (e *overrideEdits) finish(src *policySourceRecord, resolved types.RunPolicySpec) {
	if e == nil {
		return
	}
	now := specEntries(resolved)
	diff := *e.diff
	for _, row := range e.rows {
		if row.Effect == provEffectAdded && !slices.Contains(now, specEntry{row.Field, row.Value}) {
			diff.Clamped = append(diff.Clamped, row.Field+" "+row.Value)
		}
	}
	src.OverridesDiff = &diff
	src.edits = e
}

// applyRunOverrides applies the New Run card edits to the policy the run starts
// from. It runs BEFORE composer.Clamp and the member grant pipeline, so an added
// host or secret is bounded by the caller's ceiling like anything else they
// authored, and before every component, provider and autonomy gate. Each edit
// maps to an existing spec field; there is no free-form patch. Push-rule edits
// are not a spec field and ride to dispatch (the per-entry sets).
//
// It re-runs runOverridesRefusal so the fold never applies an edit the
// narrowing table refuses, whichever door called it.
func (s *Server) applyRunOverrides(ctx context.Context, r *http.Request, spec types.RunPolicySpec, o *client.RunOverrides) (types.RunPolicySpec, *overrideEdits, *runRefusal) {
	if o == nil || len(o.Items()) == 0 {
		return spec, nil, nil
	}
	if refusal := runOverridesRefusal(createRunRequest{Overrides: o}); refusal != nil {
		return spec, nil, refusal
	}
	spec = spec.Clone()
	e := &overrideEdits{diff: &client.RunOverridesDiff{RunOverrides: *o}}
	if a := o.Agent; a != nil {
		applyHostOverrides(&spec, *a, e)
		if refusal := s.applySecretOverrides(ctx, r, &spec, *a, e); refusal != nil {
			return spec, nil, refusal
		}
		applyToolRuleOverrides(&spec, a.ToolRules, e)
	}
	if h := o.Hosts; h != nil {
		applyRunHostOverrides(&spec, *h, e)
	}
	if eg := o.Egress; eg != nil {
		applyRunEgressOverrides(&spec, *eg, e)
	}
	if o.AzureDevOps != nil {
		if refusal := applyADOOverride(&spec, *o.AzureDevOps, e); refusal != nil {
			return spec, nil, refusal
		}
	}
	if refusal := applyGitPATOverrides(&spec, o.GitPAT, e); refusal != nil {
		return spec, nil, refusal
	}
	e.folded = redactSpecForUser(auditablePolicy(spec))
	return spec, e, nil
}

func applyHostOverrides(spec *types.RunPolicySpec, a client.AgentOverrides, e *overrideEdits) {
	for _, h := range a.AddHosts {
		h = strings.ToLower(h)
		if !slices.ContainsFunc(spec.AllowedDomains, func(d string) bool { return hostEqual(d, h) }) {
			spec.AllowedDomains = append(spec.AllowedDomains, h)
			e.addedHosts = append(e.addedHosts, h)
			e.add(fieldAllowed, h, provEffectAdded)
		}
	}
	for _, h := range a.RemoveHosts {
		n := len(spec.AllowedDomains)
		spec.AllowedDomains = slices.DeleteFunc(spec.AllowedDomains, func(d string) bool { return hostEqual(d, h) })
		if len(spec.AllowedDomains) != n {
			e.add(fieldAllowed, strings.ToLower(h), provEffectRemoved)
		}
	}
}

// applyRunHostOverrides adds the hosts a person put on the run beside the base
// policy's: allowed hosts to the allowlist (the ceiling then bounds them, like
// any host they authored), denied ones to the denylist, where they only take
// reach away. The base's own hosts are never edited.
func applyRunHostOverrides(spec *types.RunPolicySpec, h client.RunHostOverrides, e *overrideEdits) {
	for _, host := range h.AddAllowed {
		host = strings.ToLower(host)
		if !slices.ContainsFunc(spec.AllowedDomains, func(d string) bool { return hostEqual(d, host) }) {
			spec.AllowedDomains = append(spec.AllowedDomains, host)
			e.addedHosts = append(e.addedHosts, host)
			e.add(fieldAllowed, host, provEffectAdded)
		}
	}
	for _, host := range h.AddDenied {
		host = strings.ToLower(host)
		if !slices.ContainsFunc(spec.DeniedDomains, func(d string) bool { return hostEqual(d, host) }) {
			spec.DeniedDomains = append(spec.DeniedDomains, host)
			e.add(fieldDenied, host, provEffectAdded)
		}
	}
}

// applyRunEgressOverrides sets the run-wide egress scalars. Each is a widening
// the ceiling bounds, so it is recorded as the person's and left to the clamp.
func applyRunEgressOverrides(spec *types.RunPolicySpec, eg client.RunEgressOverrides, e *overrideEdits) {
	if eg.FirstUseApproval != "" && eg.FirstUseApproval != spec.FirstUseApproval {
		e.setFirstUse, e.prevFirstUse = true, spec.FirstUseApproval
		spec.FirstUseApproval = eg.FirstUseApproval
		e.add("first_use_approval", string(eg.FirstUseApproval), provEffectAdded)
	}
	if eg.AllowAllEgress != nil && *eg.AllowAllEgress && !spec.AllowAllEgress {
		e.setAllowAll, e.prevAllowAll = true, spec.AllowAllEgress
		spec.AllowAllEgress = true
		e.add("allow_all_egress", "true", provEffectAdded)
	}
}

// applySecretOverrides turns each added secret into an api_key header grant and
// drops every grant that pairs a removed one. An added secret must be the
// caller's own stored secret: the operator's names and another person's are
// refused, whatever the ceiling would later say, because naming a secret is
// itself the claim of ownership (OwnerOnly then pins the lookup to their row).
func (s *Server) applySecretOverrides(ctx context.Context, r *http.Request, spec *types.RunPolicySpec, a client.AgentOverrides, e *overrideEdits) *runRefusal {
	for i, sec := range a.AddSecrets {
		if !s.ownsSecretMemoized(ctx, s.secretOwnerFromRequest(r), sec.SecretName) {
			return runError(http.StatusUnprocessableEntity, reasonOverrideRefused, fmt.Sprintf(
				"overrides.agent.add_secrets[%d]: %s is not one of your stored secrets. Add it to your secrets first.", i, sec.SecretName))
		}
		scope := map[string]any{"host": strings.ToLower(sec.Host), "secret_name": sec.SecretName, "require_tls": true}
		if sec.Header != "" {
			scope["header"] = sec.Header
		}
		if sec.Format != "" {
			scope["format"] = sec.Format
		}
		g := types.GrantSpec{Kind: types.GrantAPIKey, Scope: mustJSON(scope), TTLSeconds: 3600, OwnerOnly: true}
		spec.EligibleGrants = append(spec.EligibleGrants, g)
		e.addedGrants = append(e.addedGrants, g)
		e.add(fieldGrants, grantKey(g), provEffectAdded)
	}
	for _, name := range a.RemoveSecrets {
		spec.EligibleGrants = slices.DeleteFunc(spec.EligibleGrants, func(g types.GrantSpec) bool {
			_, ref, _, covered, err := storedSecretGrantPairing(g)
			if !covered || err != nil || ref != name {
				return false
			}
			e.add(fieldGrants, grantKey(g), provEffectRemoved)
			return true
		})
	}
	return nil
}

// applyToolRuleOverrides adds hold and deny rules (the table refuses allow). A
// tool the policy already rules on keeps the stricter of the two effects, so an
// added rule can never loosen what is there.
func applyToolRuleOverrides(spec *types.RunPolicySpec, rules []types.ToolRule, e *overrideEdits) {
	strict := map[types.ToolEffect]int{types.ToolAllow: 0, types.ToolHold: 1, types.ToolDeny: 2}
	for _, nr := range rules {
		i := slices.IndexFunc(spec.ToolRules, func(r types.ToolRule) bool { return r.Tool == nr.Tool })
		switch {
		case i < 0:
			spec.ToolRules = append(spec.ToolRules, nr)
		case strict[nr.Effect] > strict[spec.ToolRules[i].Effect]:
			e.add(fieldToolRules, nr.Tool+":"+string(spec.ToolRules[i].Effect), provEffectRemoved)
			spec.ToolRules[i].Effect = nr.Effect
		default:
			continue
		}
		e.add(fieldToolRules, nr.Tool+":"+string(nr.Effect), provEffectAdded)
	}
}

// applyADOOverride narrows the run's Azure DevOps capability set to a subset of
// the one the policy names. An empty set keeps nothing, and the spec cannot say
// so: an empty azure_devops_capabilities reads as "the provider row's default
// profile" at dispatch, which would widen the run the person meant to narrow.
// So it is refused here, never folded into the source's default.
func applyADOOverride(spec *types.RunPolicySpec, o client.ADOOverrides, e *overrideEdits) *runRefusal {
	if len(o.Capabilities) == 0 {
		return overrideInvalid("azure_devops.capabilities: keeping no capability is not something this run can express; name at least one capability to keep")
	}
	var keep []adoscope.Capability
	for _, name := range o.Capabilities {
		c := adoscope.Capability(name)
		if !c.Grantable() {
			return overrideInvalid("azure_devops.capabilities: %q is not a capability a run can hold", name)
		}
		if len(spec.AzureDevOpsCapabilities) > 0 && !slices.Contains(spec.AzureDevOpsCapabilities, c) {
			return runError(http.StatusUnprocessableEntity, reasonOverrideRefused, fmt.Sprintf(
				"An override cannot add azure_devops.capabilities %s: it would give this run more reach than its source grants.", name))
		}
		keep = append(keep, c)
	}
	for _, c := range spec.AzureDevOpsCapabilities {
		if !slices.Contains(keep, c) {
			e.add(fieldADOCaps, string(c), provEffectNarrowed)
		}
	}
	spec.AzureDevOpsCapabilities = keep
	return nil
}

// applyGitPATOverrides narrows the git_pat grants of each named forge host: a
// subset of the repositories, write to read, api off. A value that would widen
// (a repository the grant does not name, write, api on) is refused. Every grant
// of the host is narrowed alike, because the proxy holds one scope per host.
func applyGitPATOverrides(spec *types.RunPolicySpec, overrides []client.GitPATOverride, e *overrideEdits) *runRefusal {
	for i, o := range overrides {
		matched := false
		for gi, g := range spec.EligibleGrants {
			if g.Kind != types.GrantGitPAT {
				continue
			}
			sc, err := types.DecodeGitPATScope(g.Scope)
			if err != nil || !hostEqual(sc.Host, o.Host) {
				continue
			}
			matched = true
			if o.Repos != nil {
				if sc.Repos != nil {
					for _, repo := range *o.Repos {
						if !slices.Contains(*sc.Repos, repo) {
							return runError(http.StatusUnprocessableEntity, reasonOverrideRefused, fmt.Sprintf(
								"An override cannot add git_pat[%d] repository %s: it would give this run more reach than its source grants.", i, repo))
						}
					}
				}
				sc.Repos = types.PATRepoSet(*o.Repos...)
			}
			if o.Access == types.PATAccessRead {
				sc.Access = types.PATAccessRead
			}
			if o.API != nil && !*o.API {
				sc.API = false
			}
			spec.EligibleGrants[gi].Scope = mustJSON(sc)
			e.add(fieldGrants, grantKey(spec.EligibleGrants[gi]), provEffectNarrowed)
		}
		if !matched {
			return overrideInvalid("git_pat[%d]: this run's policy has no git_pat grant for %s", i, o.Host)
		}
	}
	return nil
}

// boundOverrideAdds bounds the hosts and grants a person added by their ceiling
// when the policy they started from is a stored row that is not clamped as a
// whole (an unassigned member's selection, the documented residual in
// resolveRunPolicyFacts). Their additions must not ride on that row past the
// ceiling their own inline_policy would meet, so the same member pipeline runs
// on a probe holding only the additions beside the row's hosts, and whatever it
// drops is dropped from the run. dryRun: the real run's clamp audits nothing
// extra for a probe.
func (s *Server) boundOverrideAdds(ctx context.Context, r *http.Request, spec types.RunPolicySpec, e *overrideEdits, ceiling governanceCeiling) (types.RunPolicySpec, []string, *runRefusal) {
	if e == nil || len(e.addedHosts)+len(e.addedGrants) == 0 && !e.setFirstUse && !e.setAllowAll {
		return spec, nil, nil
	}
	probe := types.RunPolicySpec{AllowedDomains: slices.Clone(spec.AllowedDomains), EligibleGrants: slices.Clone(e.addedGrants),
		FirstUseApproval: spec.FirstUseApproval, AllowAllEgress: spec.AllowAllEgress}
	bounded, warns, refusal := s.boundRunUserSpec(ctx, r, probe, ceiling, "invalid policy: ", true)
	if refusal != nil {
		return spec, nil, refusal
	}
	spec = spec.Clone()
	if e.setAllowAll && !bounded.AllowAllEgress {
		spec.AllowAllEgress = e.prevAllowAll
	}
	if e.setFirstUse && bounded.FirstUseApproval != spec.FirstUseApproval.Normalize() {
		spec.FirstUseApproval = e.prevFirstUse
	}
	spec.AllowedDomains = slices.DeleteFunc(spec.AllowedDomains, func(d string) bool {
		return slices.ContainsFunc(e.addedHosts, func(h string) bool { return hostEqual(h, d) }) &&
			!slices.ContainsFunc(bounded.AllowedDomains, func(b string) bool { return hostEqual(b, d) })
	})
	spec.EligibleGrants = slices.DeleteFunc(spec.EligibleGrants, func(g types.GrantSpec) bool {
		same := func(o types.GrantSpec) bool { return o.Kind == g.Kind && string(o.Scope) == string(g.Scope) }
		return slices.ContainsFunc(e.addedGrants, same) && !slices.ContainsFunc(bounded.EligibleGrants, same)
	})
	return spec, warns, nil
}
