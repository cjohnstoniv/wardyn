// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func secretsWith(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

func scopeFields(t *testing.T, g types.GrantSpec) (header, format, secret string) {
	t.Helper()
	var sc struct {
		Header     string `json:"header"`
		Format     string `json:"format"`
		SecretName string `json:"secret_name"`
	}
	if err := json.Unmarshal(g.Scope, &sc); err != nil {
		t.Fatalf("bad scope json: %v", err)
	}
	return sc.Header, sc.Format, sc.SecretName
}

// ── ensureLLMGrant (pre-clamp; adds grant + its exact-host egress entry, no warning) ──

// WITH the anthropic key, a composed claude-code run gets an auto-mint api_key grant
// for api.anthropic.com AND the exact host in AllowedDomains (the injector requires
// the exact allowlist entry, else the proxy fails closed at startup).
func TestEnsureLLMGrant_AddsGrantAndEgressWhenSecretPresent(t *testing.T) {
	spec := types.RunPolicySpec{}
	ensureLLMGrant(&spec, "claude-code", secretsWith("anthropic-api-key"), false)

	g, ok := apiKeyGrantForHost(&spec, "api.anthropic.com")
	if !ok {
		t.Fatalf("expected an api_key grant for api.anthropic.com; grants=%+v", spec.EligibleGrants)
	}
	if g.RequiresApproval {
		t.Error("model-access api_key grant must be auto-mint (requires_approval=false)")
	}
	header, format, secret := scopeFields(t, g)
	if header != "x-api-key" || format != "%s" || secret != "anthropic-api-key" {
		t.Errorf("anthropic api_key scope wrong: header=%q format=%q secret=%q", header, format, secret)
	}
	// The coupled egress entry MUST be present (buildInjector -> AllowedExactHost).
	if !domainAllowedExact(spec.AllowedDomains, "api.anthropic.com") {
		t.Errorf("api.anthropic.com must be added to AllowedDomains alongside the grant; got %v", spec.AllowedDomains)
	}
}

// Without the secret, NO grant and NO egress entry are added (adding the grant would
// fail the proxy at startup — resolveInjection cannot resolve a missing secret).
func TestEnsureLLMGrant_NoGrantWhenSecretAbsent(t *testing.T) {
	spec := types.RunPolicySpec{}
	ensureLLMGrant(&spec, "claude-code", secretsWith(), false)
	if _, ok := apiKeyGrantForHost(&spec, "api.anthropic.com"); ok {
		t.Fatal("must NOT add an api_key grant when its secret is absent")
	}
	if domainAllowedExact(spec.AllowedDomains, "api.anthropic.com") {
		t.Error("must NOT add an egress entry when no grant is added")
	}
}

// ensureLLMGrant does not duplicate an egress entry the proposal already carries.
func TestEnsureLLMGrant_DedupsEgress(t *testing.T) {
	spec := types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}}
	ensureLLMGrant(&spec, "claude-code", secretsWith("anthropic-api-key"), false)
	n := 0
	for _, d := range spec.AllowedDomains {
		if d == "api.anthropic.com" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("api.anthropic.com egress entry duplicated: %v", spec.AllowedDomains)
	}
}

func TestEnsureLLMGrant_UnknownAgentNoop(t *testing.T) {
	spec := types.RunPolicySpec{}
	ensureLLMGrant(&spec, "some-other-agent", secretsWith("anthropic-api-key"), false)
	if len(spec.EligibleGrants) != 0 || len(spec.AllowedDomains) != 0 {
		t.Errorf("unknown agent must be a no-op; grants=%+v domains=%v", spec.EligibleGrants, spec.AllowedDomains)
	}
}

func TestEnsureLLMGrant_RespectsExistingGrant(t *testing.T) {
	existing := apiKeyGrant("api.anthropic.com", "anthropic-api-key")
	spec := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{existing}}
	ensureLLMGrant(&spec, "claude-code", secretsWith("anthropic-api-key"), false)
	if n := len(spec.EligibleGrants); n != 1 {
		t.Errorf("must not duplicate an existing api_key grant; grants=%d", n)
	}
}

func TestEnsureLLMGrant_CodexOpenAIConvention(t *testing.T) {
	spec := types.RunPolicySpec{}
	ensureLLMGrant(&spec, "codex-cli", secretsWith("openai-api-key"), false)
	g, ok := apiKeyGrantForHost(&spec, "api.openai.com")
	if !ok {
		t.Fatal("expected an api_key grant for api.openai.com")
	}
	header, format, secret := scopeFields(t, g)
	if header != "Authorization" || format != "Bearer %s" || secret != "openai-api-key" {
		t.Errorf("openai api_key scope wrong: header=%q format=%q secret=%q", header, format, secret)
	}
	if !domainAllowedExact(spec.AllowedDomains, "api.openai.com") {
		t.Error("api.openai.com must be added to AllowedDomains")
	}
}

