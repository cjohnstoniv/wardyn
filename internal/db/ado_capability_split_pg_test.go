// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// adoSplitFloor is the Azure DevOps per-area capability split: every test here
// seeds pre-split lists below it and runs the real Migrate() over them.
const adoSplitFloor = "0101"

// adoTwelveReads is the old "read", written out in the order the split emits it.
var adoTwelveReads = []string{"code_read", "work_read", "wiki_read", "build_read", "release_read",
	"serviceendpoint_read", "library_read", "packaging_read", "test_read", "project_read", "identity_read", "analytics_read"}

// adoSplitSeed is what TestPG_ADOCapabilitySplit stores before the split.
type adoSplitSeed struct {
	run                                  uuid.UUID
	policy, governance                   uuid.UUID
	escalation, consent, other, grant    uuid.UUID
	decidedEscalation, otherRunEscalated uuid.UUID
	otherRun                             uuid.UUID
}

const adoSplitSiteConfig = `{"workspace_providers": {"git": [
	{"id": "ado", "kind": "azure_devops", "base_urls": ["https://dev.azure.com/acme"], "lanes": ["entra"],
	 "credential_source": "per_user",
	 "entra": {"tenant_id": "t", "client_id": "c",
	           "capability_ceiling": ["read", "code_write", "work_write", "build_admin", "packaging_write"]}},
	{"id": "ado2", "kind": "azure_devops", "base_urls": ["https://dev.azure.com/other"], "lanes": ["entra"],
	 "credential_source": "per_user", "disabled": true,
	 "entra": {"tenant_id": "t", "client_id": "c", "capability_ceiling": ["pr", "read", "build_execute"],
	           "default_profile": ["build_execute", "read", "pr"]}},
	{"id": "gh", "kind": "github", "base_urls": ["https://github.com/acme"]}
]}, "other": {"kept": true}}`

// splitSiteConfig seeds config (when set) and anything seed adds below the
// split, runs Migrate(), and returns every audit row the migration wrote.
func splitSiteConfig(t *testing.T, config string, seed func(ctx context.Context, pool *pgxpool.Pool)) ([]conversionAudit, *pgxpool.Pool) {
	t.Helper()
	pool, _ := partialSchemaPool(t, adoSplitFloor)
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
		t.Fatalf("Migrate from %s: %v", adoSplitFloor, err)
	}
	rows, err := pool.Query(ctx, `SELECT action, target, COALESCE(data, '{}'::jsonb) FROM audit_events
		WHERE actor = 'wardyn/migration' ORDER BY seq`)
	if err != nil {
		t.Fatalf("read migration audit: %v", err)
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
	return audit, pool
}

func seedADOSplit(t *testing.T, ctx context.Context, pool *pgxpool.Pool) adoSplitSeed {
	t.Helper()
	s := adoSplitSeed{policy: uuid.New(), governance: uuid.New(), escalation: uuid.New(), consent: uuid.New(),
		other: uuid.New(), grant: uuid.New(), decidedEscalation: uuid.New(), otherRunEscalated: uuid.New()}
	var err error
	if s.run, err = insertAgentRun(t, pool, "RUNNING"); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if s.otherRun, err = insertAgentRun(t, pool, "RUNNING"); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	exec := func(what, sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed %s: %v", what, err)
		}
	}
	exec("policy", `INSERT INTO run_policies (id, name, spec) VALUES ($1, 'p', $2)`, s.policy,
		`{"min_confinement_class": "CC2", "azure_devops_capabilities": ["read", "pr", "work_write", "code_read"]}`)
	exec("policy without a list", `INSERT INTO run_policies (id, name, spec) VALUES ($1, 'q', '{"min_confinement_class": "CC2"}')`, uuid.New())
	exec("governance", `INSERT INTO governance_profiles (id, name, ceiling) VALUES ($1, 'g', $2)`, s.governance,
		`{"min_confinement_class": "CC2", "azure_devops_capabilities": ["build_execute", "packaging_write"]}`)
	exec("preset", `INSERT INTO launch_presets (name, request) VALUES ('ado', $1)`,
		`{"agent": "claude-code", "inline_policy": {"min_confinement_class": "CC2", "azure_devops_capabilities": ["build_admin", "read"]}}`)
	exec("preset without a policy", `INSERT INTO launch_presets (name, request) VALUES ('plain', '{"agent": "claude-code"}')`)
	for id, scope := range map[uuid.UUID]string{
		s.escalation: `{"lane": "azure_devops", "capability": "work_write", "grant_id": "` + s.grant.String() + `"}`,
		s.consent:    `{"lane": "azure_devops", "mechanism": "entra_consent", "scopes": ["499b84ac-1321-427f-aa17-267ca6975798/vso.work_write"]}`,
		s.other:      `{"tool": "shell", "cmd": "ls"}`,
	} {
		exec("approval", `INSERT INTO approvals (id, run_id, kind, requested_scope) VALUES ($1, $2, 'tool_call', $3)`, id, s.run, scope)
	}
	exec("other run's escalation", `INSERT INTO approvals (id, run_id, kind, requested_scope) VALUES ($1, $2, 'tool_call', $3)`,
		s.otherRunEscalated, s.otherRun, `{"lane": "azure_devops", "capability": "read"}`)
	exec("decided escalation", `INSERT INTO approvals (id, run_id, kind, requested_scope, state) VALUES ($1, $2, 'tool_call', $3, 'APPROVED')`,
		s.decidedEscalation, s.run, `{"lane": "azure_devops", "capability": "build_execute"}`)
	exec("grant", `INSERT INTO credential_grants (id, run_id, spec) VALUES ($1, $2, $3)`, s.grant, s.run,
		`{"kind": "azure_devops_entra", "scope": {"snapshot": {"capabilities": ["read", "code_write"]}}}`)
	return s
}

