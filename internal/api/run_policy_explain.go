// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/hostrules"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The closed set of runPolicyChange.Cause values, in the order the changes are
// listed. The order is the cause table's: launch-time additions first, the
// generic cause last.
const (
	causeWorkspace     = "workspace"
	causeSourceControl = "source_control"
	causeMirror        = "mirror"
	causeModelAccess   = "model_access"
	causeGitBroker     = "git_broker"
	causeProfile       = "profile"
	causeOrgDisk       = "org_disk"
	causeRestart       = "restart"
	causeLimits        = "limits"
	causeLaunch        = "launch"
)

var causeOrder = []string{
	causeWorkspace, causeSourceControl, causeMirror, causeModelAccess, causeGitBroker,
	causeProfile, causeOrgDisk, causeRestart, causeLimits, causeLaunch,
}

// The RunPolicySpec fields a change can name, as their JSON names. The list
// fields hold set entries; the rest are scalars, held as a one-entry set so the
// same diff serves both.
const (
	fieldAllowed   = "allowed_domains"
	fieldDenied    = "denied_domains"
	fieldGrants    = "eligible_grants"
	fieldMounts    = "workspace_mounts"
	fieldRepos     = "workspace_repos"
	fieldUIApps    = "ui_apps"
	fieldToolRules = "tool_rules"
	fieldADOCaps   = "azure_devops_capabilities"
	fieldMethods   = "allowed_methods"
	fieldMinCC     = "min_confinement_class"
	fieldDisk      = "resources.disk_mib"
)

var policyFieldOrder = []string{
	fieldAllowed, fieldDenied, "allow_all_egress", "first_use_approval", "first_use_hold_seconds", "max_holds",
	fieldMethods, fieldMinCC, fieldGrants, "auto_stop_after_sec", fieldMounts, fieldRepos, fieldUIApps,
	"resources.cpu_millis", "resources.memory_mib", "resources.pids_limit", fieldDisk,
	fieldToolRules, "git_push_any_branch", fieldADOCaps,
}

// The fields the member bound (boundUserSpec and the disk and ui_apps halves)
// can narrow: a removal or change in one of these, on a bound run, is cause
// "limits". The scalars are the two that hold a single value.
var (
	limitsFields = map[string]bool{
		fieldAllowed: true, fieldDenied: true, fieldGrants: true, fieldMinCC: true, fieldMounts: true, fieldRepos: true,
		fieldUIApps: true, fieldADOCaps: true, "resources.cpu_millis": true, "resources.memory_mib": true,
		"resources.pids_limit": true, fieldDisk: true,
	}
	scalarFields = map[string]bool{
		fieldMinCC: true, "resources.cpu_millis": true, "resources.memory_mib": true,
		"resources.pids_limit": true, fieldDisk: true,
	}
)

// auditEvidence is what the run's own audit rows say about how launch changed
// its policy. Every set holds entries exactly as they were written into the
// spec, so membership is an exact match.
type auditEvidence struct {
	workspace, sourceControl, mirror, model, confine, profileDenied map[string]bool
	mirrorHosts                                                     map[string]bool // bare hosts of successful mirror rows
	profile                                                         string
	profileMaxDisk                                                  int
	restart                                                         map[string]time.Time
	restartHosts                                                    []string // restart denies in the order they were added
	clamp                                                           []string
	bounded                                                         bool // the member bound applied (policy_source.bounded)
	diskFilled                                                      bool // disk_mib_filled: the size came from the org default
	legacyGitBroker                                                 bool // no confine row exists and the run holds a github grant
}

// explainRunPolicy attributes each difference between the policy a run started
// from (base, nil for an older run) and the one it got (resolved) to one of
// the closed causes. Evidence rows decide first, derivations second, and
// "launch" is the generic remainder. Without a base only what the evidence
// rows state can be said, limited to entries present in resolved.
func explainRunPolicy(base *types.RunPolicySpec, resolved types.RunPolicySpec, ev auditEvidence, sc types.SiteConfig, run types.AgentRun) []runPolicyChange {
	x := &explainer{ev: ev, sc: sc, resolvedDisk: diskMiB(resolved)}
	from := base
	if from == nil {
		b := x.evidenceBase(resolved)
		from = &b
	}
	old, cur := policyFieldSets(*from), policyFieldSets(resolved)
	x.derive(run, setDiff(cur[fieldAllowed], old[fieldAllowed]))

	groups := map[changeKey]*runPolicyChange{}
	put := func(field, entry string, added bool) {
		cause, profile, at := x.cause(field, entry, added)
		k := changeKey{cause: cause, field: field, profile: profile}
		if at != nil {
			k.at = *at
		}
		c := groups[k]
		if c == nil {
			c = &runPolicyChange{Cause: cause, Field: field, Profile: profile, At: at}
			groups[k] = c
		}
		if added {
			c.Added = append(c.Added, entry)
		} else {
			c.Removed = append(c.Removed, entry)
		}
	}
	for _, field := range policyFieldOrder {
		for _, e := range setDiff(cur[field], old[field]) {
			put(field, e, true)
		}
		for _, e := range setDiff(old[field], cur[field]) {
			put(field, e, false)
		}
	}

	out := make([]runPolicyChange, 0, len(groups))
	for _, c := range groups {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ca, cb := slices.Index(causeOrder, a.Cause), slices.Index(causeOrder, b.Cause); ca != cb {
			return ca < cb
		}
		if fa, fb := slices.Index(policyFieldOrder, a.Field), slices.Index(policyFieldOrder, b.Field); fa != fb {
			return fa < fb
		}
		return a.At != nil && b.At != nil && a.At.Before(*b.At)
	})
	for i := range out {
		if out[i].Cause == causeLimits {
			out[i].Detail = slices.Clone(ev.clamp) // once: the first limits change carries the sentences
			break
		}
	}
	return out
}

