// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// TestDeriveSetupItems_AMemberIsNotShownTheOperatorsSecretNames pins #2423: a member's checklist
// names no secret only the operator holds, judges a workspace's secret against the member's own
// namespace, and shows an admin's workspace no further than the member reads it on the workspace.
func TestDeriveSetupItems_AMemberIsNotShownTheOperatorsSecretNames(t *testing.T) {
	const member = "sub-setup-member"
	ctx := context.Background()
	sec := &memSecrets{m: map[string][]byte{"jira-token": []byte("v"), "probe-name": []byte("v"), "acme-prod-db-password": []byte("v")}}
	if err := sec.For(member).Put(ctx, "mine", []byte("v")); err != nil {
		t.Fatal(err)
	}
	admins := needsWorkspace("ops", "/ops", workspacescan.WorkspaceProfile{
		RequiredSecrets:    []workspacescan.SecretNeed{{Name: "OPS_ONLY_SECRET"}},
		SecretFilesPresent: []string{".env.production"},
		SuggestedEgress:    []string{"internal-registry.example"},
	})
	admins.Requirements = map[string]types.WorkspaceRequirement{"secret:acme-prod-db-password": {Level: "required"}}
	own := needsWorkspace("mine", "/mine", workspacescan.WorkspaceProfile{RequiredSecrets: []workspacescan.SecretNeed{{Name: "PROBE_NAME"}}})
	own.OwnedBy = member
	srv := newSetupTestServer(admins, own)
	srv.cfg.Secrets = sec
	spec := types.RunPolicySpec{
		EligibleGrants: []types.GrantSpec{apiKeyGrant("jira.example", "jira-token"), apiKeyGrant("own.example", "mine"), gitPATGrant("git.example", "jira-token")},
		WorkspaceMounts: []types.WorkspaceMount{{Source: "/ops", Target: "/home/agent/ops"}, {Source: "/mine", Target: "/home/agent/work"}},
	}
	run := composer.RunInput{Agent: "claude-code", Repo: "ephemeral"}
	mctx := memberCtxFor(member)

	items := srv.deriveSetupItems(mctx, member, run, spec, srv.presentSecretNamesFor(mctx, member), nil)

	raw := string(mustJSON(items))
	for _, leak := range []string{"jira-token", "acme-prod-db-password", "OPS_ONLY_SECRET", "ops-only-secret", ".env.production", "internal-registry.example"} {
		if strings.Contains(raw, leak) {
			t.Errorf("a member's checklist carries %q: %s", leak, raw)
		}
	}
	if it, ok := findItem(items, "secret:provided:jira.example"); !ok || it.Status != "satisfied" || it.Fix != nil {
		t.Errorf("the operator's api_key secret = %+v (%v), want a satisfied nameless row", it, ok)
	}
	if it, ok := findItem(items, "secret:mine"); !ok || it.Status != "satisfied" {
		t.Errorf("the member's own secret = %+v (%v), want it named and satisfied", it, ok)
	}
	if it, ok := findItem(items, "workspace_secret:probe-name"); !ok || it.Status != "missing" {
		t.Errorf("a name only the operator stores, asked by the member's workspace = %+v (%v), want missing", it, ok)
	}
	if it, ok := findItem(items, "repo_credential:git_pat:git.example"); !ok || it.Status != "satisfied" || it.Fix != nil {
		t.Errorf("the operator's git_pat secret = %+v (%v), want satisfied with no named fix", it, ok)
	}
}