// TestPG_ADOCapabilitySplit: every stored list becomes its exact equivalent,
// an empty default is written out as the twelve reads, pending escalations are
// cancelled with one approval.cancel row per run, and nothing else moves.
func TestPG_ADOCapabilitySplit(t *testing.T) {
	var seed adoSplitSeed
	audit, pool := splitSiteConfig(t, adoSplitSiteConfig, func(ctx context.Context, pool *pgxpool.Pool) {
		seed = seedADOSplit(t, ctx, pool)
	})
	ctx := context.Background()

	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT config FROM site_config WHERE singleton`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		WorkspaceProviders struct {
			Git []struct {
				ID    string `json:"id"`
				Entra *struct {
					TenantID          string   `json:"tenant_id"`
					CapabilityCeiling []string `json:"capability_ceiling"`
					DefaultProfile    []string `json:"default_profile"`
				} `json:"entra"`
			} `json:"git"`
		} `json:"workspace_providers"`
		Other map[string]any `json:"other"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	rows := doc.WorkspaceProviders.Git
	if len(rows) != 3 || rows[0].ID != "ado" || rows[1].ID != "ado2" || rows[2].ID != "gh" || doc.Other["kept"] != true {
		t.Fatalf("site config lost its shape: %s", raw)
	}
	wantCeiling := append(slices.Clone(adoTwelveReads), "code_write", "work_write", "work_admin", "build_admin", "release_admin", "packaging_write")
	if e := rows[0].Entra; e == nil || e.TenantID != "t" || !slices.Equal(e.CapabilityCeiling, wantCeiling) || !slices.Equal(e.DefaultProfile, adoTwelveReads) {
		t.Errorf("ado entra = %+v, want ceiling %v and the twelve reads as the default", rows[0].Entra, wantCeiling)
	}
	wantCeiling2 := append(append([]string{"pr"}, adoTwelveReads...), "build_execute", "release_execute")
	wantDefault2 := append(append([]string{"build_execute", "release_execute"}, adoTwelveReads...), "pr")
	if e := rows[1].Entra; e == nil || !slices.Equal(e.CapabilityCeiling, wantCeiling2) || !slices.Equal(e.DefaultProfile, wantDefault2) {
		t.Errorf("ado2 entra = %+v, want ceiling %v default %v", rows[1].Entra, wantCeiling2, wantDefault2)
	}
	if rows[2].Entra != nil {
		t.Errorf("the github row gained an entra block: %s", raw)
	}

	list := func(sql string, args ...any) []string {
		t.Helper()
		var b []byte
		if err := pool.QueryRow(ctx, sql, args...).Scan(&b); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		var out []string
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("decode %s: %v", b, err)
		}
		return out
	}
	if got, want := list(`SELECT spec -> 'azure_devops_capabilities' FROM run_policies WHERE id = $1`, seed.policy),
		append(append([]string{}, adoTwelveReads...), "pr", "work_write", "work_admin"); !slices.Equal(got, want) {
		t.Errorf("policy = %v, want %v (code_read kept once, where read first put it)", got, want)
	}
	var untouched int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_policies WHERE name = 'q' AND NOT spec ? 'azure_devops_capabilities'`).Scan(&untouched); err != nil || untouched != 1 {
		t.Errorf("a policy with no list gained one (count %d, %v)", untouched, err)
	}
	if got, want := list(`SELECT ceiling -> 'azure_devops_capabilities' FROM governance_profiles WHERE id = $1`, seed.governance),
		[]string{"build_execute", "release_execute", "packaging_write"}; !slices.Equal(got, want) {
		t.Errorf("governance = %v, want %v", got, want)
	}
	if got, want := list(`SELECT request #> '{inline_policy,azure_devops_capabilities}' FROM launch_presets WHERE name = 'ado'`),
		append([]string{"build_admin", "release_admin"}, adoTwelveReads...); !slices.Equal(got, want) {
		t.Errorf("preset = %v, want %v", got, want)
	}
	var plain []byte
	if err := pool.QueryRow(ctx, `SELECT request FROM launch_presets WHERE name = 'plain'`).Scan(&plain); err != nil || string(plain) != `{"agent": "claude-code"}` {
		t.Errorf("a preset with no inline policy was rewritten: %s (%v)", plain, err)
	}

	for id, want := range map[uuid.UUID]string{
		seed.escalation: "CANCELLED", seed.otherRunEscalated: "CANCELLED",
		seed.consent: "PENDING", seed.other: "PENDING", seed.decidedEscalation: "APPROVED",
	} {
		var state, reason, by string
		if err := pool.QueryRow(ctx, `SELECT state, COALESCE(reason, ''), COALESCE(decided_by, '') FROM approvals WHERE id = $1`, id).Scan(&state, &reason, &by); err != nil {
			t.Fatal(err)
		}
		if state != want || (want == "CANCELLED" && (reason != "ado_capability_split" || by != "system")) {
			t.Errorf("approval %s = %s/%q/%q, want %s", id, state, reason, by, want)
		}
	}
	var scope []byte
	if err := pool.QueryRow(ctx, `SELECT requested_scope FROM approvals WHERE id = $1`, seed.decidedEscalation).Scan(&scope); err != nil ||
		string(scope) != `{"lane": "azure_devops", "capability": "build_execute"}` {
		t.Errorf("a decided approval's scope was rewritten: %s (%v)", scope, err)
	}
	if got := list(`SELECT spec #> '{scope,snapshot,capabilities}' FROM credential_grants WHERE id = $1`, seed.grant); !slices.Equal(got, []string{"read", "code_write"}) {
		t.Errorf("grant snapshot = %v, want the record of what was granted untouched", got)
	}

	cancels := map[string]map[string]any{}
	for _, a := range audit {
		if a.Action == "approval.cancel" {
			cancels[a.Target] = a.Data
		}
	}
	for _, run := range []uuid.UUID{seed.run, seed.otherRun} {
		d := cancels[run.String()]
		if d == nil || d["reason"] != "ado_capability_split" || d["count"] != float64(1) {
			t.Errorf("approval.cancel for run %s = %v, want one, reason ado_capability_split", run, d)
		}
	}
	if len(cancels) != 2 {
		t.Errorf("approval.cancel rows = %v, want one per run with a cancelled escalation", cancels)
	}
}

