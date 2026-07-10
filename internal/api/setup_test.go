// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/subscription"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// decodeSetup runs GET /api/v1/setup/status and decodes the body on 200.
func decodeSetup(t *testing.T, srv *Server, bearer string) (int, SetupStatus) {
	t.Helper()
	w := do(t, srv, http.MethodGet, "/api/v1/setup/status", bearer, "")
	var st SetupStatus
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
			t.Fatalf("decode setup status: %v; body=%s", err, w.Body.String())
		}
	}
	return w.Code, st
}

// CRITICAL: /setup/status enumerates providers/keys/CLIs, so an anonymous
// non-local caller must be rejected (it lives under humanOrAdminAuth).
func TestSetupStatus_AnonymousNonLocal401(t *testing.T) {
	h := newHarness(t) // AdminToken set, not LocalMode
	if code, _ := decodeSetup(t, h.srv, ""); code != http.StatusUnauthorized {
		t.Fatalf("anonymous non-local: code = %d, want 401", code)
	}
}

// LocalMode bypasses auth; the handler must report auth.mode == "local".
func TestSetupStatus_LocalMode(t *testing.T) {
	srv := New(Config{LocalMode: true, LocalOperator: "local:test", LocalLoopback: true})
	code, st := decodeSetup(t, srv, "")
	if code != http.StatusOK {
		t.Fatalf("local mode: code = %d, want 200", code)
	}
	if st.Auth.Mode != "local" {
		t.Errorf("auth.mode = %q, want local", st.Auth.Mode)
	}
	if !st.Auth.LocalLoopback {
		t.Errorf("auth.local_loopback = false, want true (injected)")
	}
}

// Admin bearer authenticates and reports auth.mode == "token".
func TestSetupStatus_AdminToken(t *testing.T) {
	h := newHarness(t)
	code, st := decodeSetup(t, h.srv, adminToken)
	if code != http.StatusOK {
		t.Fatalf("admin token: code = %d, want 200", code)
	}
	if st.Auth.Mode != "token" {
		t.Errorf("auth.mode = %q, want token", st.Auth.Mode)
	}
}

