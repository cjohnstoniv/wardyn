// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// builtinGit is a built-in Git provider component the gate admitted: the
// reference as the request gave it and the one grant it becomes.
type builtinGit struct {
	ref   types.ComponentRef
	grant types.GrantSpec
	// canWrite is whether the caller's ceiling would also admit write access,
	// what Review offers when the person asked for read.
	canWrite bool
}

// builtinLabels name a built-in component on its fact and provenance rows.
var builtinLabels = map[string]string{
	types.ComponentBuiltinGitHub:      "GitHub",
	types.ComponentBuiltinAzureDevOps: "Azure DevOps",
}

// builtinRefShape is the part of a built-in reference's shape that reads the
// rest of the list: a GitHub repository is the organisation's own and named
// once, whatever its case, and one provider and organisation is one component.
// "" when it holds.
func builtinRefShape(refs []types.ComponentRef, i int) string {
	ref := refs[i]
	if ref.Builtin == types.ComponentBuiltinGitHub {
		for ri, repo := range ref.Repos {
			if owner, _, _ := strings.Cut(repo, "/"); !strings.EqualFold(owner, ref.Org) {
				return fmt.Sprintf("components[%d]: repos[%d]: %q is not a repository of %s", i, ri, repo, ref.Org)
			}
			if j := slices.IndexFunc(ref.Repos[:ri], func(o string) bool { return strings.EqualFold(o, repo) }); j >= 0 {
				return fmt.Sprintf("components[%d]: repos[%d]: %q is repos[%d] again", i, ri, repo, j)
			}
		}
	}
	if j := slices.IndexFunc(refs[:i], func(o types.ComponentRef) bool {
		return o.Builtin == ref.Builtin && strings.EqualFold(o.Org, ref.Org)
	}); j >= 0 {
		return fmt.Sprintf("components[%d]: the same built-in component as components[%d]", i, j)
	}
	return ""
}