// TestPG_ADOCapabilitySplit_SecondRunIsANoOp: the split over lists it already
// wrote changes nothing, and a database with no site config migrates cleanly.
func TestPG_ADOCapabilitySplit_SecondRunIsANoOp(t *testing.T) {
	_, pool := splitSiteConfig(t, adoSplitSiteConfig, func(ctx context.Context, pool *pgxpool.Pool) {
		seedADOSplit(t, ctx, pool)
	})
	ctx := context.Background()
	snapshot := func() string {
		t.Helper()
		var out string
		if err := pool.QueryRow(ctx, `SELECT (SELECT config::text FROM site_config)
			|| (SELECT string_agg(spec::text, '|' ORDER BY name) FROM run_policies)
			|| (SELECT string_agg(ceiling::text, '|' ORDER BY name) FROM governance_profiles)
			|| (SELECT string_agg(request::text, '|' ORDER BY name) FROM launch_presets)`).Scan(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := snapshot()
	sql, err := migrationFS.ReadFile("migrations/0101_ado_capability_split.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if after := snapshot(); after != before {
		t.Errorf("a second run rewrote migrated data:\n%s\n%s", before, after)
	}

	splitSiteConfig(t, "", nil) // a database with no site config migrates cleanly (Fatal otherwise)
}
