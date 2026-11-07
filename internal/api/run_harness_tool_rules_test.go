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
	_, got := applyHarnessToolRules(spec, tools, types.ToolHold)
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
	plain := []client.IncludedTool{{ID: "a", Kind: client.IncludedToolHarness}, {ID: "b", Kind: client.IncludedToolHarness}}
	if _, none := applyHarnessToolRules(types.RunPolicySpec{}, plain, types.ToolHold); none != nil {
		t.Error("a run with no rule anywhere left the older regime")
	}
	if _, none := applyHarnessToolRules(spec, nil, types.ToolHold); none != nil {
		t.Error("a run with no harness tool left the older regime")
	}
}

// Dispatch gives the proxy digests and the started harness its plaintext, as an
// agent-owned file under a reserved name: the plaintext is in no config, no audit
// row and no env, secret or not, and the run-wide tool_rules leave the sidecar's
// policy.
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
		HarnessToolRules: sets, ToolApprovals: "hold",
	})
	spec := fr.lastSpec
	cred := toolgateCredentialOf(t, spec)
	for k, v := range spec.SecretEnv {
		if strings.Contains(v, cred) {
			t.Errorf("secret env %s carries the plaintext credential", k)
		}
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
		RunToken: "run-token", Image: "img", HarnessToolRules: sets, ToolApprovals: "hold"})
	if len(fr2.lastSpec.ManagedFiles) != 0 || len(fr2.lastSpec.ProxyConfig.HarnessCredentials) != 2 {
		t.Errorf("a harness the run does not start was handed a credential: %+v", fr2.lastSpec.ManagedFiles)
	}
}

// A named rule is compared with the tool's EFFECTIVE effect, the exact rule else
// "*": a person's or a tool's hold on a tool the policy's "*" denies must not
// shadow the deny, run-wide (overrides) or per harness.
func TestHarnessToolRules_NamedHoldNeverShadowsAWildcardDeny(t *testing.T) {
	// Per harness: the policy (met with the ceiling) denies everything.
	spec := types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "*", Effect: types.ToolDeny}}}
	tools := []client.IncludedTool{
		{ID: "claude-code", Kind: client.IncludedToolHarness, ToolRules: []types.ToolRule{{Tool: "Bash", Effect: types.ToolHold}}},
		{ID: "codex-cli", Kind: client.IncludedToolHarness},
	}
	_, sets := applyHarnessToolRules(spec, tools, types.ToolHold)
	for _, h := range []string{"claude-code", "codex-cli"} {
		if eff, _ := types.ToolEffectForHarness(sets, h, "Bash"); eff != types.ToolDeny {
			t.Errorf("%s Bash = %q after a hold on it, want the policy's deny (sets %+v)", h, eff, sets)
		}
	}
	// Run-wide, through the overrides.
	run := types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "*", Effect: types.ToolDeny}}}
	e := &overrideEdits{}
	applyToolRuleOverrides(&run, []types.ToolRule{{Tool: "Bash", Effect: types.ToolHold}}, e)
	if eff := effectiveToolEffect(run.ToolRules, "Bash"); eff != types.ToolDeny {
		t.Errorf("Bash = %q after an override hold, want deny (rules %+v)", eff, run.ToolRules)
	}
	if len(e.rows) != 0 || e.applied.Agent != nil {
		t.Errorf("a no-op hold was recorded: rows %+v applied %+v", e.rows, e.applied)
	}
	// A hold beneath an exact allow still tightens it, and a deny beats a hold.
	run = types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "Bash", Effect: types.ToolAllow}, {Tool: "*", Effect: types.ToolHold}}}
	applyToolRuleOverrides(&run, []types.ToolRule{{Tool: "Bash", Effect: types.ToolHold}, {Tool: "Web", Effect: types.ToolDeny}}, &overrideEdits{})
	if effectiveToolEffect(run.ToolRules, "Bash") != types.ToolHold || effectiveToolEffect(run.ToolRules, "Web") != types.ToolDeny {
		t.Errorf("rules = %+v", run.ToolRules)
	}
}

// With one harness the run keeps the run-wide regime: the tool's rules and default
// are compiled into spec.ToolRules and nothing per-harness is carried; the
// per-harness sets start at two harnesses. The person's default reaches a tool
// the policy allows, since each tool takes the stricter of the two layers.
func TestHarnessToolRules_OneHarnessKeepsTheRunWideRegime(t *testing.T) {
	spec := types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "Read", Effect: types.ToolAllow}}}
	one := []client.IncludedTool{{ID: "claude-code", Kind: client.IncludedToolHarness,
		ToolRules: []types.ToolRule{{Tool: "Bash", Effect: types.ToolHold}}, DefaultEffect: types.ToolHold}}
	out, sets := applyHarnessToolRules(spec, one, types.ToolHold)
	if sets != nil {
		t.Fatalf("one harness carried per-harness sets: %+v", sets)
	}
	if effectiveToolEffect(out.ToolRules, "Bash") != types.ToolHold || effectiveToolEffect(out.ToolRules, "Other") != types.ToolHold ||
		effectiveToolEffect(out.ToolRules, "Read") != types.ToolHold {
		t.Errorf("run-wide rules = %+v, want the tool's hold and its default, which is stricter than the policy's allow", out.ToolRules)
	}
	if len(spec.ToolRules) != 1 {
		t.Error("the caller's spec was edited in place")
	}
	two := append(one, client.IncludedTool{ID: "codex-cli", Kind: client.IncludedToolHarness})
	if _, sets := applyHarnessToolRules(spec, two, types.ToolHold); len(sets) != 2 {
		t.Errorf("two harnesses carried %d sets, want 2", len(sets))
	}
}

