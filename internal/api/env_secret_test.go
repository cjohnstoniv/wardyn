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

// TestDispatchEnvSplit_CredentialsLeaveEnv pins the control-plane half of the
// F9-H1 fix — the premise every substrate then relies on.
//
// runner.SandboxSpec's Env is documented non-secret and drivers treat it that
// way (the k8s driver writes it inline into a Pod spec, readable by anyone with
// pods/get). resolveEnvSecretGrants breaks that contract by design: it puts a
// REAL stored secret under a variable name. The fix is not to stop resolving it
// but to REPORT it, so splitSecretEnv can move it to SandboxSpec.SecretEnv,
// which drivers must deliver without publishing the value.
//
// Two properties, both load-bearing: the credential lands in exactly one map,
// and it is the secret one. A copy left behind in Env would be the original
// leak with an extra secretKeyRef beside it.
func TestDispatchEnvSplit_CredentialsLeaveEnv(t *testing.T) {
	h, sec := newSecretsHarness(t)
	if err := sec.Put(context.Background(), "corp-token", []byte("s3cr3t-value")); err != nil {
		t.Fatal(err)
	}
	reg := secretmask.NewRegistry()
	h.srv.cfg.MaskRegistry = reg
	run := types.AgentRun{ID: uuid.New()}

	sandboxEnv := map[string]string{"WARDYN_TASK_MODE": "exec", "HTTP_PROXY": "http://wardyn-proxy:3128"}
	policy := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{
		envSecretGrant("CORP_API_TOKEN", "corp-token"),
		envSecretGrant("GONE_TOKEN", "no-such-secret"),
	}}
	names := h.srv.resolveEnvSecretGrants(context.Background(), run, policy, sandboxEnv)

	// Only the grants that actually RESOLVED are named: a skipped grant set no
	// variable, and naming it would move a platform value out of Env.
	if len(names) != 1 || names[0] != "CORP_API_TOKEN" {
		t.Fatalf("resolveEnvSecretGrants returned %v, want exactly [CORP_API_TOKEN]", names)
	}

	secretEnv := splitSecretEnv(sandboxEnv, names)
	if secretEnv["CORP_API_TOKEN"] != "s3cr3t-value" {
		t.Errorf("SecretEnv[CORP_API_TOKEN] = %q, want the resolved secret value", secretEnv["CORP_API_TOKEN"])
	}
	if v, still := sandboxEnv["CORP_API_TOKEN"]; still {
		t.Errorf("Env still carries CORP_API_TOKEN = %q after the split — a driver would publish it inline", v)
	}
	// Platform configuration is untouched: the split moves credentials out, it
	// does not move configuration in.
	if sandboxEnv["WARDYN_TASK_MODE"] != "exec" || sandboxEnv["HTTP_PROXY"] != "http://wardyn-proxy:3128" {
		t.Errorf("non-secret env was disturbed by the split: %v", sandboxEnv)
	}
	if _, moved := secretEnv["HTTP_PROXY"]; moved {
		t.Errorf("SecretEnv swept up a non-secret variable: %v", secretEnv)
	}

	// The drift guard, and the reason this test is worth more than the two
	// assertions above: whatever dispatch thought was worth MASKING out of a
	// recording is exactly what must not sit inline in a pod spec. A future
	// credential lane that forgets to report its keys fails here without anyone
	// having to remember this file exists.
	for k, v := range sandboxEnv {
		for _, masked := range reg.Snapshot(run.ID) {
			if len(masked) > 0 && strings.Contains(v, string(masked)) {
				t.Errorf("Env[%s] carries a mask-registered credential: a lane wrote it without naming it for splitSecretEnv", k)
			}
		}
	}

	// A run with no credential grant produces a nil SecretEnv, so its spec is
	// byte-identical to one composed before the split existed.
	if got := splitSecretEnv(map[string]string{"HTTP_PROXY": "x"}, nil); got != nil {
		t.Errorf("splitSecretEnv with no credential keys = %v, want nil", got)
	}
}
