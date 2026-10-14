// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// derivedEgress is what one source added to a run's allowlist: the kind create
// audits it under (run.egress.add) and the hosts it added.
type derivedEgress struct {
	kind  string
	added []string
}

// The derived-egress kinds, in the order deriveRunEgress unions them.
const (
	derivedWorkspace      = "workspace"
	derivedWorkspaceClone = "workspace_clone"
	derivedSiteConfig     = "site_config"
	derivedSSH            = "ssh"
	derivedGitPAT         = "git_pat"
)

// deriveRunEgress widens spec with the hosts dispatch adds from the
// deterministic, operator-trusted sources, never the model: each referenced
// workspace's registries and permanent denies, its clone hosts, the
// site-config SCM hosts when the run declares a repository, and the extra hosts
// each git_pat or ssh_key grant's lane needs (grantLaneEgress). It reads
// nothing but its arguments, so the dry doors and create call it alike and
// cannot disagree about which hosts a run gets.
//
// trusted is the run's ceiling allowlist, the authority a member-owned workspace's clone host is
// bounded by (runCloneEgress).
//
// grants are the grant specs whose lanes are wired: create passes the ones
// persistRunGrants and applySSHLaneWarnings kept (wiredLaneGrants); the dry
// doors pass lanesWiredFor's answer, from the same decisions. A github_token
// grant adds no host and only counts toward declaring a repository.
func deriveRunEgress(spec *types.RunPolicySpec, wsRefs []types.Workspace, legacyRepo string,
	scmSite types.SiteConfig, grants []types.GrantSpec, trusted []string,
) []derivedEgress {
	out := []derivedEgress{{kind: derivedWorkspace, added: unionWorkspaceEgress(spec, wsRefs)}}
	var clone []string
	for _, ws := range wsRefs {
		kept, _ := runCloneEgress(ws, trusted)
		clone = append(clone, unionAllowedDomains(spec, kept)...)
	}
	out = append(out, derivedEgress{kind: derivedWorkspaceClone, added: clone})
	// A clone lane only: a sealed local-dir run inherits no SCM host. The
	// legacy repo field counts directly, since a full https:// URL to an
	// enterprise forge is a credential-free clone whose only egress is this.
	declares := len(spec.WorkspaceRepos) > 0 || strings.TrimSpace(legacyRepo) != "" ||
		slices.ContainsFunc(grants, func(g types.GrantSpec) bool {
			return g.Kind == types.GrantGitHubToken || g.Kind == types.GrantGitPAT || g.Kind == types.GrantSSHKey
		})
	if declares {
		out = append(out, derivedEgress{kind: derivedSiteConfig, added: unionSiteConfigScmHosts(spec, scmSite)})
	}
	var ssh, pat []string
	for _, g := range grants {
		switch g.Kind {
		case types.GrantSSHKey:
			ssh = append(ssh, unionAllowedDomains(spec, grantLaneEgress(g))...)
		case types.GrantGitPAT:
			pat = append(pat, unionAllowedDomains(spec, grantLaneEgress(g))...)
		}
	}
	return append(out, derivedEgress{kind: derivedSSH, added: ssh}, derivedEgress{kind: derivedGitPAT, added: pat})
}

// derivedEgressSource is who brought a derived host: for a grant lane's host,
// whoever the fold recorded adding that grant; otherwise, and for the
// site-config SCM hosts, the policy the run started from. The workspace kinds
// add nothing here: stepPreviewEgress unioned and recorded them earlier.
func derivedEgressSource(rec *foldRecorder, grants []types.GrantSpec, kind, host string, policy provSource) provSource {
	if kind != derivedSSH && kind != derivedGitPAT {
		return policy
	}
	for _, g := range grants {
		if !slices.Contains(grantLaneEgress(g), host) {
			continue
		}
		key := grantKey(g)
		for _, row := range rec.rows {
			if row.Field == fieldGrants && row.Value == key && row.Effect == provEffectAdded {
				return row.Source
			}
		}
	}
	return policy
}