// TestSidecarPolicyLeavesOwnerOnlyRefsOff: the sidecar never reads
// owner_only_refs (the broker reads it from the grant row), so its config
// leaves the key off for a proxy that predates it, without editing the run's
// own policy.
func TestSidecarPolicyLeavesOwnerOnlyRefsOff(t *testing.T) {
	policy := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{{Kind: types.GrantSSHKey, OwnerOnlyRefs: []string{"my-key"}}}}
	raw, err := json.Marshal(sidecarPolicy(policy, dispatchParams{}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "owner_only_refs") {
		t.Fatalf("sidecar policy = %s, want no owner_only_refs", raw)
	}
	if !slices.Equal(policy.EligibleGrants[0].OwnerOnlyRefs, []string{"my-key"}) {
		t.Fatalf("run policy grant = %+v, want its owner_only_refs kept", policy.EligibleGrants[0])
	}
}

// permutations returns every ordering of rules.
func permutations(rules []types.ToolRule) [][]types.ToolRule {
	if len(rules) <= 1 {
		return [][]types.ToolRule{slices.Clone(rules)}
	}
	var out [][]types.ToolRule
	for i := range rules {
		rest := slices.Concat(rules[:i:i], rules[i+1:])
		for _, p := range permutations(rest) {
			out = append(out, append([]types.ToolRule{rules[i]}, p...))
		}
	}
	return out
}

// layerRank reads a layer's effect for a tool; a layer with no opinion allows it.
func layerRank(rules []types.ToolRule, tool string) int {
	return max(types.ToolEffectStrictness(effectiveToolEffect(rules, tool)), types.ToolEffectStrictness(types.ToolAllow))
}

// A walls document's rules compose the same way in any order: a rule for a named tool beats "*" within
// the document, a stricter rule the run already has is kept, and the composed result is never wider than
// the rules the run had nor than the document, for any tool.
func TestNarrowToolRulesAreOrderIndependentAndNeverWiden(t *testing.T) {
	bash := func(e types.ToolEffect) types.ToolRule { return types.ToolRule{Tool: "Bash", Effect: e} }
	star := func(e types.ToolEffect) types.ToolRule { return types.ToolRule{Tool: "*", Effect: e} }

	for _, order := range [][]types.ToolRule{{star(types.ToolDeny), bash(types.ToolHold)}, {bash(types.ToolHold), star(types.ToolDeny)}} {
		got := narrowToolRules(nil, order)
		if effectiveToolEffect(got, "Bash") != types.ToolHold || effectiveToolEffect(got, "Other") != types.ToolDeny {
			t.Errorf("%v over no rules: %+v, want Bash held and every other tool denied", order, got)
		}
		got = narrowToolRules([]types.ToolRule{bash(types.ToolDeny)}, order)
		if effectiveToolEffect(got, "Bash") != types.ToolDeny || effectiveToolEffect(got, "Other") != types.ToolDeny {
			t.Errorf("%v over an inherited Bash deny: %+v, want Bash still denied", order, got)
		}
	}

	bases := [][]types.ToolRule{
		nil,
		{star(types.ToolDeny)},
		{star(types.ToolAllow), bash(types.ToolDeny)},
		{bash(types.ToolAllow)},
		{star(types.ToolHold), {Tool: "Read", Effect: types.ToolAllow}, {Tool: "Web", Effect: types.ToolDeny}},
	}
	var options []types.ToolRule
	for _, tool := range []string{"*", "Bash", "Read", "Web"} {
		for _, e := range []types.ToolEffect{types.ToolAllow, types.ToolHold, types.ToolDeny} {
			options = append(options, types.ToolRule{Tool: tool, Effect: e})
		}
	}
	docs := [][]types.ToolRule{nil}
	for _, a := range options {
		docs = append(docs, []types.ToolRule{a})
		for _, b := range options {
			docs = append(docs, []types.ToolRule{a, b})
			for _, c := range options {
				docs = append(docs, []types.ToolRule{a, b, c})
			}
		}
	}
	probes := []string{"*", "Bash", "Read", "Web", "Other"}
	for bi, base := range bases {
		for _, doc := range docs {
			var first []types.ToolRule
			for i, order := range permutations(doc) {
				got := narrowToolRules(base, order)
				if i == 0 {
					first = got
				}
				for _, tool := range probes {
					if a, b := effectiveToolEffect(first, tool), effectiveToolEffect(got, tool); a != b {
						t.Fatalf("base %d, %v vs %v: %s is %q then %q, want the same in any order", bi, doc, order, tool, a, b)
					}
					if rank := layerRank(got, tool); rank < layerRank(base, tool) || rank < layerRank(order, tool) {
						t.Fatalf("base %d, %v: %s composes to %+v, wider than its rules before (%+v) or the document", bi, order, tool, got, base)
					}
				}
			}
		}
	}
}
