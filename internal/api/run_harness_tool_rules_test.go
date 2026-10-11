// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// Each harness's set is the policy's rules first, narrowed by the tool's own: the
// stricter effect wins, so a person's rule never relaxes an administrator's, and
// the policy's "*" is the harness's default.
func TestHarnessToolRules_ProductAfterTheCeiling(t *testing.T) {
	spec := types.RunPolicySpec{ToolRules: []types.ToolRule{
		{Tool: "Bash", Effect: types.ToolDeny}, {Tool: "Read", Effect: types.ToolAllow}, {Tool: "*", Effect: types.ToolHold}}}
	tools := []client.IncludedTool{
		{ID: "claude-code", Kind: client.IncludedToolHarness, ToolRules: []types.ToolRule{{Tool: "Bash", Effect: types.ToolHold}, {Tool: "Read", Effect: types.ToolDeny}}, DefaultEffect: types.ToolDeny},
		{ID: "codex-cli", Kind: client.IncludedToolHarness},
		{ID: "mcp", Kind: client.IncludedToolComponent},
	}
	got := harnessToolRulesFor(spec, tools)
	if len(got) != 2 {
		t.Fatalf("sets = %+v, want one per harness and none for a component", got)
	}
	if eff, _ := types.ToolEffectForHarness(got, "claude-code", "Bash"); eff != types.ToolDeny {
		t.Errorf("a person's hold relaxed the administrator's deny: %q", eff)
	}
	if eff, _ := types.ToolEffectForHarness(got, "claude-code", "Read"); eff != types.ToolDeny {
		t.Errorf("a person's deny did not narrow an allow: %q", eff)
	}
	if eff, _ := types.ToolEffectForHarness(got, "claude-code", "Other"); eff != types.ToolDeny {
		t.Errorf("default = %q, want the stricter of the policy's hold and the tool's deny", eff)
	}
	if eff, _ := types.ToolEffectForHarness(got, "codex-cli", "Read"); eff != types.ToolAllow {
		t.Errorf("codex-cli's own set lost the policy's rule, or took claude-code's: %q", eff)
	}
	if eff, _ := types.ToolEffectForHarness(got, "codex-cli", "Bash"); eff != types.ToolDeny {
		t.Errorf("codex-cli Bash = %q, want the policy's deny", eff)
	}
	if harnessToolRulesFor(types.RunPolicySpec{}, tools[1:2]) != nil || harnessToolRulesFor(spec, nil) != nil {
		t.Error("a run with no rule anywhere, or no harness tool, left the older regime")
	}
}

// Dispatch gives the proxy digests and the started harness its plaintext, as
// secret env: the plaintext is in no config, no audit row and no non-secret env,
// and the run-wide tool_rules leave the sidecar's policy.
func TestHarnessToolRules_DispatchCarriesDigestsNeverThePlaintext(t *testing.T) {
	sets := []types.HarnessToolRules{
		{Harness: "claude-code", Rules: []types.ToolRule{{Tool: "Bash", Effect: types.ToolHold}}},
		{Harness: "codex-cli", Default: types.ToolDeny},
	}
	fr := &fakeRunner{}
	srv, _, audit, run := dispatchTeardownFixture(t, fr, types.RunPending)
	run.Task, run.Agent = "", "claude-code"
	srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
		RunToken: "run-token", Image: "wardyn/claude-code:latest",
		Policy:           types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "Bash", Effect: types.ToolDeny}}},
		HarnessToolRules: sets,
	})
	spec := fr.lastSpec
	cred := spec.SecretEnv[toolgateCredentialEnv]
	if cred == "" {
		t.Fatalf("the started harness got no credential: %v", spec.SecretEnv)
	}
	if _, inEnv := spec.Env[toolgateCredentialEnv]; inEnv {
		t.Error("the credential is in the non-secret env")
	}
	sum := sha256.Sum256([]byte(cred))
	pc := spec.ProxyConfig
	if len(pc.HarnessCredentials) != 2 || !slices.Contains(pc.HarnessCredentials, types.HarnessCredential{Harness: "claude-code", SHA256: hex.EncodeToString(sum[:])}) {
		t.Errorf("digests = %+v, want claude-code's digest of its credential", pc.HarnessCredentials)
	}
	if len(pc.Policy.ToolRules) != 0 || len(pc.HarnessToolRules) != 2 {
		t.Errorf("sidecar tool rules: policy %v, per-harness %v", pc.Policy.ToolRules, pc.HarnessToolRules)
	}
	if err := types.ValidateHarnessCredentials(pc.HarnessCredentials); err != nil {
		t.Error(err)
	}
	cfg, _ := json.Marshal(pc)
	if strings.Contains(string(cfg), cred) {
		t.Error("the proxy config carries the plaintext credential")
	}
	for _, ev := range audit.snapshot() {
		if strings.Contains(string(ev.Data), cred) || strings.Contains(ev.Target, cred) {
			t.Errorf("audit row %s carries the plaintext credential", ev.Action)
		}
	}
	for k, v := range spec.Env {
		if strings.Contains(v, cred) {
			t.Errorf("env %s carries the plaintext credential", k)
		}
	}
	// A harness that is only included holds no credential of the started one's.
	fr2 := &fakeRunner{}
	srv2, _, _, run2 := dispatchTeardownFixture(t, fr2, types.RunPending)
	run2.Task, run2.Agent = "", "gemini-cli"
	srv2.dispatchRun(context.Background(), run2, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
		RunToken: "run-token", Image: "img", HarnessToolRules: sets})
	if _, ok := fr2.lastSpec.SecretEnv[toolgateCredentialEnv]; ok || len(fr2.lastSpec.ProxyConfig.HarnessCredentials) != 2 {
		t.Errorf("a harness the run does not start was handed a credential: %v", fr2.lastSpec.SecretEnv)
	}
}