// ── reconcileLLMAccess (post-clamp; truthful FINAL state, drops orphaned grants) ──

// A usable grant WITH its egress entry AND secret => an authoritative "provisioned"
// note (NOT a no-access warning), so the proposal states model access as ground
// truth even if the analyzer emitted a stale caution.
func TestReconcileLLMAccess_SatisfiedIsProvisionedNote(t *testing.T) {
	spec := types.RunPolicySpec{}
	ensureLLMGrant(&spec, "claude-code", secretsWith("anthropic-api-key"), false) // grant + domain
	w, _ := reconcileLLMAccess(&spec, "claude-code", secretsWith("anthropic-api-key"), false, false)
	if !strings.Contains(w, "model access provisioned") || !strings.Contains(w, "api.anthropic.com") {
		t.Errorf("expected an authoritative provisioned note; got %q", w)
	}
	if strings.Contains(strings.ToLower(w), "no model access") {
		t.Errorf("provisioned note must not read as a failure; got %q", w)
	}
}

// The must-fix regression: a grant that survived but whose exact-host egress entry
// did NOT must NOT report access OK — and the orphaned grant must be DROPPED so the
// run doesn't hard-fail at proxy startup.
func TestReconcileLLMAccess_GrantWithoutEgressIsDroppedAndWarned(t *testing.T) {
	// Grant present, but AllowedDomains does NOT contain api.anthropic.com.
	grant := apiKeyGrant("api.anthropic.com", "anthropic-api-key")
	spec := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{grant}, AllowedDomains: []string{"github.com"}}
	w, _ := reconcileLLMAccess(&spec, "claude-code", secretsWith("anthropic-api-key"), false, false)
	if w == "" {
		t.Fatal("must warn: a grant without its egress entry would fail the proxy at startup")
	}
	if _, ok := apiKeyGrantForHost(&spec, "api.anthropic.com"); ok {
		t.Error("the orphaned grant must be DROPPED to avoid a hard proxy-startup failure")
	}
}

// Secret absent => warning that names the secret AND the Claude subscription remedy.
func TestReconcileLLMAccess_NoSecret(t *testing.T) {
	spec := types.RunPolicySpec{}
	w, _ := reconcileLLMAccess(&spec, "claude-code", secretsWith(), false, false)
	if !strings.Contains(w, "no model access") || !strings.Contains(w, "anthropic-api-key") || !strings.Contains(w, "subscription") {
		t.Errorf("no-secret warning must name the secret and the subscription remedy; got %q", w)
	}
}

// Secret present but grant clamped away (ceiling forbids api_key) => policy-cause warning.
func TestReconcileLLMAccess_GrantClampedAway(t *testing.T) {
	spec := types.RunPolicySpec{}
	w, _ := reconcileLLMAccess(&spec, "claude-code", secretsWith("anthropic-api-key"), false, false)
	if !strings.Contains(w, "no model access") || !strings.Contains(w, "composer-dev.json") {
		t.Errorf("clamped-away warning must name the policy cause + remedy; got %q", w)
	}
}

// The structured provisioned flag (B3): true for a satisfied grant, false for every
// no-access case — the review gates Launch on this bool, never on the prose.
func TestReconcileLLMAccess_ProvisionedFlag(t *testing.T) {
	// Satisfied grant => provisioned true.
	ok := types.RunPolicySpec{}
	ensureLLMGrant(&ok, "claude-code", secretsWith("anthropic-api-key"), false)
	if _, provisioned := reconcileLLMAccess(&ok, "claude-code", secretsWith("anthropic-api-key"), false, false); !provisioned {
		t.Error("a satisfied grant must report provisioned=true")
	}
	// No secret => provisioned false.
	if _, provisioned := reconcileLLMAccess(&types.RunPolicySpec{}, "claude-code", secretsWith(), false, false); provisioned {
		t.Error("no-secret must report provisioned=false")
	}
	// Secret present but ceiling forbids api_key (grant clamped away) => provisioned false.
	if _, provisioned := reconcileLLMAccess(&types.RunPolicySpec{}, "claude-code", secretsWith("anthropic-api-key"), false, false); provisioned {
		t.Error("clamped-away grant must report provisioned=false")
	}
	// Non-LLM agent => (note "", provisioned true): nothing to verify, no row.
	if note, provisioned := reconcileLLMAccess(&types.RunPolicySpec{}, "some-other-agent", secretsWith(), false, false); note != "" || !provisioned {
		t.Errorf("non-LLM agent must be (note=\"\", provisioned=true); got (%q, %v)", note, provisioned)
	}
}

