// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The two runner_resident opt-ins ride the existing super-admin ADO row write:
// the sandbox opt-in needs the keystore one, an own_pat row cannot opt in, and
// the write's audit row names the rows that did.
func TestADOResident_SandboxOptInRequiresTheFirst(t *testing.T) {
	row := func(mutate func(*types.ADOEntraConfig)) string {
		raw, _ := json.Marshal(entraRow(func(r *types.GitProvider) { mutate(r.Entra) }))
		return `{"git":[` + string(raw) + `]}`
	}
	for name, body := range map[string]string{
		"sandbox without runner_resident": row(func(c *types.ADOEntraConfig) { c.RunnerResidentSandbox = true }),
		"own_pat row":                     row(func(c *types.ADOEntraConfig) { c.TokenMode, c.RunnerResident = types.ADOTokenModeOwnPAT, true }),
	} {
		fake := &fakeProvidersStore{fakeSiteConfigStore: &fakeSiteConfigStore{}}
		srv, _ := newProvidersHarness(t, fake)
		if w := do(t, srv, http.MethodPut, "/api/v1/workspace-providers", adminToken, body); w.Code != http.StatusBadRequest || fake.putSeen != nil ||
			!strings.Contains(w.Body.String(), "runner_resident") {
			t.Fatalf("%s: PUT = %d %s written=%v, want 400 and nothing stored", name, w.Code, w.Body.String(), fake.putSeen != nil)
		}
	}
	fake := &fakeProvidersStore{fakeSiteConfigStore: &fakeSiteConfigStore{}}
	srv, audit := newProvidersHarness(t, fake)
	w := do(t, srv, http.MethodPut, "/api/v1/workspace-providers", adminToken, row(func(c *types.ADOEntraConfig) { c.RunnerResident, c.RunnerResidentSandbox = true, true }))
	if w.Code != http.StatusOK || fake.putSeen == nil {
		t.Fatalf("both opt-ins: PUT = %d %s", w.Code, w.Body.String())
	}
	var data struct {
		Resident []string `json:"runner_resident"`
		Sandbox  []string `json:"runner_resident_sandbox"`
	}
	audit.mu.Lock()
	defer audit.mu.Unlock()
	i := slices.IndexFunc(audit.events, func(ev types.AuditEvent) bool { return ev.Action == "workspace_provider.write" })
	if i < 0 || json.Unmarshal(audit.events[i].Data, &data) != nil || !slices.Equal(data.Resident, []string{"ado"}) || !slices.Equal(data.Sandbox, []string{"ado"}) {
		t.Fatalf("workspace_provider.write data = %+v", data)
	}

	// PUT /site-config writes the same block, so its audit row names the rows too.
	fake = &fakeProvidersStore{fakeSiteConfigStore: &fakeSiteConfigStore{}}
	srv, siteAudit := newProvidersHarness(t, fake)
	both := row(func(c *types.ADOEntraConfig) { c.RunnerResident, c.RunnerResidentSandbox = true, true })
	if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, `{"workspace_providers":`+both+`}`); w.Code != http.StatusOK {
		t.Fatalf("site-config PUT = %d %s", w.Code, w.Body.String())
	}
	siteAudit.mu.Lock()
	defer siteAudit.mu.Unlock()
	data.Resident, data.Sandbox = nil, nil
	i = slices.IndexFunc(siteAudit.events, func(ev types.AuditEvent) bool { return ev.Action == "site_config.write" })
	if i < 0 || json.Unmarshal(siteAudit.events[i].Data, &data) != nil || !slices.Equal(data.Resident, []string{"ado"}) || !slices.Equal(data.Sandbox, []string{"ado"}) {
		t.Fatalf("site_config.write data = %+v", data)
	}
}

// The opt-in is the admitting connection's, never the deployment's: another
// row's opt-in (here a disabled one listed first) leaves this run refused.
func TestADOResident_OptInIsPerConnection(t *testing.T) {
	const repo = "https://dev.azure.com/acme/proj/_git/app"
	other := entraRow(func(r *types.GitProvider) {
		r.ID, r.BaseURLs, r.Disabled, r.Entra.RunnerResident = "other", []string{"https://dev.azure.com/other"}, true, true
	})
	this := entraRow(nil)
	sc := types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{Git: []types.GitProvider{other, this}}}
	admitted := func() *placement.Refusal {
		t.Helper()
		a, ok := adoEntraRunForRepo(sc, repo, "alice")
		if !ok {
			t.Fatal("the row does not admit its own repository")
		}
		_, r := placement.LocalEligibility(placement.LocalPlan{
			CredentialIntents: []placement.CredentialIntent{{Field: "ProxyConfig.ADOGrant[" + a.rowID + "]", Origin: (&Server{}).localADOOrigin(context.Background(), "alice", a)}},
			Delivery:          placement.DeliveryPolicy{Classes: map[string]placement.ClassPolicy{placement.ClassADOMintedPAT: {Mode: placement.ModeRunnerResident}}},
			Posture:           &types.RunnerPosture{OS: "linux"},
		})
		return r
	}
	if r := admitted(); r == nil || r.Reason != placement.ReasonPlacementCredential || !strings.Contains(r.Detail, "opted in") {
		t.Fatalf("another connection's opt-in: %+v, want this run refused for its own opt-in", r)
	}
	// Opted in, the run passes the opt-in gate; the deliver step holds it after that.
	sc.WorkspaceProviders.Git[1].Entra.RunnerResident = true
	if r := admitted(); r != nil && strings.Contains(r.Detail, "opted in") {
		t.Fatalf("this connection opted in: %v", r)
	}
}
