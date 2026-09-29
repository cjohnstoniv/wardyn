// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// conversionFloor is the model-provider conversion (multi-provider design
// §2.11): every test here seeds a pre-conversion document below it and runs the
// real Migrate() over it, the path an upgrading install takes.
const conversionFloor = "0100"

// convertedDoc is the part of the converted site config these tests read.
type convertedDoc struct {
	Integrations []struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
	} `json:"integrations"`
	AgentProviders *struct {
		Agents []map[string]any `json:"agents"`
	} `json:"agent_providers"`
	ModelProviders *struct {
		Providers []convertedProvider `json:"providers"`
	} `json:"model_providers"`
}

type convertedProvider struct {
	ID       string `json:"id"`
	UID      string `json:"uid"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Disabled bool   `json:"disabled"`
	Bedrock  *struct {
		Region       string `json:"region"`
		SSOStartURL  string `json:"sso_start_url"`
		SSOAccountID string `json:"sso_account_id"`
		SSORoleName  string `json:"sso_role_name"`
	} `json:"bedrock"`
	Harnesses []struct {
		Harness string `json:"harness"`
		Model   string `json:"model"`
	} `json:"harnesses"`
}

func (d convertedDoc) provider(t *testing.T, id string) convertedProvider {
	t.Helper()
	if d.ModelProviders != nil {
		for _, p := range d.ModelProviders.Providers {
			if p.ID == id {
				return p
			}
		}
	}
	t.Fatalf("no converted provider %q; have %+v", id, d.ModelProviders)
	return convertedProvider{}
}

func (d convertedDoc) providerIDs() []string {
	var ids []string
	if d.ModelProviders != nil {
		for _, p := range d.ModelProviders.Providers {
			ids = append(ids, p.ID)
		}
	}
	return ids
}

func (d convertedDoc) roster(t *testing.T, agent string) map[string]any {
	t.Helper()
	if d.AgentProviders != nil {
		for _, a := range d.AgentProviders.Agents {
			if a["id"] == agent {
				return a
			}
		}
	}
	t.Fatalf("no roster row %q", agent)
	return nil
}

func (p convertedProvider) harnesses() []string {
	var out []string
	for _, h := range p.Harnesses {
		out = append(out, h.Harness+"="+h.Model)
	}
	return out
}

// conversionAudit is one row the conversion wrote.
type conversionAudit struct {
	Action, Target string
	Data           map[string]any
}

// convertSiteConfig seeds config (a pre-conversion site-config document) and
// anything seed adds, runs Migrate() from conversionFloor, and returns the
// converted document with every audit row the conversion wrote.
func convertSiteConfig(t *testing.T, config string, seed func(ctx context.Context, pool *pgxpool.Pool)) (convertedDoc, []conversionAudit, *pgxpool.Pool) {
	t.Helper()
	pool, _ := partialSchemaPool(t, conversionFloor)
	ctx := context.Background()
	if config != "" {
		if _, err := pool.Exec(ctx, `INSERT INTO site_config (config) VALUES ($1)`, config); err != nil {
			t.Fatalf("seed site_config: %v", err)
		}
	}
	if seed != nil {
		seed(ctx, pool)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate from %s: %v", conversionFloor, err)
	}
	var doc convertedDoc
	if config != "" {
		var raw []byte
		if err := pool.QueryRow(ctx, `SELECT config FROM site_config WHERE singleton`).Scan(&raw); err != nil {
			t.Fatalf("read converted site_config: %v", err)
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("decode converted site_config %s: %v", raw, err)
		}
	}
	rows, err := pool.Query(ctx, `SELECT action, target, COALESCE(data, '{}'::jsonb) FROM audit_events
		WHERE actor = 'wardyn/migration' ORDER BY seq`)
	if err != nil {
		t.Fatalf("read conversion audit: %v", err)
	}
	defer rows.Close()
	var audit []conversionAudit
	for rows.Next() {
		var a conversionAudit
		var raw []byte
		if err := rows.Scan(&a.Action, &a.Target, &raw); err != nil {
			t.Fatalf("scan audit: %v", err)
		}
		if err := json.Unmarshal(raw, &a.Data); err != nil {
			t.Fatalf("decode audit data: %v", err)
		}
		audit = append(audit, a)
	}
	return doc, audit, pool
}

// notConverted returns the not_converted rows naming id, as "reason:provider".
func notConverted(audit []conversionAudit, id string) []string {
	var out []string
	for _, a := range audit {
		if a.Action == "model_provider.not_converted" && a.Data["id"] == id {
			p, _ := a.Data["provider"].(string)
			out = append(out, a.Data["reason"].(string)+":"+p)
		}
	}
	return out
}

// TestPG_ModelProviderConversion_AIKindIntegration: an AI-kind integration
// becomes a provider of the same id (made a provider id), enabled on the
// harnesses that can drive its kind, and the integration row itself is gone —
// both stored shapes, the base-component one and the {category, type} one. An
// id a provider already holds maps onto it, and nothing new is made.
func TestPG_ModelProviderConversion_AIKindIntegration(t *testing.T) {
	doc, audit, _ := convertSiteConfig(t, `{
		"integrations": [
			{"id": "Corp:Key", "name": "Corp key", "kind": "anthropic_api_key", "secrets": [{"role": "api_key", "secret_name": "anthropic-api-key"}]},
			{"id": "openai", "kind": "openai_api_key", "disabled": true},
			{"id": "legacy-sub", "category": "ai_provider", "type": "anthropic_subscription", "config": {"lane": "managed"}},
			{"id": "team-key", "kind": "anthropic_api_key"},
			{"id": "gh", "kind": "github_app", "config": {"app_id": "1"}}
		],
		"model_providers": {"providers": [{"id": "team-key", "uid": "u-team", "kind": "custom_endpoint", "base_url": "https://gw.example.com"}]}
	}`, nil)

	if got := doc.providerIDs(); !slices.Equal(got, []string{"team-key", "corp-key", "openai", "legacy-sub"}) {
		t.Fatalf("providers = %v, want the stored one then corp-key, openai, legacy-sub", got)
	}
	key := doc.provider(t, "corp-key")
	if key.Kind != "anthropic_api_key" || key.Disabled || key.Name != "Corp key" || key.UID == "" {
		t.Errorf("corp-key = %+v, want an enabled, named anthropic_api_key provider with a minted uid", key)
	}
	if _, err := uuid.Parse(key.UID); err != nil {
		t.Errorf("corp-key uid %q is not a uuid: %v", key.UID, err)
	}
	if got := key.harnesses(); !slices.Equal(got, []string{"claude-code="}) {
		t.Errorf("corp-key harnesses = %v, want claude-code only", got)
	}
	if oa := doc.provider(t, "openai"); oa.Kind != "openai_api_key" || !oa.Disabled || !slices.Equal(oa.harnesses(), []string{"codex-cli="}) {
		t.Errorf("openai = %+v, want a turned-off openai_api_key provider serving codex-cli", oa)
	}
	if sub := doc.provider(t, "legacy-sub"); sub.Kind != "anthropic_subscription" || sub.Disabled {
		t.Errorf("legacy-sub = %+v, want an enabled anthropic_subscription provider", sub)
	}
	if team := doc.provider(t, "team-key"); team.Kind != "custom_endpoint" || team.UID != "u-team" {
		t.Errorf("the stored team-key provider changed: %+v", team)
	}
	if len(doc.Integrations) != 1 || doc.Integrations[0].ID != "gh" {
		t.Errorf("integrations = %+v, want only the github_app row left", doc.Integrations)
	}
	if len(audit) != 0 {
		t.Errorf("a clean conversion wrote audit rows: %+v", audit)
	}
}

// TestPG_ModelProviderConversion_BedrockLane: auth_lane auto becomes bedrock_sso
// when the roster names it or an AWS sign-in was ever stored, else
// bedrock_bearer; bearer is bearer; any other lane is turned off and audited.
func TestPG_ModelProviderConversion_BedrockLane(t *testing.T) {
	const bedrock = `{"id": "br", "kind": "bedrock", "config": {"region": "us-east-1", "model": "us.anthropic.claude", "auth_lane": %q}}`
	cases := []struct {
		name, lane, roster string
		signedIn           bool
		wantKind           string
		wantOff            []string // not_converted "reason:provider" rows naming br
	}{
		{"auto, nothing names SSO", "auto", "", false, "bedrock_bearer", nil},
		{"unset lane reads as auto", "", "", false, "bedrock_bearer", nil},
		{"auto, a sign-in was stored", "auto", "", true, "bedrock_sso", []string{"no_sso_start_url:br"}},
		{"auto, the roster names SSO", "auto", `"agent_providers": {"agents": [{"id": "claude-code", "mechanism": "bedrock_sso", "credential_source": "per_user", "sso_start_url": "https://corp.awsapps.com/start"}]},`,
			false, "bedrock_sso", nil},
		{"bearer", "bearer", "", true, "bedrock_bearer", nil},
		{"an unknown lane", "env", "", false, "bedrock_bearer", []string{"unknown_bedrock_auth_lane:br"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := `{` + tc.roster + `"integrations": [` + strings.Replace(bedrock, "%q", `"`+tc.lane+`"`, 1) + `]}`
			doc, audit, _ := convertSiteConfig(t, cfg, func(ctx context.Context, pool *pgxpool.Pool) {
				if tc.signedIn {
					if _, err := pool.Exec(ctx, `INSERT INTO secrets (name, ciphertext, owned_by) VALUES ('wardyn-harness-aws-oauth', '\x00'::bytea, 'ann@example.com')`); err != nil {
						t.Fatalf("seed the stored sign-in: %v", err)
					}
				}
			})
			br := doc.provider(t, "br")
			if br.Kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", br.Kind, tc.wantKind)
			}
			if br.Disabled != (tc.wantOff != nil) {
				t.Errorf("disabled = %v, want %v", br.Disabled, tc.wantOff != nil)
			}
			if got := notConverted(audit, "br"); !slices.Equal(got, tc.wantOff) {
				t.Errorf("not_converted rows = %v, want %v", got, tc.wantOff)
			}
		})
	}
}

// TestPG_ModelProviderConversion_BedrockRegionModel: config.region and model
// become bedrock.region and the claude-code model; with neither (they came from
// the boot environment) the provider is made turned off and audited.
func TestPG_ModelProviderConversion_BedrockRegionModel(t *testing.T) {
	doc, audit, _ := convertSiteConfig(t, `{"integrations": [
		{"id": "br-set", "kind": "bedrock", "config": {"region": "eu-west-1", "model": "eu.anthropic.claude", "auth_lane": "bearer"}},
		{"id": "br-env", "kind": "bedrock", "config": {"auth_lane": "bearer"}}
	]}`, nil)
	set := doc.provider(t, "br-set")
	if set.Disabled || set.Bedrock == nil || set.Bedrock.Region != "eu-west-1" || !slices.Equal(set.harnesses(), []string{"claude-code=eu.anthropic.claude"}) {
		t.Errorf("br-set = %+v, want an enabled provider in eu-west-1 serving claude-code on eu.anthropic.claude", set)
	}
	env := doc.provider(t, "br-env")
	if !env.Disabled || env.Bedrock == nil || env.Bedrock.Region != "" {
		t.Errorf("br-env = %+v, want a turned-off provider with an empty bedrock block", env)
	}
	if got := notConverted(audit, "br-env"); !slices.Equal(got, []string{"region_or_model_from_boot_env:br-env"}) {
		t.Errorf("not_converted rows for br-env = %v", got)
	}
	if got := notConverted(audit, "br-set"); got != nil {
		t.Errorf("br-set was audited %v", got)
	}
}

// TestPG_ModelProviderConversion_RosterRow: a roster row's lane joins the one
// provider of that kind, or a new "<agent>-<mechanism>" provider; the row keeps
// only id, disabled and default_provider, which names that provider. A
// bedrock_sso row's start URL and pin fill the provider's sign-in setup.
func TestPG_ModelProviderConversion_RosterRow(t *testing.T) {
	doc, audit, _ := convertSiteConfig(t, `{
		"agent_providers": {"agents": [
			{"id": "claude-code", "mechanism": "bedrock_sso", "credential_source": "per_user",
			 "sso_start_url": "https://corp.awsapps.com/start", "sso_account_id": "123456789012", "sso_role_name": "Dev"},
			{"id": "codex-cli", "mechanism": "openai_api_key", "disabled": true},
			{"id": "none", "mechanism": "none"}
		]},
		"integrations": [{"id": "br", "kind": "bedrock", "config": {"region": "us-east-1", "model": "us.anthropic.claude"}}]
	}`, nil)
	br := doc.provider(t, "br")
	if br.Kind != "bedrock_sso" || br.Disabled || br.Bedrock == nil ||
		br.Bedrock.SSOStartURL != "https://corp.awsapps.com/start" || br.Bedrock.SSOAccountID != "123456789012" || br.Bedrock.SSORoleName != "Dev" {
		t.Errorf("br = %+v, want an enabled bedrock_sso provider carrying the roster's start URL and pin", br)
	}
	cx := doc.provider(t, "codex-cli-openai_api_key")
	if cx.Kind != "openai_api_key" || cx.Disabled || !slices.Equal(cx.harnesses(), []string{"codex-cli="}) {
		t.Errorf("codex-cli-openai_api_key = %+v, want a new enabled openai_api_key provider serving codex-cli", cx)
	}
	for agent, want := range map[string]map[string]any{
		"claude-code": {"id": "claude-code", "default_provider": "br"},
		"codex-cli":   {"id": "codex-cli", "disabled": true, "default_provider": "codex-cli-openai_api_key"},
		"none":        {"id": "none"},
	} {
		if got := doc.roster(t, agent); !mapsEqual(got, want) {
			t.Errorf("roster %s = %v, want %v (mechanism, credential_source and sso_* gone)", agent, got, want)
		}
	}
	if len(audit) != 0 {
		t.Errorf("a clean conversion wrote audit rows: %+v", audit)
	}
}

// TestPG_ModelProviderConversion_RosterSeveralProviders: with two providers of
// the row's kind, the DefaultFor:agent_runs one takes the roster's sign-in
// setup; with no default, a new provider is made turned off and audited.
func TestPG_ModelProviderConversion_RosterSeveralProviders(t *testing.T) {
	const roster = `"agent_providers": {"agents": [{"id": "claude-code", "mechanism": "bedrock_sso", "credential_source": "per_user", "sso_start_url": "https://corp.awsapps.com/start"}]},`
	t.Run("the default one takes it", func(t *testing.T) {
		doc, _, _ := convertSiteConfig(t, `{`+roster+`"integrations": [
			{"id": "br-a", "kind": "bedrock", "config": {"region": "us-east-1", "model": "m-a"}},
			{"id": "br-b", "kind": "bedrock", "config": {"region": "us-west-2", "model": "m-b"}, "default_for": ["agent_runs"]}
		]}`, nil)
		if b := doc.provider(t, "br-b"); b.Disabled || b.Bedrock.SSOStartURL != "https://corp.awsapps.com/start" {
			t.Errorf("br-b = %+v, want the default to carry the start URL and stay on", b)
		}
		if a := doc.provider(t, "br-a"); !a.Disabled || a.Bedrock.SSOStartURL != "" {
			t.Errorf("br-a = %+v, want no start URL (so turned off)", a)
		}
		if got := doc.roster(t, "claude-code")["default_provider"]; got != "br-b" {
			t.Errorf("default_provider = %v, want br-b", got)
		}
	})
	t.Run("no default", func(t *testing.T) {
		doc, audit, _ := convertSiteConfig(t, `{`+roster+`"integrations": [
			{"id": "br-a", "kind": "bedrock", "config": {"region": "us-east-1", "model": "m-a"}},
			{"id": "br-b", "kind": "bedrock", "config": {"region": "us-west-2", "model": "m-b"}}
		]}`, nil)
		made := doc.provider(t, "claude-code-bedrock_sso")
		if !made.Disabled || made.Kind != "bedrock_sso" {
			t.Errorf("claude-code-bedrock_sso = %+v, want a turned-off bedrock_sso provider", made)
		}
		if got := notConverted(audit, "claude-code"); !slices.Equal(got, []string{"several_providers_no_default:claude-code-bedrock_sso"}) {
			t.Errorf("not_converted rows for claude-code = %v", got)
		}
		if got := doc.roster(t, "claude-code")["default_provider"]; got != "claude-code-bedrock_sso" {
			t.Errorf("default_provider = %v, want the turned-off sentinel claude-code-bedrock_sso", got)
		}
	})
}

// TestPG_ModelProviderConversion_OrgWideNotConverted: the daemon's own AWS
// environment, the host ~/.aws mount and the operator's host ~/.claude are one
// credential serving everyone, so none becomes a provider; each is audited.
func TestPG_ModelProviderConversion_OrgWideNotConverted(t *testing.T) {
	doc, audit, _ := convertSiteConfig(t, `{
		"agent_providers": {"agents": [
			{"id": "claude-code", "mechanism": "bedrock_env"},
			{"id": "codex-cli", "mechanism": "openai_api_key"}
		]},
		"integrations": [{"id": "host-sub", "kind": "anthropic_subscription", "config": {"lane": "resident_host"}}]
	}`, nil)
	for _, id := range []string{"host-sub", "claude-code-bedrock_env"} {
		if slices.Contains(doc.providerIDs(), id) {
			t.Errorf("%s became a provider", id)
		}
	}
	if got := notConverted(audit, "claude-code"); !slices.Equal(got, []string{"org_wide_credential:"}) {
		t.Errorf("not_converted rows for claude-code = %v", got)
	}
	if got := notConverted(audit, "host-sub"); !slices.Equal(got, []string{"org_wide_credential:"}) {
		t.Errorf("not_converted rows for host-sub = %v", got)
	}
	if got := doc.roster(t, "claude-code"); !mapsEqual(got, map[string]any{"id": "claude-code"}) {
		t.Errorf("roster claude-code = %v, want the bare row", got)
	}
	if len(doc.Integrations) != 0 {
		t.Errorf("the resident_host integration row was kept: %+v", doc.Integrations)
	}
}

// TestPG_ModelProviderConversion_DefaultFor: DefaultFor:agent_runs becomes the
// default of each harness it serves — unless the row's own lane joined another
// provider, which wins, or an admin already chose one, which stands.
func TestPG_ModelProviderConversion_DefaultFor(t *testing.T) {
	doc, _, _ := convertSiteConfig(t, `{
		"agent_providers": {"agents": [
			{"id": "claude-code", "mechanism": "anthropic_subscription"},
			{"id": "codex-cli", "mechanism": "openai_api_key", "default_provider": "admin-pick"}
		]},
		"integrations": [
			{"id": "sub", "kind": "anthropic_subscription"},
			{"id": "key", "kind": "anthropic_api_key", "default_for": ["agent_runs"]},
			{"id": "oa", "kind": "openai_api_key", "default_for": ["agent_runs"]}
		],
		"model_providers": {"providers": [{"id": "admin-pick", "uid": "u1", "kind": "custom_endpoint", "base_url": "https://gw.example.com", "harnesses": [{"harness": "codex-cli"}]}]}
	}`, nil)
	if got := doc.roster(t, "claude-code")["default_provider"]; got != "sub" {
		t.Errorf("claude-code default = %v, want sub (the roster's lane wins over DefaultFor)", got)
	}
	if got := doc.roster(t, "codex-cli")["default_provider"]; got != "admin-pick" {
		t.Errorf("codex-cli default = %v, want the admin's own admin-pick", got)
	}

	doc, _, _ = convertSiteConfig(t, `{
		"agent_providers": {"agents": [{"id": "claude-code", "mechanism": "bedrock_aws_dir"}]},
		"integrations": [{"id": "key", "kind": "anthropic_api_key", "default_for": ["agent_runs"]}]
	}`, nil)
	if got := doc.roster(t, "claude-code")["default_provider"]; got != "key" {
		t.Errorf("claude-code default = %v, want key (DefaultFor, the row's own lane was not converted)", got)
	}
}

// TestPG_ModelProviderConversion_DefaultForNeedsRoster: with no roster there is
// no row to carry a default; where more than one provider serves the harness
// that loss is audited.
func TestPG_ModelProviderConversion_DefaultForNeedsRoster(t *testing.T) {
	doc, audit, _ := convertSiteConfig(t, `{"integrations": [
		{"id": "sub", "kind": "anthropic_subscription"},
		{"id": "key", "kind": "anthropic_api_key", "default_for": ["agent_runs"]}
	]}`, nil)
	if doc.AgentProviders != nil {
		t.Errorf("a roster was made: %+v", doc.AgentProviders)
	}
	if got := notConverted(audit, "key"); !slices.Equal(got, []string{"default_needs_agent_roster:key"}) {
		t.Errorf("not_converted rows for key = %v", got)
	}
}

// TestPG_ModelProviderConversion_WorkspacePin: integration_ref becomes
// provider_ref naming the provider converted from it, or NULL when nothing was;
// every rewritten pin writes workspace.llm_cred.migrated; a provider pin
// already set stands, and a pre-Integration shape is cleared silently.
func TestPG_ModelProviderConversion_WorkspacePin(t *testing.T) {
	ids := map[string]uuid.UUID{"mapped": uuid.New(), "gone": uuid.New(), "pinned": uuid.New(), "ancient": uuid.New()}
	creds := map[string]string{
		"mapped":  `{"integration_ref": "Corp:Key"}`,
		"gone":    `{"integration_ref": "deleted-row"}`,
		"pinned":  `{"integration_ref": "Corp:Key", "provider_ref": "chosen"}`,
		"ancient": `{"mode": "api_key", "api_key_secret": "anthropic-api-key"}`,
	}
	_, audit, pool := convertSiteConfig(t, `{"integrations": [{"id": "Corp:Key", "kind": "anthropic_api_key"}]}`,
		func(ctx context.Context, pool *pgxpool.Pool) {
			for name, id := range ids {
				if _, err := pool.Exec(ctx, `INSERT INTO workspaces (id, name, attachments, llm_cred) VALUES ($1, $2, '[]'::jsonb, $3)`,
					id, "ws-"+name, creds[name]); err != nil {
					t.Fatalf("seed workspace %s: %v", name, err)
				}
			}
		})
	want := map[string]string{"mapped": `{"provider_ref": "corp-key"}`, "gone": "", "pinned": `{"provider_ref": "chosen"}`, "ancient": ""}
	for name, id := range ids {
		var got *string
		if err := pool.QueryRow(context.Background(), `SELECT llm_cred::text FROM workspaces WHERE id = $1`, id).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if (got == nil) != (want[name] == "") || (got != nil && *got != want[name]) {
			t.Errorf("%s llm_cred = %v, want %q", name, deref(got), want[name])
		}
	}
	migrated := map[string]map[string]any{}
	for _, a := range audit {
		if a.Action == "workspace.llm_cred.migrated" {
			migrated[a.Target] = a.Data
		}
	}
	if len(migrated) != 3 {
		t.Fatalf("workspace.llm_cred.migrated rows = %v, want mapped, gone and pinned", migrated)
	}
	for name, to := range map[string]any{"mapped": "corp-key", "gone": nil, "pinned": "chosen"} {
		d := migrated[ids[name].String()]
		if d == nil || d["provider_ref"] != to || d["integration_ref"] == nil {
			t.Errorf("%s migrated row = %v, want provider_ref %v", name, d, to)
		}
	}
}

// TestPG_ModelProviderConversion_CancelsReauthHolds: every PENDING model
// credential re-auth hold that names no provider is cancelled — a sign-in
// resolves only its own provider's hold from here on — with one approval.cancel
// row per run. An Azure DevOps sign-in hold, a provider's own hold and a hold
// already decided are left alone.
func TestPG_ModelProviderConversion_CancelsReauthHolds(t *testing.T) {
	scopes := map[string]string{
		"aws":      `{"mechanism": "bedrock_sso", "credential_source": "per_user", "owner": "ann@example.com"}`,
		"ado":      `{"lane": "azure_devops", "mechanism": "entra_signin", "owner": "ann@example.com"}`,
		"provider": `{"mechanism": "bedrock_sso", "credential_source": "per_user", "owner": "ann@example.com", "provider": "br", "provider_uid": "u1"}`,
	}
	approvals := map[string]uuid.UUID{}
	var runID uuid.UUID
	_, audit, pool := convertSiteConfig(t, `{}`, func(ctx context.Context, pool *pgxpool.Pool) {
		var err error
		if runID, err = insertAgentRun(t, pool, "RUNNING"); err != nil {
			t.Fatalf("seed run: %v", err)
		}
		for name, scope := range scopes {
			approvals[name] = uuid.New()
			if _, err := pool.Exec(ctx, `INSERT INTO approvals (id, run_id, kind, requested_scope) VALUES ($1, $2, 'credential_reauth', $3)`,
				approvals[name], runID, scope); err != nil {
				t.Fatalf("seed %s hold: %v", name, err)
			}
		}
		approvals["decided"] = uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO approvals (id, run_id, kind, requested_scope, state) VALUES ($1, $2, 'credential_reauth', $3, 'APPROVED')`,
			approvals["decided"], runID, `{"credential_source": "shared", "owner": ""}`); err != nil {
			t.Fatalf("seed decided hold: %v", err)
		}
	})
	for name, want := range map[string]string{"aws": "CANCELLED", "ado": "PENDING", "provider": "PENDING", "decided": "APPROVED"} {
		var state, reason string
		if err := pool.QueryRow(context.Background(), `SELECT state, reason FROM approvals WHERE id = $1`, approvals[name]).Scan(&state, &reason); err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if state != want {
			t.Errorf("%s hold = %s, want %s", name, state, want)
		}
		if want == "CANCELLED" && reason != "model_provider_conversion" {
			t.Errorf("%s reason = %q", name, reason)
		}
	}
	var cancels []conversionAudit
	for _, a := range audit {
		if a.Action == "approval.cancel" {
			cancels = append(cancels, a)
		}
	}
	if len(cancels) != 1 || cancels[0].Target != runID.String() || cancels[0].Data["count"] != float64(1) ||
		cancels[0].Data["reason"] != "model_provider_conversion" {
		t.Fatalf("approval.cancel rows = %+v, want one for the run, count 1", cancels)
	}
	if bk, _ := cancels[0].Data["by_kind"].(map[string]any); bk["credential_reauth:aws_sso"] != float64(1) {
		t.Errorf("by_kind = %v, want credential_reauth:aws_sso 1", cancels[0].Data["by_kind"])
	}
}