// Claude-only remedy: a codex-cli no-secret warning must NOT mention a subscription
// mount (there is no OpenAI/Codex subscription-mount path).
func TestReconcileLLMAccess_CodexHasNoSubscriptionHint(t *testing.T) {
	spec := types.RunPolicySpec{}
	w, _ := reconcileLLMAccess(&spec, "codex-cli", secretsWith(), false, false)
	if !strings.Contains(w, "openai-api-key") {
		t.Errorf("codex warning must name openai-api-key; got %q", w)
	}
	if strings.Contains(strings.ToLower(w), "subscription") {
		t.Errorf("codex warning must NOT claim a subscription-mount remedy (Claude-only); got %q", w)
	}
}

func TestReconcileLLMAccess_UnknownAgentSilent(t *testing.T) {
	spec := types.RunPolicySpec{}
	if w, _ := reconcileLLMAccess(&spec, "some-other-agent", secretsWith(), false, false); w != "" {
		t.Errorf("unknown agent must not warn; got %q", w)
	}
}

// ── subscription mode (per-run opt-in + ceiling-blessed cred mounts) ──
// (boolPtr comes from policy_test.go — same package.)

// A ceiling that blesses the Claude credential mounts (operator-staged copies)
// and lists the subscription egress verbatim, like the generated policy.
func subscriptionCeiling() types.RunPolicySpec {
	return types.RunPolicySpec{
		AllowedDomains: []string{"*.anthropic.com", "api.anthropic.com", "github.com"},
		WorkspaceMounts: []types.WorkspaceMount{
			{Source: "/home/op/.wardyn/claude-creds/.claude", Target: claudeCredTarget, ReadOnly: boolPtr(true)},
			{Source: "/home/op/.wardyn/claude-creds/.claude.json", Target: claudeCredJSONTarget, ReadOnly: boolPtr(true)},
		},
	}
}

// Opt-in + blessed ceiling + reachable egress => both cred mounts injected,
// copied VERBATIM from the ceiling (incl. read-only), no warning.
func TestApplyLLMCredMount_InjectsBlessedMountsOnOptIn(t *testing.T) {
	ceiling := subscriptionCeiling()
	spec := types.RunPolicySpec{AllowedDomains: []string{"*.anthropic.com"}}
	injected, warns := applyLLMCredMount(&spec, ceiling, "claude-code", true)
	if !injected {
		t.Fatalf("expected injection; warns=%v", warns)
	}
	if len(warns) != 0 {
		t.Errorf("clean injection must not warn; got %v", warns)
	}
	if !specHasMountTarget(&spec, claudeCredTarget) || !specHasMountTarget(&spec, claudeCredJSONTarget) {
		t.Fatalf("both cred mounts must be injected; got %+v", spec.WorkspaceMounts)
	}
	for _, wm := range spec.WorkspaceMounts {
		if !wm.ReadOnlyOrDefault() {
			t.Errorf("injected cred mount %s must keep the ceiling's read-only", wm.Target)
		}
	}
}

// No opt-in => NO injection even with a fully blessed ceiling: per-run consent
// is the whole point (ceiling blessing alone is control-plane-wide).
func TestApplyLLMCredMount_NoOptInNoInjection(t *testing.T) {
	spec := types.RunPolicySpec{AllowedDomains: []string{"*.anthropic.com"}}
	injected, warns := applyLLMCredMount(&spec, subscriptionCeiling(), "claude-code", false)
	if injected || len(spec.WorkspaceMounts) != 0 || len(warns) != 0 {
		t.Errorf("no opt-in must be a silent no-op; injected=%v mounts=%v warns=%v", injected, spec.WorkspaceMounts, warns)
	}
}

// Opt-in without a blessed ceiling => no injection + a warning naming the fix.
func TestApplyLLMCredMount_NotBlessedWarns(t *testing.T) {
	spec := types.RunPolicySpec{AllowedDomains: []string{"*.anthropic.com"}}
	injected, warns := applyLLMCredMount(&spec, types.RunPolicySpec{}, "claude-code", true)
	if injected || len(spec.WorkspaceMounts) != 0 {
		t.Fatal("must not inject when the ceiling blesses no cred mount")
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "stage-claude-creds.sh") {
		t.Errorf("expected the not-blessed warning naming the staging script; got %v", warns)
	}
}