type changeKey struct {
	cause, field, profile string
	at                    time.Time
}

type explainer struct {
	ev           auditEvidence
	sc           types.SiteConfig
	resolvedDisk int
	providerHost map[string]bool // rule 9
	adoEntries   map[string]bool // rule 10
}

// evidenceBase is the base an older run is diffed against: the resolved policy
// minus every entry the evidence rows claim, so that only claimed entries show
// up as changes and the rest is silence rather than a guess.
func (x *explainer) evidenceBase(resolved types.RunPolicySpec) types.RunPolicySpec {
	b := resolved.Clone()
	claimed := func(field string, entry string) bool {
		switch field {
		case fieldAllowed:
			ev := x.ev
			return ev.workspace[entry] || ev.sourceControl[entry] || ev.mirror[entry] || ev.model[entry]
		default:
			_, restart := x.ev.restart[entry]
			return x.ev.confine[entry] || x.ev.profileDenied[entry] || restart || x.legacyGit(entry)
		}
	}
	b.AllowedDomains = slices.DeleteFunc(b.AllowedDomains, func(e string) bool { return claimed(fieldAllowed, e) })
	b.DeniedDomains = slices.DeleteFunc(b.DeniedDomains, func(e string) bool { return claimed(fieldDenied, e) })
	if b.Resources != nil && (x.ev.diskFilled || x.profileClampedDisk()) {
		r := *b.Resources
		r.DiskMiB = 0
		b.Resources = &r
	}
	return b
}

// legacyGit is rule 12: an older run's broker-managed host, when it held a
// github grant (confineGitBrokerEgress is a no-op without one).
func (x *explainer) legacyGit(entry string) bool {
	return x.ev.legacyGitBroker && (gitBrokerManaged(entry) || slices.Contains(gitBrokerSSHHosts(), egressEntryHost(entry)))
}

func (x *explainer) profileClampedDisk() bool {
	return x.ev.profileMaxDisk > 0 && x.resolvedDisk == x.ev.profileMaxDisk
}

// derive fills the two derivation sets from the entries launch added to the
// allowlist: the chosen model provider's key host (rule 9) and the Azure DevOps
// Entra hosts of every organisation whose <org>.visualstudio.com entry is among
// them (rule 10).
func (x *explainer) derive(run types.AgentRun, added []string) {
	x.providerHost, x.adoEntries = map[string]bool{}, map[string]bool{}
	if run.ModelProviderID != "" {
		if p, ok := modelProviderByID(x.sc.ModelProviders, run.ModelProviderID); ok {
			if lane, ok := providerKeyLaneFor(p, run.Agent); ok {
				x.providerHost[lane.host] = true
			}
		}
	}
	for _, e := range added {
		host, port, err := net.SplitHostPort(e)
		org, isOrgHost := strings.CutSuffix(host, ".visualstudio.com")
		if err != nil || port != adoEntraHostPort || !isOrgHost || org == "" || strings.Contains(org, ".") {
			continue
		}
		for _, entry := range adoEntraEgressEntries(org) {
			if slices.Contains(added, entry) {
				x.adoEntries[entry] = true
			}
		}
	}
}

// cause is the rule table: the first rule that names the entry wins.
func (x *explainer) cause(field, entry string, added bool) (cause, profile string, at *time.Time) {
	ev := x.ev
	allowAdded, denyAdded := field == fieldAllowed && added, field == fieldDenied && added
	switch {
	case allowAdded && ev.workspace[entry]:
		return causeWorkspace, "", nil
	case allowAdded && ev.sourceControl[entry]:
		return causeSourceControl, "", nil
	case x.mirrored(field, entry, added):
		return causeMirror, "", nil
	case allowAdded && ev.model[entry]:
		return causeModelAccess, "", nil
	case ev.confine[entry] && (denyAdded || field == fieldAllowed && !added):
		return causeGitBroker, "", nil
	case denyAdded && ev.profileDenied[entry], field == fieldDisk && x.profileClampedDisk():
		return causeProfile, ev.profile, nil
	case field == fieldDisk && ev.diskFilled:
		return causeOrgDisk, "", nil
	case denyAdded && !ev.restart[entry].IsZero():
		t := ev.restart[entry]
		return causeRestart, "", &t
	case allowAdded && x.providerHost[entry]:
		return causeModelAccess, "", nil
	case allowAdded && x.adoEntries[entry]:
		return causeSourceControl, "", nil
	case ev.bounded && limitsFields[field] && (scalarFields[field] || !added || field == fieldDenied):
		return causeLimits, "", nil
	case (denyAdded || field == fieldAllowed && !added) && x.legacyGit(entry):
		return causeGitBroker, "", nil
	}
	return causeLaunch, "", nil
}

