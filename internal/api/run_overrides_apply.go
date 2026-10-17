// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// overrideEdits is what applyRunOverrides did to the policy the run started
// from, before the clamp. rows are the person's provenance, effect `added`
// until the clamp has had its say (recordPolicySource downgrades the ones the
// resolved spec lacks to `clamped`); folded is the spec the clamp then read.
type overrideEdits struct {
	// applied is the edits that took effect, as the request spelled them: a
	// no-op (a hold on a tool the policy already denies, a removal of what is not
	// there) is not in it, and finish moves what the ceiling dropped to Clamped.
	applied client.RunOverrides
	rows    []provenanceRow
	folded  types.RunPolicySpec
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

// agent is the applied agent block, made on first use.
func (e *overrideEdits) agent() *client.AgentOverrides {
	if e.applied.Agent == nil {
		e.applied.Agent = &client.AgentOverrides{}
	}
	return e.applied.Agent
}

func (e *overrideEdits) hosts() *client.RunHostOverrides {
	if e.applied.Hosts == nil {
		e.applied.Hosts = &client.RunHostOverrides{}
	}
	return e.applied.Hosts
}

func (e *overrideEdits) egress() *client.RunEgressOverrides {
	if e.applied.Egress == nil {
		e.applied.Egress = &client.RunEgressOverrides{}
	}
	return e.applied.Egress
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
// grant pipeline have run. Only edits that took effect are in it: what the
// ceiling or the member grant pipeline dropped moves to Clamped, and a request
// whose every edit was a no-op records no diff.
func (e *overrideEdits) finish(src *policySourceRecord, resolved types.RunPolicySpec) {
	if e == nil {
		return
	}
	now := specEntries(resolved)
	diff := client.RunOverridesDiff{RunOverrides: e.applied}
	for _, row := range e.rows {
		if row.Effect == provEffectAdded && !slices.Contains(now, specEntry{row.Field, row.Value}) {
			diff.Clamped = append(diff.Clamped, row.Field+" "+row.Value)
			diff.RunOverrides = withoutClamped(diff.RunOverrides, row)
		}
	}
	src.edits = e
	if diff.Empty() {
		src.OverridesDiff = nil
		return
	}
	src.OverridesDiff = &diff
}

// withoutClamped removes the edit a clamped row stands for from the applied block.
func withoutClamped(o client.RunOverrides, row provenanceRow) client.RunOverrides {
	if a := o.Agent; a != nil {
		c := *a
		switch row.Field {
		case fieldAllowed:
			c.AddHosts = slices.DeleteFunc(slices.Clone(c.AddHosts), func(h string) bool { return hostEqual(h, row.Value) })
		case fieldGrants:
			c.AddSecrets = slices.DeleteFunc(slices.Clone(c.AddSecrets), func(sec client.SecretOverride) bool {
				return "api_key:"+strings.ToLower(sec.Host) == row.Value
			})
		}
		o.Agent = &c
	}
	if h := o.Hosts; h != nil && row.Field == fieldAllowed {
		c := *h
		c.AddAllowed = slices.DeleteFunc(slices.Clone(c.AddAllowed), func(v string) bool { return hostEqual(v, row.Value) })
		o.Hosts = &c
	}
	if eg := o.Egress; eg != nil {
		c := *eg
		switch row.Field {
		case "first_use_approval":
			c.FirstUseApproval = ""
		case "allow_all_egress":
			c.AllowAllEgress = nil
		}
		o.Egress = &c
	}
	return o.Pruned()
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
func (s *Server) applyRunOverrides(ctx context.Context, r *http.Request, spec types.RunPolicySpec, req *createRunRequest) (types.RunPolicySpec, *overrideEdits, *runRefusal) {
	o := req.Overrides
	if o == nil || len(o.Items()) == 0 {
		return spec, nil, nil
	}
	if refusal := runOverridesRefusal(*req); refusal != nil {
		return spec, nil, refusal
	}
	spec = spec.Clone()
	e := &overrideEdits{}
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
		base, refusal := s.adoOverrideBase(ctx, r, spec, req)
		if refusal == nil {
			refusal = applyADOOverride(&spec, *o.AzureDevOps, base, e)
		}
		if refusal != nil {
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
			e.agent().AddHosts = append(e.agent().AddHosts, h)
			e.add(fieldAllowed, h, provEffectAdded)
		}
	}
	for _, h := range a.RemoveHosts {
		n := len(spec.AllowedDomains)
		spec.AllowedDomains = slices.DeleteFunc(spec.AllowedDomains, func(d string) bool { return hostEqual(d, h) })
		if len(spec.AllowedDomains) != n {
			e.agent().RemoveHosts = append(e.agent().RemoveHosts, h)
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
			e.hosts().AddAllowed = append(e.hosts().AddAllowed, host)
			e.add(fieldAllowed, host, provEffectAdded)
		}
	}
	for _, host := range h.AddDenied {
		host = strings.ToLower(host)
		if !slices.ContainsFunc(spec.DeniedDomains, func(d string) bool { return hostEqual(d, host) }) {
			spec.DeniedDomains = append(spec.DeniedDomains, host)
			e.hosts().AddDenied = append(e.hosts().AddDenied, host)
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
		e.egress().FirstUseApproval = eg.FirstUseApproval
		e.add("first_use_approval", string(eg.FirstUseApproval), provEffectAdded)
	}
	if eg.AllowAllEgress != nil && *eg.AllowAllEgress && !spec.AllowAllEgress {
		e.setAllowAll, e.prevAllowAll = true, spec.AllowAllEgress
		spec.AllowAllEgress = true
		e.egress().AllowAllEgress = eg.AllowAllEgress
		e.add("allow_all_egress", "true", provEffectAdded)
	}
}

// applySecretOverrides turns each added secret into an api_key header grant and
// drops every grant that pairs a removed one. An added or removed secret must be
// the caller's own stored secret: the operator's names and another person's are
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
		e.agent().AddSecrets = append(e.agent().AddSecrets, sec)
		e.add(fieldGrants, grantKey(g), provEffectAdded)
	}
	for i, name := range a.RemoveSecrets {
		// A member's ownership first, and the same refusal for a name that is
		// someone else's and for one nobody has (an operator reads the names, so
		// is not asked): matching the policy's grants before this would
		// tell a person which operator secrets it pairs, which a member never
		// reads (policy reads strip the references).
		if !s.isOperator(r.Context()) && !s.ownsSecretMemoized(ctx, s.secretOwnerFromRequest(r), name) {
			return runError(http.StatusUnprocessableEntity, reasonOverrideRefused, fmt.Sprintf(
				"overrides.agent.remove_secrets[%d]: %s is not one of your stored secrets.", i, name))
		}
		removed := false
		spec.EligibleGrants = slices.DeleteFunc(spec.EligibleGrants, func(g types.GrantSpec) bool {
			_, ref, _, covered, err := storedSecretGrantPairing(g)
			if !covered || err != nil || ref != name {
				return false
			}
			e.add(fieldGrants, grantKey(g), provEffectRemoved)
			removed = true
			return true
		})
		if removed {
			e.agent().RemoveSecrets = append(e.agent().RemoveSecrets, name)
		}
	}
	return nil
}

// applyToolRuleOverrides adds hold and deny rules (the table refuses allow). Each
// is compared with the tool's EFFECTIVE effect (its exact rule, else the "*"
// default, as the proxy and composer.Clamp look a tool up) and applies only when
// stricter, so an added rule can never loosen what is there: a hold on a tool the
// policy's "*" denies is dropped, not turned into an exact-name hold that would
// shadow the deny.
func applyToolRuleOverrides(spec *types.RunPolicySpec, rules []types.ToolRule, e *overrideEdits) {
	for _, nr := range rules {
		next, replaced, applied := narrowToolRule(spec.ToolRules, nr)
		if !applied {
			continue
		}
		spec.ToolRules = next
		e.agent().ToolRules = append(e.agent().ToolRules, nr)
		if replaced != "" {
			e.add(fieldToolRules, nr.Tool+":"+string(replaced), provEffectRemoved)
		}
		e.add(fieldToolRules, nr.Tool+":"+string(nr.Effect), provEffectAdded)
	}
}

// adoOverrideBase is the Azure DevOps capability set an override can narrow: the
// set the policy names, or, where it names none (every governed no-policy run),
// the default profile of the provider row that admits the run's repository,
// resolved the way dispatch resolves it (resolveADOEntraRun). With no such row
// there is no Azure DevOps access to narrow and the override is refused.
func (s *Server) adoOverrideBase(ctx context.Context, r *http.Request, spec types.RunPolicySpec, req *createRunRequest) ([]adoscope.Capability, *runRefusal) {
	if len(spec.AzureDevOpsCapabilities) > 0 {
		return spec.AzureDevOpsCapabilities, nil
	}
	site, err := s.scmLaneSiteConfig(ctx, spec, req.Repo)
	if err != nil {
		return nil, runServerError("get site config", err)
	}
	lane, on := resolveADOEntraRun(site, spec.WorkspaceRepos, req.Repo, adoLaneOwnerOf(r))
	if !on {
		return nil, runError(http.StatusUnprocessableEntity, reasonOverrideRefused,
			"azure_devops.capabilities can only narrow Azure DevOps access this run has, and this run's repository resolves to no per-person Azure DevOps access.")
	}
	return lane.caps, nil
}

// applyADOOverride narrows the run's Azure DevOps capability set to a subset of
// base, the set the run would hold (adoOverrideBase). An empty set keeps nothing,
// and the spec cannot say so: an empty azure_devops_capabilities reads as "the
// provider row's default profile" at dispatch, which would widen the run the
// person meant to narrow. So it is refused here, never folded into the source's
// default.
func applyADOOverride(spec *types.RunPolicySpec, o client.ADOOverrides, base []adoscope.Capability, e *overrideEdits) *runRefusal {
	if len(o.Capabilities) == 0 {
		return overrideInvalid("azure_devops.capabilities: keeping no capability is not something this run can express; name at least one capability to keep")
	}
	var keep []adoscope.Capability
	for _, name := range o.Capabilities {
		c := adoscope.Capability(name)
		if !c.Grantable() {
			return overrideInvalid("azure_devops.capabilities: %q is not a capability a run can hold", name)
		}
		if !slices.Contains(base, c) {
			return runError(http.StatusUnprocessableEntity, reasonOverrideRefused, fmt.Sprintf(
				"An override can only narrow azure_devops.capabilities, and %s is not among the capabilities this run has.", name))
		}
		keep = append(keep, c)
	}
	narrowed := false
	for _, c := range base {
		if !slices.Contains(keep, c) {
			narrowed = true
			e.add(fieldADOCaps, string(c), provEffectNarrowed)
		}
	}
	if narrowed {
		e.applied.AzureDevOps = &o
		spec.AzureDevOpsCapabilities = keep
	}
	return nil
}

// applyGitPATOverrides narrows the git_pat grants of each named forge host: a
// subset of the repositories, write to read, api off. A value that would widen
// (a repository the grant does not name, write, api on) is refused. Every grant
// of the host is narrowed alike, because the proxy holds one scope per host.
func applyGitPATOverrides(spec *types.RunPolicySpec, overrides []client.GitPATOverride, e *overrideEdits) *runRefusal {
	for i, o := range overrides {
		matched, changed := false, false
		for gi, g := range spec.EligibleGrants {
			if g.Kind != types.GrantGitPAT {
				continue
			}
			sc, err := types.DecodeGitPATScope(g.Scope)
			if err != nil || !hostEqual(sc.Host, o.Host) {
				continue
			}
			matched = true
			before := mustJSON(sc) // the normalized scope, so a no-op edit compares equal
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
			if narrowed := mustJSON(sc); !bytes.Equal(narrowed, before) {
				spec.EligibleGrants[gi].Scope = narrowed
				e.add(fieldGrants, grantKey(spec.EligibleGrants[gi]), provEffectNarrowed)
				changed = true
			}
		}
		if !matched {
			return overrideInvalid("git_pat[%d]: this run's policy has no git_pat grant for %s", i, o.Host)
		}
		if changed {
			e.applied.GitPAT = append(e.applied.GitPAT, o)
		}
	}
	return nil
}

// boundOverrideAdds bounds the hosts and grants a person added by their ceiling
// when the policy they started from is not clamped as a whole: the no-policy
// default (which IS the ceiling) and a stored row an unassigned member selected
// (the documented residual in resolveRunPolicyFacts). Their additions must not
// ride on that policy past the ceiling their own inline_policy would meet, so the
// same member pipeline runs on a probe holding only the additions beside the
// policy's hosts, and whatever it drops is dropped from the run. The probe's
// drops are audited on create (dryRun false) only where they are the person's
// own: the probe also carries the policy's hosts, which stay.
func (s *Server) boundOverrideAdds(ctx context.Context, r *http.Request, spec types.RunPolicySpec, e *overrideEdits, ceiling governanceCeiling, dryRun bool) (types.RunPolicySpec, []string, *runRefusal) {
	if e == nil || len(e.addedHosts)+len(e.addedGrants) == 0 && !e.setFirstUse && !e.setAllowAll {
		return spec, nil, nil
	}
	probe := types.RunPolicySpec{AllowedDomains: slices.Clone(spec.AllowedDomains), EligibleGrants: slices.Clone(e.addedGrants),
		FirstUseApproval: spec.FirstUseApproval, AllowAllEgress: spec.AllowAllEgress}
	bounded, warns, drops, refusal := s.boundRunUserSpecDrops(ctx, r, probe, ceiling, "invalid policy: ")
	if refusal != nil {
		return spec, nil, refusal
	}
	if !dryRun {
		s.auditUserPolicyDrops(ctx, r, slices.DeleteFunc(drops, func(d capDrop) bool {
			return d.reason == authz.ReasonCapabilityEgressHost && !slices.ContainsFunc(e.addedHosts, func(h string) bool { return hostEqual(h, d.detail) })
		}))
	}
	// The probe also carries the policy's own hosts, which stay: a warning about one is not the person's.
	warns = slices.DeleteFunc(warns, func(w string) bool {
		return slices.ContainsFunc(spec.AllowedDomains, func(d string) bool {
			return w == privateLiteralDropWarning(d) && !slices.ContainsFunc(e.addedHosts, func(h string) bool { return hostEqual(h, d) })
		})
	})
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

// recordPushRules adds the push-rule edits that took effect (pushRuleSetsFor) to
// the diff, making it when they are the run's only edits. Nothing applied leaves
// the diff as the policy step left it, and a request whose every edit was a no-op
// ends with none.
func (src *policySourceRecord) recordPushRules(applied []client.PushRuleOverride) {
	if len(applied) == 0 {
		return
	}
	diff := client.RunOverridesDiff{}
	if src.OverridesDiff != nil {
		diff = *src.OverridesDiff
	}
	diff.PushRules = applied
	src.OverridesDiff = &diff
}

// overridden reports whether the person changed the policy this run launched
// with: a diff holding only what the ceiling clamped changed nothing, and a
// refusal's attribution must not call the policy theirs for it.
func (src policySourceRecord) overridden() bool {
	return src.OverridesDiff != nil && len(src.OverridesDiff.Pruned().Items()) > 0
}
