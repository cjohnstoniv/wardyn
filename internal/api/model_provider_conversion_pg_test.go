// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_ModelProviderConversion_OutputPassesTheWriteDoors: whatever migration
// 0100_model_provider_conversion writes, an admin must be able to save back
// unchanged — so its document decodes STRICTLY into types.SiteConfig (no
// retired roster field survives) and passes the same validation
// PUT /model-providers and PUT /agent-providers apply. A conversion the doors
// refuse would wedge the admin's next edit on the upgrade's own output.
func TestPG_ModelProviderConversion_OutputPassesTheWriteDoors(t *testing.T) {
	migration, err := os.ReadFile("../db/migrations/0100_model_provider_conversion.sql")
	if err != nil {
		t.Fatal(err)
	}
	for name, doc := range map[string]string{
		"every AI kind, a stored provider and an id collision": `{"integrations": [
			{"id": "Corp:Key", "name": "Corp key", "kind": "anthropic_api_key"},
			{"id": "openai", "kind": "openai_api_key", "disabled": true},
			{"id": "legacy-sub", "category": "ai_provider", "type": "anthropic_subscription", "config": {"lane": "managed"}},
			{"id": "team-key", "kind": "anthropic_api_key"},
			{"id": "br-set", "kind": "bedrock", "config": {"region": "eu-west-1", "model": "eu.anthropic.claude", "auth_lane": "bearer"}},
			{"id": "br-env", "kind": "bedrock", "config": {"auth_lane": "bearer"}},
			{"id": "gh", "kind": "github_app", "config": {"app_id": "1"}}
		],
		"model_providers": {"providers": [{"id": "team-key", "uid": "u-team", "kind": "custom_endpoint",
			"base_url": "https://gw.example.com", "harnesses": [{"harness": "claude-code"}]}]}}`,
		"a per-person SSO roster row and a turned-off agent": `{
		"agent_providers": {"agents": [
			{"id": "claude-code", "mechanism": "bedrock_sso", "credential_source": "per_user",
			 "sso_start_url": "https://corp.awsapps.com/start", "sso_account_id": "123456789012", "sso_role_name": "Dev"},
			{"id": "codex-cli", "mechanism": "openai_api_key", "disabled": true}
		]},
		"integrations": [{"id": "br", "kind": "bedrock", "config": {"region": "us-east-1", "model": "us.anthropic.claude"}}]}`,
		"several Bedrock providers and no default": `{
		"agent_providers": {"agents": [{"id": "claude-code", "mechanism": "bedrock_sso",
			"sso_start_url": "https://corp.awsapps.com/start"}]},
		"integrations": [
			{"id": "br-a", "kind": "bedrock", "config": {"region": "us-east-1", "model": "m-a"}},
			{"id": "br-b", "kind": "bedrock", "config": {"region": "us-west-2", "model": "m-b"}}
		]}`,
		"defaults, org-wide lanes and an admin's own pick": `{
		"agent_providers": {"agents": [
			{"id": "claude-code", "mechanism": "bedrock_env"},
			{"id": "codex-cli", "mechanism": "openai_api_key", "default_provider": "admin-pick"}
		]},
		"integrations": [
			{"id": "host-sub", "kind": "anthropic_subscription", "config": {"lane": "resident_host"}},
			{"id": "key", "kind": "anthropic_api_key", "default_for": ["agent_runs"]},
			{"id": "oa", "kind": "openai_api_key", "default_for": ["agent_runs"]}
		],
		"model_providers": {"providers": [{"id": "admin-pick", "uid": "u1", "kind": "custom_endpoint",
			"base_url": "https://gw.example.com", "harnesses": [{"harness": "codex-cli"}]}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			pool := throwawayPGPool(t)
			if _, err := pool.Exec(ctx, `DELETE FROM site_config`); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO site_config (config) VALUES ($1)`, doc); err != nil {
				t.Fatalf("seed site_config: %v", err)
			}
			// 0100 predates audit_append and writes its audit rows with a direct INSERT, which the
			// partitioned table refuses. It runs here on one connection with a temporary table standing in
			// for audit_events: what is under test is the document it writes, not those rows.
			conn, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Release()
			if _, err := conn.Exec(ctx, `CREATE TEMP TABLE audit_events (id uuid, run_id uuid, actor_type text, actor text,
				action text, target text, outcome text, data jsonb)`); err != nil {
				t.Fatal(err)
			}
			defer conn.Exec(context.Background(), `DROP TABLE IF EXISTS pg_temp.audit_events`) //nolint:errcheck
			if _, err := conn.Exec(ctx, string(migration)); err != nil {
				t.Fatalf("run the conversion: %v", err)
			}
			var raw []byte
			if err := pool.QueryRow(ctx, `SELECT config FROM site_config WHERE singleton`).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var sc types.SiteConfig
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&sc); err != nil {
				t.Fatalf("the converted document does not decode strictly: %v\n%s", err, raw)
			}
			if err := validateModelProviders(sc.ModelProviders, false); err != nil {
				t.Errorf("PUT /model-providers would refuse the converted block: %v\n%s", err, raw)
			}
			if err := validateAgentProviders(sc.AgentProviders, nil); err != nil {
				t.Errorf("PUT /agent-providers would refuse the converted roster: %v\n%s", err, raw)
			}
			if err := validateDefaultProviders(sc.AgentProviders, sc.ModelProviders); err != nil {
				t.Errorf("the converted defaults are refused: %v\n%s", err, raw)
			}
			for _, in := range sc.Integrations {
				if in.Kind != types.IntegrationKindGitHubApp && in.Kind != types.IntegrationKindGitHost {
					t.Errorf("integration %q (kind %q) survived the conversion", in.ID, in.Kind)
				}
			}
		})
	}
}
