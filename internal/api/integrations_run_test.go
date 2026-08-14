// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// integrations_run_test.go covers the RUNTIME half of the entity: what an
// `integration:<id>` workspace requirement does to a run's resolved policy.
// The load-bearing claim across the whole file is NOTHING IS AMBIENT —
// configuring an integration grants nothing until a run is granted it.

// feedIntegration is the round-H shape under test: a generic-kind row that
// names its own egress and delivers its credential by proxy header.
func feedIntegration() types.Integration {
	return types.Integration{
		ID: "corp-artifactory", Name: "Corp Artifactory",
		Kind:   "artifactory",
		Egress: []string{"artifactory.corp.internal", "nexus.corp.internal"},
		Secrets: []types.IntegrationSecret{{Role: types.IntegrationCredentialToken, SecretName: "artifactory-token",
			Delivery: &types.IntegrationDelivery{Mode: types.DeliveryProxyHeader, Header: "Authorization", Format: "Bearer %s"}}},
	}
}

// runIntegrationSrv builds a Server whose site config holds `integs` and whose
// secret store holds `secrets`.
func runIntegrationSrv(t *testing.T, integs []types.Integration, secrets map[string][]byte) *Server {
	t.Helper()
	return New(integrationsTestConfig(t, types.SiteConfig{Integrations: integs}, secrets))
}

// wsRequiring returns one workspace whose contract names `key` at `level`.
func wsRequiring(id uuid.UUID, key, level string) []types.Workspace {
	return []types.Workspace{{ID: id, Requirements: map[string]types.WorkspaceRequirement{
		key: {Level: level, Provenance: "operator_set"},
	}}}
}

// apiKeyScopes decodes every api_key grant scope on the spec, in order.
func apiKeyScopes(t *testing.T, spec *types.RunPolicySpec) []map[string]string {
	t.Helper()
	var out []map[string]string
	for _, g := range spec.EligibleGrants {
		if g.Kind != types.GrantAPIKey {
			continue
		}
		var sc map[string]string
		if err := json.Unmarshal(g.Scope, &sc); err != nil {
			t.Fatalf("decode api_key scope: %v", err)
		}
		out = append(out, sc)
	}
	return out
}

// ─── nothing is ambient ──────────────────────────────────────────────────────

// TestIntegrationFold_NothingIsAmbient is the invariant the whole design rests
// on: an integration that EXISTS but is not named by any workspace requirement
// must leave the resolved spec byte-identical. An operator with integrations
// configured and a workspace that names none of them gets exactly the run they
// would have got with none configured at all.
func TestIntegrationFold_NothingIsAmbient(t *testing.T) {
	srv := runIntegrationSrv(t, []types.Integration{feedIntegration()},
		map[string][]byte{"artifactory-token": []byte("tok")})
	wsID := uuid.New()
	cases := map[string][]types.Workspace{
		"no requirements at all":             {{ID: wsID}},
		"requirements naming something else": wsRequiring(wsID, "egress:api.stripe.com", "required"),
	}
	for name, wsRefs := range cases {
		t.Run(name, func(t *testing.T) {
			spec := &types.RunPolicySpec{MinConfinementClass: types.CC2}
			before, _ := json.Marshal(spec)
			srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code", wsRefs, nil)
			// The egress-only case legitimately adds its own host; what must
			// NEVER appear is anything belonging to the integration.
			for _, h := range feedIntegration().Egress {
				if slices.Contains(spec.AllowedDomains, h) {
					t.Errorf("host %q reached the run without being granted — integrations must never be ambient", h)
				}
			}
			if len(apiKeyScopes(t, spec)) != 0 {
				t.Errorf("an unnamed integration authored a credential grant; spec was %s", before)
			}
		})
	}
}

// ─── the granted path ────────────────────────────────────────────────────────

