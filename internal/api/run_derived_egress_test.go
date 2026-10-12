// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The hosts the dry doors' fold adds are the hosts create dispatches, lane by
// lane: a workspace's registries and denies, its clone host, an ssh_key's
// endpoint and a git_pat's Azure DevOps bundle. The site-config SCM hosts are
// the one lane create adds that the dry doors withhold: no reader of theirs
// may see the operator's configured host list.
func TestDerivedEgress_PreviewEqualsDispatch(t *testing.T) {
	sshGrant := types.GrantSpec{Kind: types.GrantSSHKey, Scope: mustJSON(map[string]string{"host": "github.com", "key_secret_ref": "deploy-key"})}
	patGrant := gitPATGrant("dev.azure.com", "ado-pat")
	ws := types.Workspace{ID: uuid.New(), Name: "app", ApprovedEgress: []string{"registry.example"}, DeniedEgress: []string{"bad.example"},
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: "https://gitlab.example/team/app"}}}
	sshEndpoint, _ := sshOver443Endpoint("github.com")
	for _, tc := range []struct {
		lane   string
		wsRefs []types.Workspace
		grants []types.GrantSpec
		repo   string
		want   string
	}{
		{"workspace", []types.Workspace{ws}, nil, "", "registry.example"},
		{"clone", []types.Workspace{ws}, nil, "", "gitlab.example"},
		{"site", nil, nil, "https://ghes.corp.example/team/app", "ghes.corp.example"},
		{"ssh", nil, []types.GrantSpec{sshGrant}, "", sshEndpoint},
		{"git_pat", nil, []types.GrantSpec{patGrant}, "", "dev.azure.com"},
	} {
		t.Run(tc.lane, func(t *testing.T) {
			var site types.SiteConfig
			if tc.lane == "site" {
				site.ScmHosts = []string{"ghes.corp.example"}
			}
			base := types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}, EligibleGrants: tc.grants}
			req := createRunRequest{Agent: "claude-code", Repo: tc.repo}
			// The dry doors: the fold's last step.
			f := runFold{spec: base.Clone(), wsRefs: tc.wsRefs, req: &req, scmSite: site}
			rec := &foldRecorder{}
			if !(&Server{}).stepDerivedEgress(&f, rec) {
				t.Fatal("stepDerivedEgress answered")
			}
			// Create: every lane the grants open is wired.
			gw := grantWiring{gitPATGrants: map[string]string{"dev.azure.com": "pat"}, sshGrants: map[string]string{"github.com": "ssh"}}
			dispatched := base.Clone()
			newComponentFixture(t).srv.unionRunEgress(context.Background(), uuid.New(), &dispatched, gw, tc.wsRefs, tc.repo, site, nil, runComponents{})
			if !slices.Contains(dispatched.AllowedDomains, tc.want) {
				t.Fatalf("dispatch lacks %s: %v", tc.want, dispatched.AllowedDomains)
			}
			if tc.lane == "site" {
				if slices.Contains(f.spec.AllowedDomains, tc.want) {
					t.Errorf("the dry doors disclose the configured SCM host: %v", f.spec.AllowedDomains)
				}
				return
			}
			if !slices.Equal(f.spec.AllowedDomains, dispatched.AllowedDomains) || !slices.Equal(f.spec.DeniedDomains, dispatched.DeniedDomains) {
				t.Errorf("preview allows %v denies %v; dispatch allows %v denies %v",
					f.spec.AllowedDomains, f.spec.DeniedDomains, dispatched.AllowedDomains, dispatched.DeniedDomains)
			}
			if !slices.ContainsFunc(rec.rows, func(r provenanceRow) bool { return r.Field == fieldAllowed && r.Value == tc.want }) &&
				tc.lane != "workspace" && tc.lane != "clone" {
				t.Errorf("no provenance row for %s: %+v", tc.want, rec.rows)
			}
		})
	}
}

// A lane create declined to wire adds no host at dispatch, and its grant no
// longer declares a repository for the site-config SCM hosts.
func TestDerivedEgress_VetoedLaneAddsNothingAtDispatch(t *testing.T) {
	spec := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{gitPATGrant("dev.azure.com", "ado-pat")}}
	site := types.SiteConfig{ScmHosts: []string{"ghes.corp.example"}}
	newComponentFixture(t).srv.unionRunEgress(context.Background(), uuid.New(), &spec, grantWiring{}, nil, "", site, nil, runComponents{})
	if len(spec.AllowedDomains) != 0 {
		t.Errorf("a vetoed git_pat lane still dispatched %v", spec.AllowedDomains)
	}
}
