// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func envSecretGrant(name, secretName string) types.GrantSpec {
	return types.GrantSpec{
		Kind:  types.GrantEnvSecret,
		Scope: mustJSON(map[string]any{"name": name, "secret_name": secretName}),
	}
}

// TestEnvSecretScope_WriteTimeRules covers the two things nothing downstream can
// undo: the variable name is written verbatim into a process environment, and
// the secret name is read verbatim out of the store.
func TestEnvSecretScope_WriteTimeRules(t *testing.T) {
	for _, c := range []struct {
		name    string
		grant   types.GrantSpec
		wantErr bool
	}{
		{"ok", envSecretGrant("CORP_API_TOKEN", "corp-token"), false},
		{"lower case refused", envSecretGrant("corp_token", "corp-token"), true},
		{"leading digit refused", envSecretGrant("1TOKEN", "corp-token"), true},
		{"dash refused", envSecretGrant("CORP-TOKEN", "corp-token"), true},
		{"empty name refused", envSecretGrant("", "corp-token"), true},
		{"empty secret refused", envSecretGrant("CORP_TOKEN", ""), true},
		// The harness's own namespace: an env_secret authoring WARDYN_TASK_MODE
		// would be a dispatch-config override wearing a credential's clothes.
		{"WARDYN_ prefix refused", envSecretGrant("WARDYN_TASK_MODE", "corp-token"), true},
		{"reserved secret refused", envSecretGrant("CORP_TOKEN", "wardyn-signing-key"), true},
	} {
		err := validateEligibleGrant(0, c.grant)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: validateEligibleGrant err=%v, wantErr=%v", c.name, err, c.wantErr)
		}
	}

	// requires_approval is REFUSED, not ignored: env_secret is resolved at
	// dispatch, so there is no mint for an approval to gate and accepting the
	// flag would advertise a human gate that never fires.
	gated := envSecretGrant("CORP_API_TOKEN", "corp-token")
	gated.RequiresApproval = true
	if err := validateEligibleGrant(0, gated); err == nil {
		t.Error("env_secret with requires_approval=true: want an error, got nil")
	}
}

// TestFilterMemberGrants_EnvSecretIsAdminOnly is the posture gate: a member's
// env_secret grant is dropped even when the operator's ceiling lists the exact
// (name, secret) pairing, until the deployment opens envAllowMemberEnvSecret.
func TestFilterMemberGrants_EnvSecretIsAdminOnly(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.DefaultPolicy = types.RunPolicySpec{
		EligibleGrants: []types.GrantSpec{envSecretGrant("CORP_API_TOKEN", "corp-token")},
	}
	listed := envSecretGrant("CORP_API_TOKEN", "corp-token")

	kept, warns, code, err := h.srv.filterMemberGrants(context.Background(), "", nil, []types.GrantSpec{listed})
	if len(kept) != 0 || len(warns) != 1 || code != 0 || err != nil {
		t.Fatalf("default posture: kept=%d warns=%d code=%d err=%v, want (0,1,0,nil) — env_secret is admin-only",
			len(kept), len(warns), code, err)
	}

	t.Setenv(envAllowMemberEnvSecret, "1")
	if kept, _, _, _ := h.srv.filterMemberGrants(context.Background(), "", nil, []types.GrantSpec{listed}); len(kept) != 1 {
		t.Fatalf("posture open, ceiling-listed pairing: kept=%d, want 1", len(kept))
	}
	// Still bounded by the ceiling pairing once open: the NAME is part of the
	// match, so an operator-blessed secret cannot be re-homed to a variable the
	// operator never wrote.
	if kept, _, _, _ := h.srv.filterMemberGrants(context.Background(), "", nil, []types.GrantSpec{envSecretGrant("OTHER_VAR", "corp-token")}); len(kept) != 0 {
		t.Fatalf("posture open, unlisted variable name: kept=%d, want 0", len(kept))
	}
	if kept, _, _, _ := h.srv.filterMemberGrants(context.Background(), "", nil, []types.GrantSpec{envSecretGrant("CORP_API_TOKEN", "prod-db-password")}); len(kept) != 0 {
		t.Fatalf("posture open, unlisted secret: kept=%d, want 0", len(kept))
	}
}

// TestResolveEnvSecretGrants covers the dispatch sink: the value lands in the
// sandbox env, is mask-registered, never reaches the audit stream, and a grant
// that cannot be honored is SKIPPED rather than substituted or blank-set.
func TestResolveEnvSecretGrants(t *testing.T) {
	h, sec := newSecretsHarness(t)
	if err := sec.Put(context.Background(), "corp-token", []byte("s3cr3t-value")); err != nil {
		t.Fatal(err)
	}
	reg := secretmask.NewRegistry()
	h.srv.cfg.MaskRegistry = reg
	run := types.AgentRun{ID: uuid.New()}

	env := map[string]string{"WARDYN_TASK_MODE": "exec", "CORP_API_TOKEN": ""}
	policy := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{
		envSecretGrant("CORP_API_TOKEN", "corp-token"),
		envSecretGrant("GONE_TOKEN", "no-such-secret"),
	}}
	h.srv.resolveEnvSecretGrants(context.Background(), run, policy, env)

	if env["CORP_API_TOKEN"] != "s3cr3t-value" {
		t.Fatalf("CORP_API_TOKEN = %q, want the resolved secret value", env["CORP_API_TOKEN"])
	}
	if _, set := env["GONE_TOKEN"]; set {
		t.Errorf("GONE_TOKEN = %q, want ABSENT — an unresolvable grant is skipped, not blank-set", env["GONE_TOKEN"])
	}
	masked := false
	for _, v := range reg.Snapshot(run.ID) {
		masked = masked || string(v) == "s3cr3t-value"
	}
	if !masked {
		t.Error("resolved env_secret value was not mask-registered for the run")
	}
	// The value never enters the audit stream — name and secret_name only.
	for _, ev := range h.audit.events {
		if ev.Action == "run.env_secret.resolve" && strings.Contains(string(ev.Data), "s3cr3t-value") {
			t.Fatalf("run.env_secret.resolve audit carries the secret VALUE: %s", ev.Data)
		}
	}

	// A grant may not OVERWRITE a variable dispatch already set: the WARDYN_
	// prefix is refused at write time, but everything else platform-authored
	// (ANTHROPIC_*, artifact config, ExtraEnv) is covered only by this check.
	env2 := map[string]string{"ANTHROPIC_BASE_URL": "https://api.anthropic.com"}
	h.srv.resolveEnvSecretGrants(context.Background(), run,
		types.RunPolicySpec{EligibleGrants: []types.GrantSpec{envSecretGrant("ANTHROPIC_BASE_URL", "corp-token")}}, env2)
	if env2["ANTHROPIC_BASE_URL"] != "https://api.anthropic.com" {
		t.Fatalf("ANTHROPIC_BASE_URL = %q, want the platform value — a grant must not override platform-authored env", env2["ANTHROPIC_BASE_URL"])
	}
}