func TestIntegrationFold_GrantedFoldsHostsAndOneGrantPerHost(t *testing.T) {
	srv := runIntegrationSrv(t, []types.Integration{feedIntegration()},
		map[string][]byte{"artifactory-token": []byte("tok")})
	spec := &types.RunPolicySpec{}
	events := srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code",
		wsRequiring(uuid.New(), "integration:corp-artifactory", "required"), nil)

	for _, h := range feedIntegration().Egress {
		if !slices.Contains(spec.AllowedDomains, h) {
			t.Errorf("AllowedDomains = %v, want %q — the integration is the reason it is reachable", spec.AllowedDomains, h)
		}
	}
	scopes := apiKeyScopes(t, spec)
	if len(scopes) != 2 {
		t.Fatalf("api_key grants = %d, want one per host; got %+v", len(scopes), scopes)
	}
	for _, sc := range scopes {
		if sc["header"] != "Authorization" || sc["format"] != "Bearer %s" || sc["secret_name"] != "artifactory-token" {
			t.Errorf("grant scope = %+v, want the integration's own header/format/secret", sc)
		}
		if !slices.Contains(feedIntegration().Egress, sc["host"]) {
			t.Errorf("grant scope host = %q, not one of the integration's hosts", sc["host"])
		}
	}
	if len(events) != 1 || events[0].action != "run.workspace.requirement.integration" {
		t.Fatalf("events = %+v, want one integration audit entry", events)
	}
	if events[0].target != "corp-artifactory" {
		t.Errorf("audit target = %q, want the integration id", events[0].target)
	}
}

// An empty Format means the RAW secret is the header value. It must be written
// explicitly as "%s": injectionRuleFromScope defaults an empty format to
// "Bearer %s", which would corrupt every custom-header credential.
func TestIntegrationFold_EmptyFormatBecomesRawSecretNotBearer(t *testing.T) {
	integ := feedIntegration()
	integ.Secrets[0].Delivery = &types.IntegrationDelivery{Mode: types.DeliveryProxyHeader, Header: "x-api-key"}
	integ.Egress = []string{"artifactory.corp.internal"}
	srv := runIntegrationSrv(t, []types.Integration{integ}, map[string][]byte{"artifactory-token": []byte("tok")})

	spec := &types.RunPolicySpec{}
	srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code",
		wsRequiring(uuid.New(), "integration:corp-artifactory", "required"), nil)

	scopes := apiKeyScopes(t, spec)
	if len(scopes) != 1 || scopes[0]["format"] != "%s" {
		t.Fatalf("scopes = %+v, want exactly one with format %%s (a raw secret, never Bearer)", scopes)
	}
}

// ─── optional lane, same rules as every other requirement type ──────────────

func TestIntegrationFold_OptionalNeedsAnExplicitPerRunEnable(t *testing.T) {
	srv := runIntegrationSrv(t, []types.Integration{feedIntegration()},
		map[string][]byte{"artifactory-token": []byte("tok")})
	wsID := uuid.New()
	wsRefs := wsRequiring(wsID, "integration:corp-artifactory", "optional")

	spec := &types.RunPolicySpec{}
	srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code", wsRefs, nil)
	if len(spec.AllowedDomains) != 0 || len(apiKeyScopes(t, spec)) != 0 {
		t.Errorf("optional integration folded in without an enable: domains=%v grants=%+v", spec.AllowedDomains, spec.EligibleGrants)
	}

	enabled := &types.RunPolicySpec{}
	sel := map[string]client.WorkspaceSelection{
		wsID.String(): {WorkspaceID: wsID.String(), EnabledOptional: []string{"integration:corp-artifactory"}},
	}
	srv.applyWorkspaceRequirements(context.Background(), enabled, "claude-code", wsRefs, sel)
	if len(enabled.AllowedDomains) != 2 || len(apiKeyScopes(t, enabled)) != 2 {
		t.Errorf("enabled optional integration must fold in fully: domains=%v grants=%+v", enabled.AllowedDomains, enabled.EligibleGrants)
	}
}

// ─── degrade, never brick ────────────────────────────────────────────────────

