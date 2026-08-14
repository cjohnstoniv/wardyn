// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The gap this closes: the runtime fold degrades SILENTLY for an
// `integration:<id>` naming a row that isn't configured — deliberately, so a
// workspace may state an intent before the integration exists. Preflight is
// exactly where that silence is wrong, because it is the operator asking what
// this run will actually get.

func integWith(fn func(*types.Integration)) *types.Integration {
	i := types.Integration{
		ID: "corp-artifactory", Kind: "artifactory",
		Egress: []string{"artifactory.corp.internal"},
		Secrets: []types.IntegrationSecret{{Role: types.IntegrationCredentialToken, SecretName: "artifactory-token",
			Delivery: &types.IntegrationDelivery{Mode: types.DeliveryProxyHeader, Header: "Authorization"}}},
	}
	if fn != nil {
		fn(&i)
	}
	return &i
}

func TestIntegrationSetupItem(t *testing.T) {
	stored := map[string]bool{"artifactory-token": true}
	cases := []struct {
		name       string
		integ      *types.Integration
		present    map[string]bool
		wantStatus string
		wantDetail string // substring
		wantFix    bool
	}{
		{
			name: "fully wired is satisfied and says what rides", integ: integWith(nil), present: stored,
			wantStatus: "satisfied", wantDetail: "presents artifactory-token in Authorization",
		},
		{
			name: "not configured at all", integ: nil, present: stored,
			wantStatus: "missing", wantDetail: "opens nothing",
		},
		{
			name:       "disabled",
			integ:      integWith(func(i *types.Integration) { i.Disabled = true }),
			present:    stored,
			wantStatus: "missing", wantDetail: "turned off",
		},
		{
			name:       "no hosts opens nothing",
			integ:      integWith(func(i *types.Integration) { i.Egress = nil }),
			present:    stored,
			wantStatus: "missing", wantDetail: "names no hosts",
		},
		{
			// The path DOES open — that is the difference between reaching the
			// system and not reaching it at all — so the detail must not read
			// as though nothing happens.
			name: "secret not stored: path opens, credential does not", integ: integWith(nil), present: nil,
			wantStatus: "missing", wantDetail: "is not in the store", wantFix: true,
		},
		{
			// A system that authenticates outside HTTP is honestly satisfied by
			// the path alone; there is no credential lane to be missing.
			name: "no header lane is satisfied, not a gap",
			integ: integWith(func(i *types.Integration) {
				i.Secrets = nil
			}),
			present:    nil,
			wantStatus: "satisfied", wantDetail: "authenticates outside HTTP",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var integ types.Integration
			if c.integ != nil {
				integ = *c.integ
			}
			it := integrationSetupItem("corp-artifactory", "payments", integ, c.integ != nil, c.present)
			if it.Status != c.wantStatus {
				t.Errorf("status = %q, want %q (detail: %s)", it.Status, c.wantStatus, it.Detail)
			}
			if !strings.Contains(it.Detail, c.wantDetail) {
				t.Errorf("detail = %q, want it to contain %q", it.Detail, c.wantDetail)
			}
			if (it.Fix != nil) != c.wantFix {
				t.Errorf("fix = %+v, want present=%v", it.Fix, c.wantFix)
			}
			// Kind stays config-state, never the credential-absence kind the
			// review panel reserves destructive styling for (decision 4).
			if it.Kind != "workspace_integration" {
				t.Errorf("kind = %q, want workspace_integration", it.Kind)
			}
		})
	}
}

func TestSetupWorkspaceIntegrationItems_OnlyRequired(t *testing.T) {
	srv := New(integrationsTestConfig(t, types.SiteConfig{Integrations: []types.Integration{*integWith(nil)}},
		map[string][]byte{"artifactory-token": []byte("v")}))
	ws := types.Workspace{Name: "payments", Requirements: map[string]types.WorkspaceRequirement{
		"integration:corp-artifactory": {Level: "required", Provenance: "operator_set"},
		// An OPTIONAL integration only applies when a run enables it; rowing
		// every declined one would bury the required set.
		"integration:corp-jira": {Level: "optional", Provenance: "operator_set"},
		"egress:api.stripe.com": {Level: "required", Provenance: "operator_set"},
	}}
	items := srv.setupWorkspaceIntegrationItems(t.Context(), []types.Workspace{ws}, map[string]bool{"artifactory-token": true})
	if len(items) != 1 {
		t.Fatalf("items = %+v, want exactly the one REQUIRED integration", items)
	}
	if items[0].ID != "workspace_integration:corp-artifactory" || items[0].Status != "satisfied" {
		t.Errorf("item = %+v, want the satisfied artifactory row", items[0])
	}
	if items[0].RequiredBy != "workspace payments" {
		t.Errorf("required_by = %q, want the naming workspace", items[0].RequiredBy)
	}
}

// A workspace with no integration requirements adds no rows at all — the
// checklist must not grow for every operator who never used this feature.
func TestSetupWorkspaceIntegrationItems_NoneIsNoRows(t *testing.T) {
	srv := New(integrationsTestConfig(t, types.SiteConfig{}, nil))
	ws := types.Workspace{Name: "payments", Requirements: map[string]types.WorkspaceRequirement{
		"secret:acme-key": {Level: "required", Provenance: "operator_set"},
	}}
	if items := srv.setupWorkspaceIntegrationItems(t.Context(), []types.Workspace{ws}, nil); len(items) != 0 {
		t.Errorf("items = %+v, want none", items)
	}
}

// The silent-degrade case this whole file exists for: the workspace requires an
// integration nobody has configured. The fold opens nothing and audits nothing;
// preflight must still say so.
func TestSetupWorkspaceIntegrationItems_UnconfiguredIsVisible(t *testing.T) {
	srv := New(integrationsTestConfig(t, types.SiteConfig{}, nil))
	ws := types.Workspace{Name: "payments", Requirements: map[string]types.WorkspaceRequirement{
		"integration:not-configured-yet": {Level: "required", Provenance: "operator_set"},
	}}
	items := srv.setupWorkspaceIntegrationItems(t.Context(), []types.Workspace{ws}, nil)
	if len(items) != 1 || items[0].Status != "missing" {
		t.Fatalf("items = %+v, want one missing row", items)
	}
	if !strings.Contains(items[0].Detail, "not-configured-yet") {
		t.Errorf("detail = %q, want it to name the integration", items[0].Detail)
	}
}