// admitBuiltinComponent takes a built-in Git provider reference through the
// doors a repository already meets on its way into a run, so a built-in
// reaches nothing a workspace of the same repositories could not:
//
//   - admission, then the caller's workspace_provider capability, over each
//     repository (repoAdmissionRefusal, workspaceProvidersRefusal);
//   - the GitHub App lane on the row that admitted it: a built-in GitHub
//     component is brokered on /wardyn/gh/ and never served by the helper, so a
//     row that turned the lane off would leave it nothing;
//   - the deny lists, over the hosts dispatch confines to the broker;
//   - the caller's ceiling for a github_token grant, which bounds the access.
//
// Azure DevOps is refused: dispatch resolves its lane from the run's own
// repositories, which a built-in does not add.
func (s *Server) admitBuiltinComponent(r *http.Request, adm *componentAdmission, i int, ref types.ComponentRef,
) (attachedComponent, *componentRefusal) {
	if ref.Builtin != types.ComponentBuiltinGitHub {
		return attachedComponent{}, &componentRefusal{ordinal: i, source: componentSourceOrg, reason: reasonRequestFieldUnavailable,
			refusal: builtinUnavailable(i, ref.Builtin)}
	}
	// GitHub names are case-insensitive, and the broker mints one grant's
	// repositories under ONE owner spelling (splitRepos): lowercase, the
	// spelling the broker map already keys on.
	ref.Repos = slices.Clone(ref.Repos)
	for ri, repo := range ref.Repos {
		ref.Repos[ri] = strings.ToLower(repo)
	}
	urls := make([]string, len(ref.Repos))
	for ri, repo := range ref.Repos {
		urls[ri] = "https://github.com/" + repo
	}
	if refusal := s.repoAdmissionRefusal(r, urls...); refusal != nil {
		return attachedComponent{}, builtinDoorRefusal(refusal, i)
	}
	if refusal := s.workspaceProvidersRefusal(r, "runs.component", urls...); refusal != nil {
		return attachedComponent{}, builtinDoorRefusal(refusal, i)
	}
	refused := func(reason, msg string) *componentRefusal {
		return &componentRefusal{ordinal: i, source: componentSourceOrg, reason: reason,
			refusal: runError(http.StatusUnprocessableEntity, reason, fmt.Sprintf("components[%d]: %s", i, msg))}
	}
	for _, u := range urls {
		if row := admitRepoURL(adm.site, u).Provider; row.ID != "" && !laneAllowed(row, types.GitLaneApp) {
			return attachedComponent{}, refused(reasonComponentBuiltinLaneOff,
				"your organisation's GitHub provider does not serve repositories through its GitHub App, which a built-in GitHub component needs. Ask your admin.")
		}
	}
	if ceilingDeniesAny(adm.bounds.deny, gitBrokerManagedHosts) {
		return attachedComponent{}, refused(reasonComponentHostDenied, "GitHub is blocked by your organisation.")
	}
	access := cmp.Or(ref.Access, types.ComponentAccessRead)
	grant, ok := s.builtinGitHubGrant(r, adm.ceiling, ref.Repos, access)
	if !ok {
		return attachedComponent{}, refused(reasonComponentBuiltinAccessAboveCeiling,
			fmt.Sprintf("access: your organisation does not allow %s access to GitHub repositories on your runs.", access))
	}
	// The broker's own shape rule, so a grant it would refuse at mint never
	// launches; then the single-lane rule a policy holding both grants meets
	// (validateGrantLaneExclusivity), over the run's grants and this one.
	err := broker.ValidateGitHubScopeShape(grant.Scope)
	if err == nil {
		err = validateGrantLaneExclusivity(append(slices.Clone(adm.grants), grant))
	}
	if err != nil {
		return attachedComponent{}, &componentRefusal{ordinal: i, source: componentSourceOrg, reason: reasonComponentRefInvalid,
			refusal: runError(http.StatusBadRequest, reasonComponentRefInvalid, fmt.Sprintf("components[%d]: %v", i, err))}
	}
	_, canWrite := s.builtinGitHubGrant(r, adm.ceiling, ref.Repos, types.ComponentAccessWrite)
	return attachedComponent{
		source:   componentSourceOrg,
		snapshot: types.RunComponent{Name: builtinLabels[ref.Builtin] + ": " + ref.Org},
		builtin:  &builtinGit{ref: ref, grant: grant, canWrite: canWrite},
	}, nil
}

// builtinUnavailable refuses a built-in no lane serves yet, by name.
func builtinUnavailable(i int, builtin string) *runRefusal {
	return runError(http.StatusUnprocessableEntity, reasonRequestFieldUnavailable, fmt.Sprintf(
		"components[%d]: built-in %s access is not available on this server yet, so the run was not created.", i, builtin))
}

// builtinGitHubGrant is the github_token grant a built-in GitHub component
// becomes: its repositories, and contents at access. For a governed caller it
// is bounded by the ceiling exactly as a policy's own github_token grant is
// (composer.Clamp: approval and TTL tighten), less the ceiling's repository
// list, which bounds what a policy may name and not what a person may reach —
// admission and the provider capability answered that. ok is false when the
// clamp would lower the access or drop the grant: the gate refuses rather than
// launch with less than was asked.
func (s *Server) builtinGitHubGrant(r *http.Request, ceiling governanceCeiling, repos []string, access string) (types.GrantSpec, bool) {
	perms := map[string]string{"contents": access}
	g := types.GrantSpec{Kind: types.GrantGitHubToken, TTLSeconds: 3600, Scope: mustJSON(map[string]any{"permissions": perms})}
	if !s.runUngoverned(r.Context()) {
		clamped, _ := composer.Clamp(types.RunPolicySpec{EligibleGrants: []types.GrantSpec{g}},
			types.RunPolicySpec{EligibleGrants: ceiling.Spec.EligibleGrants}, types.GovernanceLimits{})
		if len(clamped.EligibleGrants) != 1 {
			return types.GrantSpec{}, false
		}
		var got struct {
			Permissions map[string]string `json:"permissions"`
		}
		if json.Unmarshal(clamped.EligibleGrants[0].Scope, &got) != nil || got.Permissions["contents"] != access {
			return types.GrantSpec{}, false
		}
		g = clamped.EligibleGrants[0]
	}
	// builtin marks the grant as this component's: the record a revive and an
	// extension read its repositories back from (builtinGitHubRepos). The
	// broker's decode ignores the key.
	g.Scope = mustJSON(map[string]any{"repos": repos, "permissions": perms, "builtin": types.ComponentBuiltinGitHub})
	return g, true
}