// TestPG_ModelProviderConversion_UpAndDown: up, the converted database is
// recorded at the conversion and a second Migrate() changes nothing; down, a
// wardynd that ships only the migrations before it refuses the database rather
// than read a document it cannot understand, naming the conversion.
func TestPG_ModelProviderConversion_UpAndDown(t *testing.T) {
	const cfg = `{"integrations": [{"id": "key", "kind": "anthropic_api_key"}], "agent_providers": {"agents": [{"id": "claude-code", "mechanism": "anthropic_api_key"}]}}`
	_, _, pool := convertSiteConfig(t, cfg, nil)
	ctx := context.Background()
	var before []byte
	if err := pool.QueryRow(ctx, `SELECT config FROM site_config`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	var after []byte
	if err := pool.QueryRow(ctx, `SELECT config FROM site_config`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("a second Migrate rewrote the converted document:\n%s\n%s", before, after)
	}

	var older []string
	for _, n := range readMigrationNames(t) {
		if n < conversionFloor {
			older = append(older, n)
		}
	}
	unknown, err := unknownAppliedMigrations(ctx, pool, append(older, slices.Sorted(maps.Keys(retiredMigrations))...))
	if err != nil {
		t.Fatal(err)
	}
	if len(unknown) == 0 || !strings.HasPrefix(unknown[0], conversionFloor) {
		t.Errorf("a binary without the conversion sees unknown migrations %v; want it to refuse, naming %s", unknown, conversionFloor)
	}
}

func mapsEqual(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func deref(s *string) string {
	if s == nil {
		return "<NULL>"
	}
	return *s
}