// Every one of these would fail the proxy CLOSED at startup if it authored a
// grant anyway (an unresolvable secret, or a host the exact allowlist can't
// match) — so each drops the credential and leaves the path open instead of
// bricking the run.
func TestIntegrationFold_DegradesNeverBricks(t *testing.T) {
	t.Run("unknown id opens nothing", func(t *testing.T) {
		srv := runIntegrationSrv(t, nil, nil)
		spec := &types.RunPolicySpec{}
		events := srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code",
			wsRequiring(uuid.New(), "integration:not-configured-yet", "required"), nil)
		if len(spec.AllowedDomains) != 0 || len(spec.EligibleGrants) != 0 || len(events) != 0 {
			t.Errorf("an unconfigured integration must be a no-op: %+v / %+v", spec, events)
		}
	})

	t.Run("disabled row opens nothing", func(t *testing.T) {
		integ := feedIntegration()
		integ.Disabled = true
		srv := runIntegrationSrv(t, []types.Integration{integ}, map[string][]byte{"artifactory-token": []byte("tok")})
		spec := &types.RunPolicySpec{}
		srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code",
			wsRequiring(uuid.New(), "integration:corp-artifactory", "required"), nil)
		if len(spec.AllowedDomains) != 0 || len(spec.EligibleGrants) != 0 {
			t.Errorf("a disabled integration must grant nothing: %+v", spec)
		}
	})

	t.Run("unstored secret opens the path but grants no credential", func(t *testing.T) {
		srv := runIntegrationSrv(t, []types.Integration{feedIntegration()}, nil) // secret absent
		spec := &types.RunPolicySpec{}
		srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code",
			wsRequiring(uuid.New(), "integration:corp-artifactory", "required"), nil)
		if len(spec.AllowedDomains) != 2 {
			t.Errorf("hosts must still open — reaching the system is the value: %v", spec.AllowedDomains)
		}
		if len(apiKeyScopes(t, spec)) != 0 {
			t.Error("an unstored secret must not author a grant; the proxy would fail closed at startup")
		}
	})

	t.Run("wildcard and port hosts open the path but never carry the credential", func(t *testing.T) {
		// A row stored before validateIntegrationHosts existed, or a derived
		// one — the write path rejects this combination today.
		integ := feedIntegration()
		integ.Egress = []string{"*.corp.internal", "nexus.corp.internal:8443", "artifactory.corp.internal"}
		srv := runIntegrationSrv(t, []types.Integration{integ}, map[string][]byte{"artifactory-token": []byte("tok")})
		spec := &types.RunPolicySpec{}
		srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code",
			wsRequiring(uuid.New(), "integration:corp-artifactory", "required"), nil)
		if len(spec.AllowedDomains) != 3 {
			t.Errorf("all three entries should reach egress: %v", spec.AllowedDomains)
		}
		scopes := apiKeyScopes(t, spec)
		if len(scopes) != 1 || scopes[0]["host"] != "artifactory.corp.internal" {
			t.Fatalf("scopes = %+v, want ONLY the bare exact host (buildInjector refuses the others)", scopes)
		}
	})

	t.Run("no hosts is a no-op", func(t *testing.T) {
		integ := feedIntegration()
		integ.Egress = nil
		srv := runIntegrationSrv(t, []types.Integration{integ}, map[string][]byte{"artifactory-token": []byte("tok")})
		spec := &types.RunPolicySpec{}
		events := srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code",
			wsRequiring(uuid.New(), "integration:corp-artifactory", "required"), nil)
		if len(spec.EligibleGrants) != 0 || len(events) != 0 {
			t.Errorf("an integration with no hosts opens nothing: %+v / %+v", spec, events)
		}
	})
}

// ─── never double-grant a host ───────────────────────────────────────────────

// Two workspaces requiring the SAME integration, or an integration whose host
// a policy already credentialed, must not stack two api_key grants on one host
// — the same rule ensureLLMGrant/applyWorkspaceCreds follow.
func TestIntegrationFold_NeverDoubleGrantsAHost(t *testing.T) {
	integ := feedIntegration()
	integ.Egress = []string{"artifactory.corp.internal"}
	srv := runIntegrationSrv(t, []types.Integration{integ}, map[string][]byte{"artifactory-token": []byte("tok")})

	wsRefs := []types.Workspace{
		{ID: uuid.New(), Requirements: map[string]types.WorkspaceRequirement{
			"integration:corp-artifactory": {Level: "required", Provenance: "operator_set"},
		}},
		{ID: uuid.New(), Requirements: map[string]types.WorkspaceRequirement{
			"integration:corp-artifactory": {Level: "required", Provenance: "operator_set"},
		}},
	}
	spec := &types.RunPolicySpec{}
	events := srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code", wsRefs, nil)

	if n := len(apiKeyScopes(t, spec)); n != 1 {
		t.Errorf("api_key grants = %d, want exactly 1 for one host across two requiring workspaces", n)
	}
	if len(events) != 1 {
		t.Errorf("events = %+v, want one — the second fold changed nothing, so it audits nothing", events)
	}
}
