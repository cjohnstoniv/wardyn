// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type policyPreviewResponse struct {
	Spec             types.RunPolicySpec       `json:"spec"`
	Source           policyPreviewSource       `json:"source"`
	Provisional      bool                      `json:"provisional"`
	Redacted         bool                      `json:"redacted"`
	Warnings         []string                  `json:"warnings"`
	Pending          []policyPreviewPending    `json:"pending"`
	RepositoryAccess []policyPreviewRepository `json:"repository_access"`
	// Components is what the draft is given access to (componentFacts): absent
	// for a draft with no repository on a Git provider and no component.
	Components []componentFact `json:"components,omitempty"`
	// Provenance says why each entry of the spec is there (runFold.prov); never null.
	Provenance []provenanceRow `json:"provenance"`
}

type policyPreviewPending string

const (
	previewTask          policyPreviewPending = "task"
	previewProvider      policyPreviewPending = "model_provider_selection"
	previewCredential    policyPreviewPending = "credential_liveness"
	previewAutonomy      policyPreviewPending = "autonomy"
	previewToolApprovals policyPreviewPending = "tool_approvals"
	previewConfinement   policyPreviewPending = "runner_confinement"
	previewDrive         policyPreviewPending = "drive_readiness"
	previewEgress        policyPreviewPending = "dispatch_egress"
)

type policyPreviewSource struct {
	Kind     string     `json:"kind"`
	Name     string     `json:"name,omitempty"`
	PolicyID *uuid.UUID `json:"policy_id,omitempty"`
}

type policyPreviewRepository struct {
	Kind              string                `json:"kind"`
	Repos             []string              `json:"repos"`
	Org               string                `json:"org,omitempty"`
	DefaultProfile    []adoscope.Capability `json:"default_profile"`
	CapabilityCeiling []adoscope.Capability `json:"capability_ceiling"`
}

func policyPreviewFacts(req createRunRequest, spec types.RunPolicySpec, source policySourceRecord,
	warnings []string, site types.SiteConfig, choice runProviderChoice, comps runComponents) policyPreviewResponse {
	out := redactSpecForRead(spec, false)
	if out.LLMInspection != nil {
		inspection := *out.LLMInspection
		inspection.WorkspaceSecretValues = nil
		out.LLMInspection = &inspection
	}
	redacted := !reflect.DeepEqual(specJSON(spec), specJSON(out))
	if out.AllowedDomains == nil {
		out.AllowedDomains = []string{}
	}
	return policyPreviewResponse{
		Spec: out, Source: policyPreviewSource{Kind: source.Kind, Name: source.Name, PolicyID: source.PolicyID},
		Provisional: true, Redacted: redacted,
		Warnings: previewSafeWarnings(warnings), Pending: previewPending(req, choice),
		RepositoryAccess: previewRepositoryAccess(req, out, site),
		Components:       componentFacts(req, spec, site, comps, nil),
		Provenance:       []provenanceRow{},
	}
}

// Whitelist whole messages whose substitutions are counts or closed enums.
// Clamp warnings that interpolate resource names never cross this read door.
var previewSafeWarning = regexp.MustCompile(`^(confinement raised from "(CC[1-3]|)" to operator minimum "CC[1-3]"|` +
	`first_use_approval raised from "(always_deny|deny_with_review|wait_for_review)" to operator minimum "(always_deny|deny_with_review|wait_for_review)"|` +
	`auto_stop_after_sec capped to operator maximum [0-9]+s|` +
	`dropped [0-9]+ proposed workspace mount\(s\): host mounts are operator-authored, never composer-proposed|` +
	`github grant: dropped [0-9]+ repo\(s\) outside operator scope|` +
	`push_rules\.(max_inspect_pack_mib|max_file_size_mib) capped to operator maximum [0-9]+)$`)

func previewSafeWarnings(warnings []string) []string {
	out := []string{}
	for _, warning := range warnings {
		if warning == composer.WarnResourcesCapped || warning == "push_rules inherited from the operator's policy" ||
			warning == "allow_all_egress disabled: operator policy does not permit allow-all egress" ||
			warning == "git_push_any_branch disabled: operator policy keeps push branch-namespace confinement on" || previewSafeWarning.MatchString(warning) {
			out = append(out, warning)
		}
	}
	return out
}