// Opt-in but the clamped egress cannot reach api.anthropic.com => REFUSE to
// mount (a resident credential the agent cannot use is pure risk) + warn.
func TestApplyLLMCredMount_NoEgressNoInjection(t *testing.T) {
	spec := types.RunPolicySpec{AllowedDomains: []string{"github.com"}}
	injected, warns := applyLLMCredMount(&spec, subscriptionCeiling(), "claude-code", true)
	if injected || len(spec.WorkspaceMounts) != 0 {
		t.Fatal("must not inject cred mounts when egress cannot reach the provider")
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "api.anthropic.com") {
		t.Errorf("expected the no-egress warning; got %v", warns)
	}
}

// Subscription is Claude-only: a codex opt-in warns and injects nothing.
func TestApplyLLMCredMount_NonClaudeAgentWarns(t *testing.T) {
	spec := types.RunPolicySpec{AllowedDomains: []string{"*.anthropic.com"}}
	injected, warns := applyLLMCredMount(&spec, subscriptionCeiling(), "codex-cli", true)
	if injected || len(spec.WorkspaceMounts) != 0 {
		t.Fatal("must not inject cred mounts for a non-Claude agent")
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "Claude-only") {
		t.Errorf("expected the Claude-only warning; got %v", warns)
	}
}

// Subscribed pre-clamp path proposes the egress entries and NO grant (the
// explicit transport choice is respected, not doubled up with an api key).
func TestEnsureLLMGrant_SubscribedProposesEgressNotGrant(t *testing.T) {
	spec := types.RunPolicySpec{}
	ensureLLMGrant(&spec, "claude-code", secretsWith("anthropic-api-key"), true)
	if len(spec.EligibleGrants) != 0 {
		t.Errorf("subscribed path must not add an api_key grant; got %+v", spec.EligibleGrants)
	}
	if !domainAllowedExact(spec.AllowedDomains, "*.anthropic.com") || !domainAllowedExact(spec.AllowedDomains, "api.anthropic.com") {
		t.Errorf("subscribed path must propose both subscription egress entries; got %v", spec.AllowedDomains)
	}
}

// Final spec carrying the cred mount + reachable egress => the authoritative
// subscription "provisioned" note (incl. the honest resident-credential caveat).
func TestReconcileLLMAccess_SubscriptionProvisionedNote(t *testing.T) {
	spec := types.RunPolicySpec{AllowedDomains: []string{"*.anthropic.com"}}
	injected, _ := applyLLMCredMount(&spec, subscriptionCeiling(), "claude-code", true)
	if !injected {
		t.Fatal("setup: injection expected")
	}
	w, _ := reconcileLLMAccess(&spec, "claude-code", secretsWith(), false, false)
	if !strings.Contains(w, "model access provisioned") || !strings.Contains(w, "subscription") {
		t.Errorf("expected the subscription provisioned note; got %q", w)
	}
	if !strings.Contains(w, "resident") {
		t.Errorf("the note must carry the honest resident-credential caveat; got %q", w)
	}
	if strings.Contains(strings.ToLower(w), "no model access") {
		t.Errorf("provisioned note must not read as failure; got %q", w)
	}
}

// The DEFAULT (subscription-inject ON): the note states the credential is
// injected PROXY-SIDE and never goes stale — and with MITM auto-enabled, the
// require_inspectable_llm predicate that fail-closes the LEGACY path must NOT
// fire (same spec as the fail-close test, opposite outcome).
func TestReconcileLLMAccess_SubscriptionInjectDefaultNote(t *testing.T) {
	spec := types.RunPolicySpec{
		AllowedDomains: []string{"*.anthropic.com"},
		LLMInspection:  &types.LLMInspectionSpec{Mode: "alert", DetectSecrets: true, RequireInspectableLLM: true},
	}
	if injected, _ := applyLLMCredMount(&spec, subscriptionCeiling(), "claude-code", true); !injected {
		t.Fatal("setup: injection expected")
	}
	w, _ := reconcileLLMAccess(&spec, "claude-code", secretsWith(), true, false)
	if !strings.Contains(w, "PROXY-SIDE") || !strings.Contains(w, "never goes stale") {
		t.Errorf("default note must state proxy-side injection + no staleness; got %q", w)
	}
	if strings.Contains(w, "FAIL at launch") {
		t.Errorf("inject-on auto-enables MITM, so require_inspectable_llm must NOT fail-close; got %q", w)
	}
}