// mirrored is rule 3: the mirror host itself, the public hosts a mirror
// replaced (only for a redirect whose To a mirror row names), and the deny a
// network-only redirect adds on its From host.
func (x *explainer) mirrored(field, entry string, added bool) bool {
	switch {
	case field == fieldAllowed && added:
		return x.ev.mirror[entry]
	case field == fieldAllowed:
		for _, r := range x.sc.EgressRedirects {
			if !x.ev.mirrorHosts[strings.ToLower(hostrules.HostOf(r.To))] {
				continue
			}
			pub := map[string]bool{}
			for _, h := range redirectPublicHosts(r) {
				pub[strings.ToLower(h)] = true
			}
			if entryCoversAny(entry, pub) {
				return true
			}
		}
	case field == fieldDenied && added:
		for _, r := range x.sc.EgressRedirects {
			if r.Ecosystem == "" && strings.ToLower(hostrules.HostOf(r.From)) == entry {
				return true
			}
		}
	}
	return false
}

// policyFieldSets is a spec's comparable content: each list field as its
// redaction-safe entry keys, each scalar as a one-entry set (none when unset).
// A grant is "kind:host" or "kind:repo,repo", a mount its target, a repo
// "repo@ref". Nothing here reads a mount source or a secret name, so the same
// keys come out of a spec and of its redacted copy.
func policyFieldSets(sp types.RunPolicySpec) map[string][]string {
	m := map[string][]string{
		fieldAllowed: sp.AllowedDomains, fieldDenied: sp.DeniedDomains, fieldMethods: sp.AllowedMethods,
	}
	scalar := func(field, v string) {
		if v != "" && v != "0" && v != "false" {
			m[field] = []string{v}
		}
	}
	scalar("allow_all_egress", strconv.FormatBool(sp.AllowAllEgress))
	scalar("first_use_approval", string(sp.FirstUseApproval))
	scalar("first_use_hold_seconds", strconv.Itoa(sp.FirstUseHoldSeconds))
	scalar("max_holds", strconv.Itoa(sp.MaxHolds))
	scalar(fieldMinCC, string(sp.MinConfinementClass))
	scalar("auto_stop_after_sec", strconv.Itoa(sp.AutoStopAfterSec))
	scalar("git_push_any_branch", strconv.FormatBool(sp.GitPushAnyBranch))
	if r := sp.Resources; r != nil {
		scalar("resources.cpu_millis", strconv.Itoa(r.CPUMillis))
		scalar("resources.memory_mib", strconv.Itoa(r.MemoryMiB))
		scalar("resources.pids_limit", strconv.Itoa(r.PidsLimit))
		scalar(fieldDisk, strconv.Itoa(r.DiskMiB))
	}
	for _, g := range sp.EligibleGrants {
		m[fieldGrants] = append(m[fieldGrants], grantKey(g))
	}
	for _, mnt := range sp.WorkspaceMounts {
		m[fieldMounts] = append(m[fieldMounts], mnt.Target)
	}
	for _, r := range sp.WorkspaceRepos {
		key := r.Repo
		if r.Ref != "" {
			key += "@" + r.Ref
		}
		m[fieldRepos] = append(m[fieldRepos], key)
	}
	for _, a := range sp.UIApps {
		m[fieldUIApps] = append(m[fieldUIApps], a.Name)
	}
	for _, t := range sp.ToolRules {
		m[fieldToolRules] = append(m[fieldToolRules], t.Tool+":"+string(t.Effect))
	}
	for _, c := range sp.AzureDevOpsCapabilities {
		m[fieldADOCaps] = append(m[fieldADOCaps], string(c))
	}
	return m
}

// grantKey names a grant by its kind and the one non-secret thing that says
// what it reaches: a host, or the repos.
func grantKey(g types.GrantSpec) string {
	var scope struct {
		Host  string   `json:"host"`
		Repos []string `json:"repos"`
	}
	_ = json.Unmarshal(g.Scope, &scope) // an unreadable scope is just the bare kind
	detail := scope.Host
	if detail == "" {
		detail = strings.Join(scope.Repos, ",")
	}
	if detail == "" {
		return string(g.Kind)
	}
	return string(g.Kind) + ":" + detail
}

func diskMiB(sp types.RunPolicySpec) int {
	if sp.Resources == nil {
		return 0
	}
	return sp.Resources.DiskMiB
}

// setDiff is the entries of a not in b, in a's order, once each.
func setDiff(a, b []string) []string {
	var out []string
	for _, e := range a {
		if !slices.Contains(b, e) && !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	return out
}