// Full assembly: a fake Runner + fake Composer + in-memory Secrets + injected
// ComposerBackends snapshot + AgeKeyDurable are echoed correctly, and reserved
// secret names are excluded from secrets.present.
func TestSetupStatus_Assembly(t *testing.T) {
	reg, err := composer.NewRegistry("primary", []composer.RegistryEntry{
		{Info: composer.BackendInfo{Name: "primary", Provider: "anthropic", Model: "m"}, Composer: &composer.FakeComposer{}},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	sec := &memSecrets{m: map[string][]byte{
		"anthropic-api-key":  []byte("sk"),
		"wardyn-signing-key": []byte("reserved"), // must be excluded from present
	}}
	backends := []ComposerBackendReadiness{
		{Name: "primary", Provider: "anthropic", Model: "m", Wire: "anthropic", Enabled: true, NeedsKey: true, KeySecret: "anthropic-api-key", KeyResolved: true},
		{Name: "off", Provider: "openai", Model: "g", Wire: "openai", Enabled: false, NeedsKey: true, KeySecret: "openai-key", KeyResolved: false},
	}
	srv := New(Config{
		Runner:           &fakeRunner{},
		Composer:         reg,
		Secrets:          sec,
		AdminToken:       adminToken,
		AgeKeyDurable:    true,
		ComposerBackends: backends,
	})

	code, st := decodeSetup(t, srv, adminToken)
	if code != http.StatusOK {
		t.Fatalf("assembly: code = %d, want 200", code)
	}

	// Runner classes echoed from the fake's Capabilities (CC1,CC2,CC3).
	if len(st.Runner.ConfinementClasses) != 3 {
		t.Errorf("runner.confinement_classes = %v, want 3 entries", st.Runner.ConfinementClasses)
	}
	if st.Runner.Driver != "fake" {
		t.Errorf("runner.driver = %q, want fake", st.Runner.Driver)
	}

	// Composer reflects the injected snapshot verbatim.
	if !st.Composer.Enabled || st.Composer.Default != "primary" {
		t.Errorf("composer enabled/default = %v/%q, want true/primary", st.Composer.Enabled, st.Composer.Default)
	}
	if len(st.Composer.Backends) != 2 {
		t.Fatalf("composer.backends = %d, want 2", len(st.Composer.Backends))
	}
	byName := map[string]ComposerBackendReadiness{}
	for _, b := range st.Composer.Backends {
		byName[b.Name] = b
	}
	if b := byName["off"]; b.Enabled {
		t.Errorf("backend off should be disabled: %+v", b)
	}
	if b := byName["off"]; !b.NeedsKey || b.KeyResolved {
		t.Errorf("backend off should be needs-key + unresolved: %+v", b)
	}
	if b := byName["primary"]; !b.NeedsKey || !b.KeyResolved || b.KeySecret != "anthropic-api-key" {
		t.Errorf("backend primary readiness wrong: %+v", b)
	}

	// Reserved secret names are excluded; the user secret is present.
	for _, n := range st.Secrets.Present {
		if n == "wardyn-signing-key" {
			t.Errorf("secrets.present must exclude reserved name %q", n)
		}
	}
	foundUser := false
	for _, n := range st.Secrets.Present {
		if n == "anthropic-api-key" {
			foundUser = true
		}
	}
	if !foundUser {
		t.Errorf("secrets.present missing user secret; got %v", st.Secrets.Present)
	}

	if !st.AgeKey.Durable {
		t.Errorf("age_key.durable = false, want true (injected)")
	}
	// A live runner class => ready true.
	if !st.Ready {
		t.Errorf("ready = false, want true with a live runner")
	}
}

// restart_required drift: an ENABLED boot-unresolved needs-key backend whose key
// secret now appears in the live set => true (add-a-key-restart-to-apply). Absent
// => false. A DISABLED backend NEVER sets it (a restart re-runs BuildRegistry,
// which skips disabled backends, so "restart to apply" would be a lie).
func TestSetupStatus_RestartRequiredDrift(t *testing.T) {
	enabled := []ComposerBackendReadiness{
		{Name: "be", Wire: "openai", Enabled: true, NeedsKey: true, KeySecret: "openai-key", KeyResolved: false},
	}

	// Enabled + key now present in the live secret set => restart_required true.
	present := &memSecrets{m: map[string][]byte{"openai-key": []byte("sk")}}
	srv := New(Config{Runner: &fakeRunner{}, Secrets: present, AdminToken: adminToken, ComposerBackends: enabled})
	code, st := decodeSetup(t, srv, adminToken)
	if code != http.StatusOK {
		t.Fatalf("drift-present: code = %d", code)
	}
	if !st.RestartRequired || st.RestartReason == "" {
		t.Errorf("expected restart_required with a reason; got %v %q", st.RestartRequired, st.RestartReason)
	}

	// Key still absent => no restart drift.
	absent := &memSecrets{m: map[string][]byte{}}
	srv2 := New(Config{Runner: &fakeRunner{}, Secrets: absent, AdminToken: adminToken, ComposerBackends: enabled})
	if _, st2 := decodeSetup(t, srv2, adminToken); st2.RestartRequired {
		t.Errorf("restart_required = true with the key absent, want false")
	}

	// DISABLED backend with the key present => still false (restart wouldn't apply
	// it): the honesty guard. This is the case the review flagged.
	disabled := []ComposerBackendReadiness{
		{Name: "off", Wire: "openai", Enabled: false, NeedsKey: true, KeySecret: "openai-key", KeyResolved: false},
	}
	srv3 := New(Config{Runner: &fakeRunner{}, Secrets: present, AdminToken: adminToken, ComposerBackends: disabled})
	if _, st3 := decodeSetup(t, srv3, adminToken); st3.RestartRequired {
		t.Errorf("restart_required = true for a DISABLED backend, want false (restart wouldn't apply the key)")
	}
}

// ready must be false (wizard opens) when the runner is nil.
func TestSetupStatus_ReadyFalseWhenRunnerNil(t *testing.T) {
	srv := New(Config{AdminToken: adminToken}) // Runner nil
	code, st := decodeSetup(t, srv, adminToken)
	if code != http.StatusOK {
		t.Fatalf("nil runner: code = %d, want 200", code)
	}
	if st.Ready {
		t.Errorf("ready = true with nil runner, want false")
	}
	if st.Runner.Driver != "none" {
		t.Errorf("runner.driver = %q, want none", st.Runner.Driver)
	}
}

// llmProvenance must NOT count a `fake` (deterministic stub) composer backend
// as real LLM access — otherwise the first-run page shows "LLM access ✓" for the
// default `make setup` config (a single fake backend), which is an overclaim: the
// stub calls no model. Real backends, logged-in CLIs, and api-key secrets DO count.
func TestLLMProvenance_FakeBackendIsNotAccess(t *testing.T) {
	llmAccessAvailable := func(p []SetupProvider, b []ComposerBackendReadiness, s []string) bool {
		return llmProvenance(p, b, s, "") != ""
	}
	fakeOnly := []ComposerBackendReadiness{{Name: "dev", Wire: "fake", Enabled: true, KeyResolved: true}}
	if llmAccessAvailable(nil, fakeOnly, nil) {
		t.Error("fake-only backend counted as LLM access; want false (the deterministic stub calls no model)")
	}
	real := []ComposerBackendReadiness{{Name: "primary", Wire: "anthropic", Enabled: true, KeyResolved: true}}
	if !llmAccessAvailable(nil, real, nil) {
		t.Error("real resolved backend not counted as LLM access; want true")
	}
	if !llmAccessAvailable([]SetupProvider{{Tool: "claude", Installed: true, LoggedIn: true}}, fakeOnly, nil) {
		t.Error("logged-in CLI not counted as LLM access even with a fake backend; want true")
	}
	if !llmAccessAvailable(nil, fakeOnly, []string{"anthropic-api-key"}) {
		t.Error("anthropic-api-key secret not counted as LLM access; want true")
	}
	// A fake backend that is disabled/unresolved is obviously not access either.
	if llmAccessAvailable(nil, []ComposerBackendReadiness{{Name: "dev", Wire: "fake", Enabled: false, KeyResolved: true}}, nil) {
		t.Error("disabled fake backend counted as access; want false")
	}
}

// deploymentHostLike is true only for a claude provider that is BOTH installed
// and logged in (host mode); anything less (not the claude tool, only one of
// the two, or no providers at all) is false (compose/blind).
func TestDeploymentHostLike(t *testing.T) {
	if !deploymentHostLike([]SetupProvider{{Tool: "claude", Installed: true, LoggedIn: true}}) {
		t.Error("installed+logged-in claude: got false, want true")
	}
	if deploymentHostLike([]SetupProvider{{Tool: "claude", Installed: true, LoggedIn: false}}) {
		t.Error("installed but not logged in: got true, want false")
	}
	if deploymentHostLike([]SetupProvider{{Tool: "claude", Installed: false, LoggedIn: true}}) {
		t.Error("logged in but not installed: got true, want false")
	}
	if deploymentHostLike([]SetupProvider{{Tool: "codex", Installed: true, LoggedIn: true}}) {
		t.Error("codex, not claude: got true, want false")
	}
	if deploymentHostLike(nil) {
		t.Error("no providers: got true, want false")
	}
}

// llmProvenance must follow llmAccessAvailable's priority (CLI login > real backend
// > api-key secret) and return the winning detail; a logged-in claude uses the
// precomputed subscription detail, everything else its own sentence. "" iff there
// is no signal (the lockstep that keeps readiness and the rendered detail from
// drifting — see llmAccessAvailable).
func TestLLMProvenance_PriorityAndDetail(t *testing.T) {
	claudeLoggedIn := []SetupProvider{{Tool: "claude", Installed: true, LoggedIn: true}}
	realBackend := []ComposerBackendReadiness{{Name: "primary", Provider: "anthropic", Wire: "anthropic", Enabled: true, KeyResolved: true}}

	// Logged-in claude wins and uses the injected subscription detail verbatim.
	if got := llmProvenance(claudeLoggedIn, realBackend, []string{"anthropic-api-key"}, "SUB-DETAIL"); got != "SUB-DETAIL" {
		t.Errorf("claude winner detail = %q, want SUB-DETAIL (CLI login outranks backend/secret)", got)
	}
	// Logged-in claude with no peeked detail falls back to a generic sentence.
	if got := llmProvenance(claudeLoggedIn, nil, nil, ""); !strings.Contains(got, "claude CLI is logged in") {
		t.Errorf("generic claude detail = %q, want a 'logged in' sentence", got)
	}
	// codex login (non-claude) never consumes the claude detail.
	codex := []SetupProvider{{Tool: "codex", LoggedIn: true}}
	if got := llmProvenance(codex, nil, nil, "SUB-DETAIL"); got == "SUB-DETAIL" || !strings.Contains(got, "codex") {
		t.Errorf("codex detail = %q, want a codex sentence, not the claude subscription detail", got)
	}
	// No CLI => a real backend wins; fake is skipped.
	if got := llmProvenance(nil, realBackend, nil, ""); !strings.Contains(got, "primary") {
		t.Errorf("backend detail = %q, want it to name the backend", got)
	}
	fakeOnly := []ComposerBackendReadiness{{Name: "dev", Wire: "fake", Enabled: true, KeyResolved: true}}
	if got := llmProvenance(nil, fakeOnly, []string{"anthropic-api-key"}, ""); !strings.Contains(got, "anthropic-api-key") {
		t.Errorf("secret detail = %q, want it to name the secret (fake backend skipped)", got)
	}
	// Nothing at all => "" (readiness false).
	if got := llmProvenance(nil, fakeOnly, nil, ""); got != "" {
		t.Errorf("no-signal detail = %q, want empty", got)
	}
}

// subscriptionLLMDetail: fresh vs expired vs no-token, inject on/off, and the
// off-PATH fallback. Wording is asserted by stable substrings (not verbatim) so
// copy tweaks don't brittle the test, but the honesty-load-bearing tokens
// (EXPIRED, injection posture, off-PATH) are pinned.
func TestSubscriptionLLMDetail(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	fresh := subscription.Token{Value: "t", ExpiresAt: now.Add(time.Hour)}
	expired := subscription.Token{Value: "t", ExpiresAt: now.Add(-time.Hour)}

	// Fresh + inject ON + on PATH.
	d := subscriptionLLMDetail(fresh, nil, true, "/home/u/.claude", "/usr/bin/claude", now)
	if !strings.Contains(d, "Claude subscription") || strings.Contains(d, "EXPIRED") ||
		!strings.Contains(d, "inject a fresh host token") || strings.Contains(d, "not on PATH") {
		t.Errorf("fresh/inject-on/on-path detail wrong: %q", d)
	}

	// Expired + inject OFF.
	d = subscriptionLLMDetail(expired, nil, false, "/home/u/.claude", "/usr/bin/claude", now)
	if !strings.Contains(d, "EXPIRED") || !strings.Contains(d, "injection is off") {
		t.Errorf("expired/inject-off detail wrong: %q", d)
	}

	// Logged in but the CLI is OFF PATH (binPath == "").
	d = subscriptionLLMDetail(fresh, nil, true, "/home/u/.claude", "", now)
	if !strings.Contains(d, "not on PATH") {
		t.Errorf("off-PATH detail missing the caveat: %q", d)
	}

	// No readable subscription token (peek error): CLI login still noted, but no
	// subscription claim; the login path is surfaced.
	d = subscriptionLLMDetail(subscription.Token{}, errors.New("no creds"), true, "/home/u/.claude", "/usr/bin/claude", now)
	if strings.Contains(d, "subscription token valid") || !strings.Contains(d, "no readable Claude subscription token") ||
		!strings.Contains(d, "/home/u/.claude") {
		t.Errorf("peek-fail detail wrong: %q", d)
	}
	// A present provider but empty token value is treated the same as a peek error.
	d = subscriptionLLMDetail(subscription.Token{Value: ""}, nil, true, "", "/usr/bin/claude", now)
	if !strings.Contains(d, "no readable Claude subscription token") {
		t.Errorf("empty-token detail wrong: %q", d)
	}
}

// composerCeilingCheck / llmCeilingAdmits: the ceiling-aware "will a composed run
// actually reach the model" row. It mirrors clampGrants (kind-keyed), ensureLLMGrant
// (exact-host egress), and applyLLMCredMount (subscription mount), so it MUST agree
// with the real clamp: a stored key under a github-token-only ceiling (demo.json) is
// a WARN; the same key under composer-dev.json is OK; and no credential => no row.
func TestComposerCeilingCheck(t *testing.T) {
	// demo.json-shaped ceiling: github_token only, no anthropic egress, no api_key.
	demo := types.RunPolicySpec{
		AllowedDomains: []string{"github.com", "*.githubusercontent.com"},
		EligibleGrants: []types.GrantSpec{{Kind: types.GrantGitHubToken}},
	}
	// composer-dev.json-shaped ceiling: auto-mint api_key + anthropic/openai egress.
	apiKeyScope, _ := json.Marshal(map[string]string{"host": "api.anthropic.com", "header": "x-api-key"})
	composerDev := types.RunPolicySpec{
		AllowedDomains: []string{"api.anthropic.com", "api.openai.com", "github.com"},
		EligibleGrants: []types.GrantSpec{
			{Kind: types.GrantAPIKey, Scope: apiKeyScope, RequiresApproval: false},
			{Kind: types.GrantGitHubToken},
		},
	}

	// No credential at all => no row (llm_provider already covers "add one").
	if _, ok := composerCeilingCheck(demo, false, false, false); ok {
		t.Fatalf("no-credential case should produce no composer_llm_ceiling row")
	}

	// Anthropic key present but demo ceiling brokers no api_key grant => WARN.
	chk, ok := composerCeilingCheck(demo, true, false, false)
	if !ok || chk.Status != "warn" || !strings.Contains(chk.Detail, "Anthropic") {
		t.Fatalf("anthropic key under demo ceiling: ok=%v status=%q detail=%q, want warn naming Anthropic", ok, chk.Status, chk.Detail)
	}
	if !strings.Contains(chk.Fix, "composer-dev.json") {
		t.Errorf("warn Fix should name composer-dev.json; got %q", chk.Fix)
	}

	// Same key under a composer-capable ceiling => OK.
	if chk, ok := composerCeilingCheck(composerDev, true, false, false); !ok || chk.Status != "ok" {
		t.Fatalf("anthropic key under composer-dev ceiling: ok=%v status=%q, want ok", ok, chk.Status)
	}

	// An api_key grant that REQUIRES APPROVAL does not auto-inject => WARN (mirrors
	// clampGrants force-tightening approval + reconcileLLMAccess's approval branch).
	approvalScope, _ := json.Marshal(map[string]string{"host": "api.anthropic.com"})
	approvalCeiling := types.RunPolicySpec{
		AllowedDomains: []string{"api.anthropic.com"},
		EligibleGrants: []types.GrantSpec{{Kind: types.GrantAPIKey, Scope: approvalScope, RequiresApproval: true}},
	}
	if chk, ok := composerCeilingCheck(approvalCeiling, true, false, false); !ok || chk.Status != "warn" {
		t.Fatalf("approval-required api_key grant: ok=%v status=%q, want warn", ok, chk.Status)
	}

	// OpenAI key resolves against the same kind-keyed clamp: OK under composer-dev.
	if chk, ok := composerCeilingCheck(composerDev, false, true, false); !ok || chk.Status != "ok" {
		t.Fatalf("openai key under composer-dev ceiling: ok=%v status=%q, want ok", ok, chk.Status)
	}

	// Subscription path: demo ceiling doesn't bless /home/agent/.claude => WARN; a
	// ceiling that blesses the mount + allows anthropic egress => OK.
	if chk, ok := composerCeilingCheck(demo, false, false, true); !ok || chk.Status != "warn" {
		t.Fatalf("claude subscription under demo ceiling: ok=%v status=%q, want warn", ok, chk.Status)
	}
	subCeiling := types.RunPolicySpec{
		AllowedDomains:  []string{"api.anthropic.com"},
		WorkspaceMounts: []types.WorkspaceMount{{Target: claudeCredTarget}},
	}
	if chk, ok := composerCeilingCheck(subCeiling, false, false, true); !ok || chk.Status != "ok" {
		t.Fatalf("claude subscription under blessed ceiling: ok=%v status=%q, want ok", ok, chk.Status)
	}
}

// agentImageCheck: the shipped ghcr/compose-demo convention images are known
// Node-only by construction => warn naming WARDYN_AGENT_IMAGES; any operator
// override is assumed provisioned on purpose => info, not a red.
func TestAgentImageCheck(t *testing.T) {
	if chk := agentImageCheck(nil); chk.Status != "warn" || !strings.Contains(chk.Detail, "ghcr.io/cjohnstoniv/agent-claude-code:latest") {
		t.Errorf("nil images (ghcr fallback): status=%q detail=%q, want warn naming the ghcr ref", chk.Status, chk.Detail)
	}
	if chk := agentImageCheck(map[string]string{"claude-code": "wardyn/agent-claude-code:demo"}); chk.Status != "warn" {
		t.Errorf("compose demo convention image: status=%q, want warn", chk.Status)
	}
	if chk := agentImageCheck(map[string]string{"claude-code": "wardyn/agent-campaign:demo"}); chk.Status != "info" {
		t.Errorf("operator override image: status=%q, want info (not a red)", chk.Status)
	}
	for _, chk := range []SetupCheck{agentImageCheck(nil), agentImageCheck(map[string]string{"claude-code": "custom:tag"})} {
		if chk.ID != "agent_image" {
			t.Errorf("check id = %q, want agent_image", chk.ID)
		}
	}
}