// Subscription is the chosen transport: a provider api_key grant that rode along
// (model-proposed, ceiling-blessed) is DROPPED — least privilege, and fail-safe:
// with its secret absent it would fail the proxy closed at startup and hard-kill
// the launch the provisioned note promises. (Observed live: Opus proposed an
// api_key grant on a subscription compose with no secret stored.)
func TestReconcileLLMAccess_SubscriptionDropsRideAlongAPIKeyGrant(t *testing.T) {
	grant := apiKeyGrant("api.anthropic.com", "anthropic-api-key")
	spec := types.RunPolicySpec{
		AllowedDomains: []string{"*.anthropic.com", "api.anthropic.com"},
		EligibleGrants: []types.GrantSpec{grant},
	}
	if injected, _ := applyLLMCredMount(&spec, subscriptionCeiling(), "claude-code", true); !injected {
		t.Fatal("setup: injection expected")
	}
	w, _ := reconcileLLMAccess(&spec, "claude-code", secretsWith(), true, false) // secret ABSENT; default proxy-side inject
	if !strings.Contains(w, "model access provisioned") {
		t.Fatalf("subscription must still be provisioned; got %q", w)
	}
	if _, ok := apiKeyGrantForHost(&spec, "api.anthropic.com"); ok {
		t.Error("the ride-along api_key grant must be dropped in subscription mode (missing secret would fail the proxy at startup)")
	}
}

// api-key mode, model-proposed grant, secret ABSENT => the grant is dropped
// (proxy would fail closed at startup) and the honest no-secret warning stands.
func TestReconcileLLMAccess_NoSecretDropsOrphanedGrant(t *testing.T) {
	grant := apiKeyGrant("api.anthropic.com", "anthropic-api-key")
	spec := types.RunPolicySpec{
		AllowedDomains: []string{"api.anthropic.com"},
		EligibleGrants: []types.GrantSpec{grant},
	}
	w, _ := reconcileLLMAccess(&spec, "claude-code", secretsWith(), false, false)
	if !strings.Contains(w, "no model access") || !strings.Contains(w, "anthropic-api-key") {
		t.Errorf("expected the no-secret warning; got %q", w)
	}
	if _, ok := apiKeyGrantForHost(&spec, "api.anthropic.com"); ok {
		t.Error("a grant with an absent secret must be dropped (it fails the proxy closed at startup)")
	}
}

// The EXACT dispatch fail-close predicate (require_inspectable_llm=true AND an
// active mode AND no intercept_tls AND subscription) => a launch-will-fail
// warning at review time...
func TestReconcileLLMAccess_SubscriptionInspectionFailCloseWarn(t *testing.T) {
	spec := types.RunPolicySpec{
		AllowedDomains: []string{"*.anthropic.com"},
		LLMInspection:  &types.LLMInspectionSpec{Mode: "alert", DetectSecrets: true, RequireInspectableLLM: true},
	}
	if injected, _ := applyLLMCredMount(&spec, subscriptionCeiling(), "claude-code", true); !injected {
		t.Fatal("setup: injection expected")
	}
	w, _ := reconcileLLMAccess(&spec, "claude-code", secretsWith(), false, false)
	if !strings.Contains(w, "FAIL at launch") || !strings.Contains(w, "intercept_tls") {
		t.Errorf("expected the fail-at-launch warning; got %q", w)
	}
}

// ...and ONLY that predicate: the default require_inspectable_llm=false merely
// degrades visibly at dispatch, so the provisioned note must NOT turn into a
// spurious failure warning (review catch: warning on mode alone over-fires).
func TestReconcileLLMAccess_SubscriptionInspectionDefaultNoFailWarn(t *testing.T) {
	spec := types.RunPolicySpec{
		AllowedDomains: []string{"*.anthropic.com"},
		LLMInspection:  &types.LLMInspectionSpec{Mode: "alert", DetectSecrets: true}, // RequireInspectableLLM=false
	}
	if injected, _ := applyLLMCredMount(&spec, subscriptionCeiling(), "claude-code", true); !injected {
		t.Fatal("setup: injection expected")
	}
	w, _ := reconcileLLMAccess(&spec, "claude-code", secretsWith(), false, false)
	if strings.Contains(w, "FAIL at launch") {
		t.Errorf("require_inspectable_llm=false must not produce the fail-at-launch warning; got %q", w)
	}
	if !strings.Contains(w, "model access provisioned") {
		t.Errorf("expected the provisioned note; got %q", w)
	}
}
