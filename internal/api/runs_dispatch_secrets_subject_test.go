// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// dispatchSubjectFixture seeds a secret under the NAMED principal's own
// namespace and returns a Server plus the run that names them as CreatedBy.
//
// CreatedBy is the ATTRIBUTION — in LocalMode it is whatever the DEV-ONLY
// X-Wardyn-Principal header said (actorFromRequest's case 1). The SUBJECT, the
// thing that selects a secret namespace, comes from runIdentitySubject, which
// no request header can move (F099, runs_policy.go).
func dispatchSubjectFixture(t *testing.T, victim, secretName string) (*Server, *recRecorder, types.AgentRun) {
	t.Helper()
	h := newHarness(t)
	secrets := &memSecrets{m: map[string][]byte{}, owned: map[string]map[string][]byte{}}
	if err := secrets.For(victim).Put(context.Background(), secretName, []byte("alice-plaintext")); err != nil {
		t.Fatalf("seed %s's secret: %v", victim, err)
	}
	audit := &recRecorder{}
	cfg := baseTestConfig(h, nil)
	cfg.Audit = audit
	cfg.Secrets = secrets
	return New(cfg), audit, types.AgentRun{ID: uuid.New(), CreatedBy: victim, Agent: "claude-code"}
}

// TestDispatchSecrets_ResolveTheRunSubjectNotTheAttribution is B2-F2.
//
// Both dispatch-side secret resolvers — the llm_inspection detection corpus and
// the env_secret grants — read Secrets.For(run.CreatedBy). CreatedBy is the
// attribution string, which in LocalMode a caller writes into a header, while
// every other credential-bearing path on the run resolves its namespace through
// runIdentitySubject. Off LocalMode the two expressions are identical; inside
// it they are not, and a run could be dispatched against a namespace the run's
// own identity was never minted for.
func TestDispatchSecrets_ResolveTheRunSubjectNotTheAttribution(t *testing.T) {
	const (
		operator   = "local:operator"
		victim     = "sub-alice@corp.example"
		secretName = "alice-token"
	)
	srv, audit, run := dispatchSubjectFixture(t, victim, secretName)
	// The run identity was minted for the INJECTED local operator, so this is
	// the context dispatch carries (handleCreateRun's ctx survives WithoutCancel).
	ctx := withLocalPrincipal(context.Background(), operator)

	scope, _ := json.Marshal(map[string]string{"name": "MY_VAR", "secret_name": secretName})
	policy := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{
		{Kind: types.GrantEnvSecret, Scope: scope},
	}}
	sandboxEnv := map[string]string{}
	srv.resolveEnvSecretGrants(ctx, run, policy, sandboxEnv)

	if v, ok := sandboxEnv["MY_VAR"]; ok {
		t.Errorf("MY_VAR = %q — the header-supplied attribution selected %s's own secret namespace", v, victim)
	}
	var sawRefusal bool
	for _, ev := range audit.events {
		if ev.Action != "run.env_secret.resolve" {
			continue
		}
		if ev.Outcome == "failure" && strings.Contains(string(ev.Data), "could not be resolved") {
			sawRefusal = true
		}
	}
	if !sawRefusal {
		t.Error("no run.env_secret.resolve failure row: a grant that resolved nothing must say so")
	}

	// The llm_inspection corpus is the SAME question asked by the sibling
	// resolver two functions up.
	li := &types.LLMInspectionSpec{WorkspaceSecretNames: []string{secretName}}
	insp := types.RunPolicySpec{LLMInspection: li}
	srv.resolveLLMInspectionSecrets(ctx, run, &insp)
	if len(li.WorkspaceSecretValues) != 0 {
		t.Errorf("llm_inspection resolved %d value(s) from the attribution's namespace", len(li.WorkspaceSecretValues))
	}
}

// TestDispatchSecrets_OperatorRunIsUnchanged is B2-F2's negative control: with
// no local principal on the context (every OIDC and admin-token caller)
// runIdentitySubject returns CreatedBy unchanged, so the resolution is
// byte-identical to what it has always been.
func TestDispatchSecrets_OperatorRunIsUnchanged(t *testing.T) {
	const owner, secretName = "sub-alice@corp.example", "alice-token"
	srv, _, run := dispatchSubjectFixture(t, owner, secretName)

	scope, _ := json.Marshal(map[string]string{"name": "MY_VAR", "secret_name": secretName})
	policy := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{
		{Kind: types.GrantEnvSecret, Scope: scope},
	}}
	sandboxEnv := map[string]string{}
	srv.resolveEnvSecretGrants(context.Background(), run, policy, sandboxEnv)

	if sandboxEnv["MY_VAR"] != "alice-plaintext" {
		t.Errorf("MY_VAR = %q, want the owner's own secret — an ordinary run must be unaffected", sandboxEnv["MY_VAR"])
	}
}
