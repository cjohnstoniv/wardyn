// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// perEntryConfig is a stored proxy config that carries its rules per entry, as dispatch writes it for a run
// with a repository and two harnesses: sets and a base, and no run-wide block.
func perEntryConfig(t *testing.T) *proxy.Config {
	t.Helper()
	return &proxy.Config{RunID: uuid.New(), RunToken: "tok", ControlPlaneURL: "https://wardynd:8443", ControlPlaneCAPEM: string(testHopCA(t).CertPEM),
		PushRuleSets: []types.PushRuleSet{{Provider: "github", Host: "github.com", Org: "acme",
			Rules: types.PushRulesSpec{DenyPaths: []string{"ci/**"}, MaxFileSizeMiB: 50}}},
		PushRuleBase: &types.PushRulesSpec{DenyPaths: []string{"ci/**"}, MaxFileSizeMiB: 50},
		HarnessToolRules: []types.HarnessToolRules{
			{Harness: "claude-code", Default: types.ToolAllow, Rules: []types.ToolRule{{Tool: "Bash", Effect: types.ToolAllow}}},
			{Harness: "codex-cli", Rules: []types.ToolRule{{Tool: "Read", Effect: types.ToolAllow}}},
		}}
}

// TestRevivedPerEntryRulesMeetRestriction: a revive of a run that carries per-entry push rule sets and
// per-harness tool rules, under a restriction that names push_rules and tool_rules, still loads. The seal writes
// the restriction's rules into the run-wide block, which the proxy refuses beside the per-entry form, so they
// are met into every set, the base and every harness's rules instead, and the run-wide block stays off.
func TestRevivedPerEntryRulesMeetRestriction(t *testing.T) {
	deny := []string{".github/**"}
	exec, tenMiB := true, 10
	layers := []sealLayer{restrictionSealLayer(types.Restriction{Name: "locked",
		Spec: types.RestrictionSpec{
			PushRules: &types.PushRulesOverlay{DenyPaths: &deny, DenyNewExecutables: &exec, MaxFileSizeMiB: &tenMiB},
			ToolRules: &[]types.ToolRule{{Tool: "Bash", Effect: types.ToolDeny}},
		}})}

	cfg := perEntryConfig(t)
	sealPolicy(&cfg.Policy, layers) // sealRunPolicy's step: the restriction lands in the run-wide block
	if cfg.Policy.PushRules == nil || len(cfg.Policy.ToolRules) == 0 {
		t.Fatalf("the seal wrote push rules %+v tool rules %+v, want both (the precondition of the defect)", cfg.Policy.PushRules, cfg.Policy.ToolRules)
	}
	if _, rerr := encodeRevivedConfig(context.Background(), cfg); rerr == nil {
		t.Fatal("the sealed config loads without the per-entry seal; the test no longer pins the defect")
	}

	sealPerEntryRules(cfg, layers)
	if _, rerr := encodeRevivedConfig(context.Background(), cfg); rerr != nil {
		t.Fatalf("the revived config does not load: %+v", rerr)
	}
	if cfg.Policy.PushRules != nil || cfg.Policy.ToolRules != nil {
		t.Errorf("run-wide block kept: push rules %+v tool rules %+v", cfg.Policy.PushRules, cfg.Policy.ToolRules)
	}
	for name, r := range map[string]*types.PushRulesSpec{"set": &cfg.PushRuleSets[0].Rules, "base": cfg.PushRuleBase} {
		if r == nil || !slices.Equal(r.DenyPaths, []string{".github/**", "ci/**"}) || !r.DenyNewExecutables || r.MaxFileSizeMiB != 10 {
			t.Errorf("%s = %+v, want the set's paths and the restriction's, executables denied, the smaller cap 10", name, r)
		}
	}
	for _, h := range cfg.HarnessToolRules {
		if !hasToolEffect(h.Rules, "Bash", types.ToolDeny) {
			t.Errorf("%s rules = %+v, want Bash denied", h.Harness, h.Rules)
		}
		if types.ToolEffectStrictness(h.Default) < types.ToolEffectStrictness(types.ToolHold) {
			t.Errorf("%s default %q, want at least hold: the restriction names tool rules", h.Harness, h.Default)
		}
	}
}

// TestSealPerEntryRulesLeavesAPlainConfigAlone: a config with no per-entry rules keeps the run-wide block the
// seal wrote, and a layer with no push or tool rules changes no set.
func TestSealPerEntryRulesLeavesAPlainConfigAlone(t *testing.T) {
	layers := []sealLayer{listLayer("walls", []string{"a.example"})}
	plain := &proxy.Config{Policy: types.RunPolicySpec{PushRules: &types.PushRulesSpec{DenyAll: true}, ToolRules: []types.ToolRule{{Tool: "Bash", Effect: types.ToolDeny}}}}
	sealPerEntryRules(plain, layers)
	if plain.Policy.PushRules == nil || len(plain.Policy.ToolRules) != 1 || plain.PushRuleBase != nil {
		t.Errorf("plain config = %+v base %+v, want the run-wide block kept", plain.Policy, plain.PushRuleBase)
	}
	cfg := perEntryConfig(t)
	want := jsonOf(t, cfg)
	sealPerEntryRules(cfg, layers)
	if got := jsonOf(t, cfg); got != want {
		t.Errorf("a layer with no push or tool rules changed the per-entry rules:\n got %s\nwant %s", got, want)
	}
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