// builtinGitHubRepos is the repositories of a run's built-in GitHub
// components, as clone URLs, read from the grants their admission wrote. A
// policy's own github_token grant carries no builtin key and is not read, so
// its repositories meet no provider door they did not meet at launch.
func builtinGitHubRepos(grants []types.GrantSpec) []string {
	var out []string
	for _, g := range grants {
		if isBuiltinGitHubGrant(g) {
			for _, repo := range githubScopeRepos(g.Scope) {
				out = append(out, "https://github.com/"+repo)
			}
		}
	}
	return out
}

// isBuiltinGitHubGrant reports whether g is the grant a built-in GitHub
// component's admission wrote.
func isBuiltinGitHubGrant(g types.GrantSpec) bool {
	var sc struct {
		Builtin string `json:"builtin"`
	}
	return g.Kind == types.GrantGitHubToken && json.Unmarshal(g.Scope, &sc) == nil && sc.Builtin == types.ComponentBuiltinGitHub
}

// builtinGitHubRemotes is the clone URLs of the request's built-in GitHub
// components, the remotes their push rules are keyed by.
func builtinGitHubRemotes(refs []types.ComponentRef) []string {
	var out []string
	for _, ref := range refs {
		if ref.Builtin != types.ComponentBuiltinGitHub {
			continue
		}
		for _, repo := range ref.Repos {
			out = append(out, "https://github.com/"+repo)
		}
	}
	return out
}

// builtinDoorRefusal wraps a repository door's refusal with what the
// component's audit row records.
func builtinDoorRefusal(refusal *runRefusal, i int) *componentRefusal {
	out := componentDoorRefusal(refusal, i, componentSourceOrg, nil)
	if out.reason == "" {
		out.reason = refusal.body.Reason
	}
	return out
}

// auditEntry describes the built-in for the run's audit rows: the organisation's
// provider and the repositories it reaches there, which name no one's content.
func (b builtinGit) auditEntry(ordinal int) map[string]any {
	return map[string]any{
		"ordinal": ordinal, "kind": types.ComponentGitProvider, "source": componentSourceOrg,
		"provider": b.ref.Builtin, "org": b.ref.Org, "repos": b.ref.Repos, "access": cmp.Or(b.ref.Access, types.ComponentAccessRead),
	}
}

// fact is the built-in's row in Review and the preview: a Git provider on the
// app lane, its repositories and their access, and the push rules the git
// route holds each one to.
func (b builtinGit) fact(ordinal int, name string, f componentFactInputs) componentFact {
	fact := componentFact{
		Kind: types.ComponentGitProvider, Provider: b.ref.Builtin, ID: "inline:" + strconv.Itoa(ordinal), Name: name,
		Reason: componentSourceOrg, Status: componentUnknown, Requirements: []SetupItem{},
		Lane: string(types.GitLaneApp), Org: b.ref.Org,
	}
	for _, repo := range b.ref.Repos {
		url := "https://github.com/" + repo
		fact.Repos = append(fact.Repos, url)
		appendRepoAccess(&fact, url, b.ref.Access == types.ComponentAccessWrite, b.canWrite)
		if fact.PushRules == nil {
			fact.PushRules = gitFactPushRules(f, previewRepo{locator: url})
		}
	}
	return fact
}
