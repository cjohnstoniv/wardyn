// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestResolveLLMInspectionSecrets_ReservedNamesRefused is the F126 regression.
// resolveLLMInspectionSecrets resolves each workspace_secret_name to PLAINTEXT
// on the policy copy dispatch hands the proxy sidecar, which makes it a
// credential SINK — and sinkReservedSecret is the guard every sink takes
// ("reject it at every sink", secrets.go), which its direct sibling in the same
// file (resolveEnvSecretGrants) already carried and this lane did not. Without
// it, a stored policy, an inline policy or WARDYN_DEFAULT_POLICY could ship
// wardyn-signing-key / wardyn-session-key / the harness OAuth blob / the
// resident AWS SigV4 secrets out of the store in cleartext.
func TestResolveLLMInspectionSecrets_ReservedNamesRefused(t *testing.T) {
	h := newHarness(t)
	sec := &memSecrets{m: map[string][]byte{
		"wardyn-signing-key":        []byte("SIGNING-KEY-PLAINTEXT-0123456789"),
		"wardyn-session-key":        []byte("SESSION-KEY-PLAINTEXT-0123456789"),
		"aws-secret-access-key":     []byte("AWS-SIGV4-SECRET-0123456789"),
		"wardyn-harness-aws-oauth":  []byte(`{"access_token":"AWS-SSO-BLOB"}`),
		"ordinary-workspace-secret": []byte("ordinary-value-0123456789"),
	}}
	cfg := h.srv.cfg
	cfg.Secrets = sec
	srv := New(cfg)

	names := []string{
		"wardyn-signing-key", "wardyn-session-key", "aws-secret-access-key",
		"wardyn-harness-aws-oauth", "ordinary-workspace-secret",
	}
	spec := &types.RunPolicySpec{LLMInspection: &types.LLMInspectionSpec{
		Mode: "alert", DetectSecrets: true, WorkspaceSecretNames: names,
	}}
	run := types.AgentRun{ID: uuid.New(), CreatedBy: "alice@example.com"}
	srv.resolveLLMInspectionSecrets(context.Background(), run, spec)

	got := spec.LLMInspection.WorkspaceSecretValues
	if len(got) != 1 || got[0] != "ordinary-value-0123456789" {
		t.Fatalf("resolved values = %d entries %v, want only the one non-reserved secret", len(got), got)
	}
	for _, v := range got {
		for _, leak := range []string{"SIGNING-KEY", "SESSION-KEY", "AWS-SIGV4", "AWS-SSO-BLOB"} {
			if strings.Contains(v, leak) {
				t.Errorf("a reserved platform secret reached the proxy sidecar's policy copy (%s)", leak)
			}
		}
	}
}

// TestValidateLLMInspection_ReservedNameRefusedAtWrite is the write-time half:
// an operator authoring a reserved name gets a refusal, not a run whose scanner
// silently covers one fewer value than they asked for.
func TestValidateLLMInspection_ReservedNameRefusedAtWrite(t *testing.T) {
	for _, name := range []string{"wardyn-signing-key", "wardyn-session-key", "aws-secret-access-key"} {
		spec := types.RunPolicySpec{LLMInspection: &types.LLMInspectionSpec{
			Mode: "alert", DetectSecrets: true, WorkspaceSecretNames: []string{name},
		}}
		if err := validateLLMInspection(spec); err == nil {
			t.Errorf("validateLLMInspection(workspace_secret_names=[%q]) = nil, want a refusal", name)
		}
	}
	ok := types.RunPolicySpec{LLMInspection: &types.LLMInspectionSpec{
		Mode: "alert", DetectSecrets: true, WorkspaceSecretNames: []string{"corp-api-token"},
	}}
	if err := validateLLMInspection(ok); err != nil {
		t.Errorf("an ordinary workspace secret name must still be allowed: %v", err)
	}
}