// previewRepo is one of a draft's repositories as the read doors describe it:
// the provider kind it classifies to, the address its row clones from, its own
// address without credentials, and the admission verdict that classified it.
type previewRepo struct {
	kind, org, url string
	locator, host  string // as the draft names it; the host git will dial
	verdict        providerVerdict
}

// previewRepos classifies every repository the draft names, in the order it
// names them. One that cannot be read is left out: admission refuses it.
func previewRepos(req createRunRequest, spec types.RunPolicySpec, site types.SiteConfig) []previewRepo {
	var out []previewRepo
	for _, repo := range presentRepos(append(repoLocatorsOf(spec.WorkspaceRepos), req.Repo, req.DevcontainerRepo)) {
		clone := repoCloneURL(repo)
		target, ok := parseCloneTarget(clone, adoServerHosts(site))
		if !ok {
			continue
		}
		if target.ssh {
			target.host = canonicalProviderHost(strings.TrimSuffix(target.host, "."))
		}
		normalized := previewRepositoryURL(clone, target)
		if normalized == "" {
			continue
		}
		verdict := admitRepoURL(site, clone)
		row := verdict.Provider
		kind, org := "other", ""
		switch {
		case row.Kind == types.GitProviderAzureDevOps:
			kind, org = "azure_devops", adoOrgDisplay(row)
		case row.Kind == types.GitProviderGitHub || target.host == "github.com":
			kind = "github"
		case adoServicesHost(target.host):
			kind = "azure_devops"
		}
		out = append(out, previewRepo{kind: kind, org: org, url: normalized, locator: repo, host: target.host, verdict: verdict})
	}
	return out
}

func previewRepositoryAccess(req createRunRequest, spec types.RunPolicySpec, site types.SiteConfig) []policyPreviewRepository {
	out := []policyPreviewRepository{}
	for _, repo := range previewRepos(req, spec, site) {
		row, normalized := repo.verdict.Provider, repo.url
		access := policyPreviewRepository{Kind: repo.kind, Org: repo.org, Repos: []string{}, DefaultProfile: []adoscope.Capability{}, CapabilityCeiling: []adoscope.Capability{}}
		if row.Entra != nil {
			access.DefaultProfile = row.Entra.Profile()
			slices.Sort(access.DefaultProfile)
			access.DefaultProfile = slices.Compact(access.DefaultProfile)
			access.CapabilityCeiling = append(access.CapabilityCeiling, row.Entra.CapabilityCeiling...)
			slices.Sort(access.CapabilityCeiling)
			access.CapabilityCeiling = slices.Compact(access.CapabilityCeiling)
		}
		group := slices.IndexFunc(out, func(group policyPreviewRepository) bool {
			return group.Kind == access.Kind && group.Org == access.Org &&
				slices.Equal(group.DefaultProfile, access.DefaultProfile) && slices.Equal(group.CapabilityCeiling, access.CapabilityCeiling)
		})
		if group < 0 {
			out = append(out, access)
			group = len(out) - 1
		}
		if !slices.Contains(out[group].Repos, normalized) {
			out[group].Repos = append(out[group].Repos, normalized)
		}
	}
	return out
}

// Admission deliberately ignores SSH paths; display keeps the full repository
// identity without userinfo, query strings or fragments from the clone URL.
func previewRepositoryURL(clone string, target cloneTarget) string {
	if target.ssh && !strings.Contains(clone, "://") {
		_, address, _ := strings.Cut(clone, "@")
		_, path, _ := strings.Cut(address, ":")
		clone = "ssh://" + target.host + "/" + strings.TrimPrefix(path, "/")
	}
	u, err := url.Parse(clone)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Host)
	if target.ssh {
		host = target.host
	}
	return strings.ToLower(u.Scheme) + "://" + host + strings.TrimSuffix(u.EscapedPath(), "/")
}
