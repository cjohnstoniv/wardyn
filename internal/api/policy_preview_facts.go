// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
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
	warnings []string, site types.SiteConfig, choice runProviderChoice) policyPreviewResponse {
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

func previewRepositoryAccess(req createRunRequest, spec types.RunPolicySpec, site types.SiteConfig) []policyPreviewRepository {
	out := []policyPreviewRepository{}
	for _, repo := range presentRepos(append(repoLocatorsOf(spec.WorkspaceRepos), req.Repo, req.DevcontainerRepo)) {
		clone := repoCloneURL(repo)
		target, ok := parseCloneTarget(clone, adoServerHosts(site))
		if !ok {
			continue
		}
		row := admitRepoURL(site, clone).Provider
		kind, org := "other", ""
		switch {
		case row.Kind == types.GitProviderAzureDevOps:
			kind, org = "azure_devops", adoOrgDisplay(row)
		case row.Kind == types.GitProviderGitHub || target.host == "github.com":
			kind = "github"
		case adoServicesHost(target.host):
			kind = "azure_devops"
		}
		group := slices.IndexFunc(out, func(group policyPreviewRepository) bool { return group.Kind == kind && group.Org == org })
		if group < 0 {
			access := policyPreviewRepository{Kind: kind, Org: org, Repos: []string{}, DefaultProfile: []adoscope.Capability{}, CapabilityCeiling: []adoscope.Capability{}}
			if row.Entra != nil {
				access.DefaultProfile = row.Entra.Profile()
				access.CapabilityCeiling = append(access.CapabilityCeiling, row.Entra.CapabilityCeiling...)
			}
			out = append(out, access)
			group = len(out) - 1
		}
		// The parsed target contains neither URL userinfo nor query credentials.
		normalized := "https://" + target.host + "/" + strings.TrimPrefix(target.path, "/")
		if !slices.Contains(out[group].Repos, normalized) {
			out[group].Repos = append(out[group].Repos, normalized)
		}
	}
	return out
}
