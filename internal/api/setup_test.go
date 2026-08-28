// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/subscription"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// decodeSetupSSO is decodeSetup's SSO-session twin, for the member-redaction
// tests (redaction is a role check — a bearer-token caller is always admin,
// so it needs a real SSO session to exercise the member branch at all).
func decodeSetupSSO(t *testing.T, srv *Server, cookie *http.Cookie) (int, SetupStatus) {
	t.Helper()
	w := doSSO(t, srv, http.MethodGet, "/api/v1/setup/status", cookie, "")
	var st SetupStatus
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
			t.Fatalf("decode setup status: %v; body=%s", err, w.Body.String())
		}
	}
	return w.Code, st
}

// TestSetupStatus_MemberRedactionPreservesLLMReady is the HIGH-4 review fix:
// a member's response drops checks/providers/secret-names/runner-detail (item
// 2's redaction) but LLMReady survives it — computed BEFORE redaction from
// the SAME signal llmProvenance already folds (here, a stored anthropic-api-key
// secret), matching exactly what an admin sees for the identical server state.
// Without this a member's console has no way to answer "is there any LLM
// access at all" once the detail that used to imply it is gone.
func TestSetupStatus_MemberRedactionPreservesLLMReady(t *testing.T) {
	srv := New(Config{
		Runner:     &fakeRunner{},
		Secrets:    &memSecrets{m: map[string][]byte{"anthropic-api-key": []byte("sk-ant-x")}},
		AdminToken: adminToken,
		OIDC:       &oidc.Authenticator{},
	})

	adminCode, adminSt := decodeSetupSSO(t, srv, ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin))
	if adminCode != http.StatusOK {
		t.Fatalf("admin: code = %d, want 200", adminCode)
	}
	if !adminSt.LLMReady {
		t.Fatalf("admin: llm_ready = false, want true (a stored anthropic-api-key secret is configured)")
	}
	if len(adminSt.Checks) == 0 {
		t.Fatalf("admin: checks unexpectedly empty — the fixture is not exercising the signal this test needs")
	}

	memberCode, memberSt := decodeSetupSSO(t, srv, ssoSession(t, "sub-member", "member@corp.example", oidc.RoleMember))
	if memberCode != http.StatusOK {
		t.Fatalf("member: code = %d, want 200 (redaction is never a 403/401)", memberCode)
	}
	// Redacted: item 2's drop list.
	if len(memberSt.Checks) != 0 {
		t.Errorf("member: checks = %v, want empty (redacted)", memberSt.Checks)
	}
	if len(memberSt.Providers) != 0 {
		t.Errorf("member: providers = %v, want empty (redacted)", memberSt.Providers)
	}
	if len(memberSt.Secrets.Present) != 0 {
		t.Errorf("member: secrets.present = %v, want empty (redacted)", memberSt.Secrets.Present)
	}
	// NOT redacted: ConfinementClasses feeds barrierReady (deriveReadiness),
	// which gates a member's own demo Start button — zeroing it disabled
	// demos for every member (W3-S1-2).
	if len(memberSt.Runner.ConfinementClasses) == 0 {
		t.Errorf("member: runner.confinement_classes = %v, want the real classes (drives demo barrierReady)", memberSt.Runner.ConfinementClasses)
	}
	if memberSt.Runner.Driver != "" {
		t.Errorf("member: runner.driver = %q, want empty (redacted diagnostic detail)", memberSt.Runner.Driver)
	}
	// NOT redacted: the answer a member's console needs to function.
	if !memberSt.LLMReady {
		t.Errorf("member: llm_ready = false, want true (must survive redaction, matching the admin's view)")
	}
	if !memberSt.Ready {
		t.Errorf("member: ready = false, want true (App.tsx's reachability gate — never redacted)")
	}
}

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

// The two additive SetupStatus fields (Integrations, Harnesses) are populated
// alongside the existing checks, never in place of them: Harnesses always
// echoes the static harness catalog, and Integrations reflects a legacy row
// derived from a plain stored secret exactly like GET /integrations would.
func TestSetupStatus_IntegrationsAndHarnessesAdditive(t *testing.T) {
	srv := New(Config{
		AdminToken: adminToken,
		Secrets:    &memSecrets{m: map[string][]byte{"anthropic-api-key": []byte("sk-ant-x")}},
	})
	code, st := decodeSetup(t, srv, adminToken)
	if code != http.StatusOK {
		t.Fatalf("code = %d, want 200", code)
	}
	if len(st.Harnesses) != len(harnessCatalog) {
		t.Errorf("Harnesses = %d entries, want %d (one per catalog row)", len(st.Harnesses), len(harnessCatalog))
	}
	found := false
	for _, in := range st.Integrations {
		if in.ID == "anthropic_api_key" {
			found = true
		}
	}
	if !found {
		t.Errorf("Integrations = %+v, want an anthropic_api_key entry derived from the stored secret", st.Integrations)
	}
}

// Full assembly: a fake Runner + in-memory Secrets + AgeKeyDurable are echoed
// correctly, and reserved secret names are excluded from secrets.present.
func TestSetupStatus_Assembly(t *testing.T) {
	sec := &memSecrets{m: map[string][]byte{
		"anthropic-api-key":  []byte("sk"),
		"wardyn-signing-key": []byte("reserved"), // must be excluded from present
	}}
	srv := New(Config{
		Runner:        &fakeRunner{},
		Secrets:       sec,
		AdminToken:    adminToken,
		AgeKeyDurable: true,
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

// k8sRunner is a minimal runner.Runner fake reporting a k8s-shaped
// Capabilities() (Driver "k8s", CC1 only, a settable NetworkPolicy verdict) —
// embeds a nil runner.Runner so only the two methods setupRunnerInfo actually
// calls (Name/Capabilities) need implementing, same seam
// setupCheckIdsStore uses for store.Store.
type k8sRunner struct {
	runner.Runner
	networkPolicy             bool
	networkPolicyAcknowledged bool
}

func (k8sRunner) Name() string { return "k8s" }
func (r k8sRunner) Capabilities(context.Context) (runner.Capabilities, error) {
	return runner.Capabilities{
		Driver:                    "k8s",
		ConfinementClasses:        []types.ConfinementClass{types.CC1},
		NetworkPolicy:             r.networkPolicy,
		NetworkPolicyAcknowledged: r.networkPolicyAcknowledged,
	}, nil
}

// TestSetupStatus_K8sEgressContainmentCheck: handleSetupStatus's checks list
// carries a k8s_egress_containment row matching the k8s substrate's
// aggregated Capabilities.NetworkPolicy verdict — proving setupRunnerInfo's
// local netpol computation (a local return value, not a wire field — see L2
// review: SetupRunner.NetworkPolicyProven was dropped as unconsumed; the
// graded check IS the wire surface) reaches handleSetupStatus correctly, not
// just at the pure-function level (see TestK8sEgressContainmentCheck in
// setup_checks_test.go for that).
func TestSetupStatus_K8sEgressContainmentCheck(t *testing.T) {
	cases := []struct {
		name       string
		netpol     bool
		wantStatus string
	}{
		{"canary proved enforced", true, "ok"},
		{"canary proved unenforced (opted out — the only live non-enforced verdict)", false, "fail"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := New(Config{AdminToken: adminToken, Runner: k8sRunner{networkPolicy: tc.netpol}})
			code, st := decodeSetup(t, srv, adminToken)
			if code != http.StatusOK {
				t.Fatalf("code = %d, want 200", code)
			}
			if st.Runner.Driver != "k8s" {
				t.Fatalf("runner.driver = %q, want k8s", st.Runner.Driver)
			}
			var found *SetupCheck
			for i := range st.Checks {
				if st.Checks[i].ID == "k8s_egress_containment" {
					found = &st.Checks[i]
				}
			}
			if found == nil {
				t.Fatalf("checks missing k8s_egress_containment; got %+v", st.Checks)
			}
			if found.Status != tc.wantStatus {
				t.Errorf("k8s_egress_containment status = %q, want %q", found.Status, tc.wantStatus)
			}
		})
	}
}

// TestSetupStatus_K8sEgressContainmentCheck_Acknowledged is B1's handler-
// level twin of TestK8sEgressContainmentCheck_Acknowledged
// (setup_checks_test.go): proves the "acknowledged" row actually reaches
// /setup/status wired through the real Capabilities() call, warn-graded and
// distinct from both the enforced (ok) and unenforced (fail) rows.
func TestSetupStatus_K8sEgressContainmentCheck_Acknowledged(t *testing.T) {
	srv := New(Config{AdminToken: adminToken, Runner: k8sRunner{networkPolicyAcknowledged: true}})
	code, st := decodeSetup(t, srv, adminToken)
	if code != http.StatusOK {
		t.Fatalf("code = %d, want 200", code)
	}
	var found *SetupCheck
	for i := range st.Checks {
		if st.Checks[i].ID == "k8s_egress_containment" {
			found = &st.Checks[i]
		}
	}
	if found == nil {
		t.Fatalf("checks missing k8s_egress_containment; got %+v", st.Checks)
	}
	if found.Status != "warn" {
		t.Errorf("k8s_egress_containment status = %q, want warn", found.Status)
	}
}

// A docker-shaped (non-k8s) runner must never carry the row — it is L0
// structural, not L1 packet-filter, and has nothing to prove here.
func TestSetupStatus_NonK8sRunnerOmitsEgressContainmentRow(t *testing.T) {
	srv := New(Config{AdminToken: adminToken, Runner: &fakeRunner{}})
	code, st := decodeSetup(t, srv, adminToken)
	if code != http.StatusOK {
		t.Fatalf("code = %d, want 200", code)
	}
	for _, c := range st.Checks {
		if c.ID == "k8s_egress_containment" {
			t.Errorf("non-k8s driver must not carry a k8s_egress_containment row: %+v", c)
		}
	}
}

// TestSetupStatus_ConfinementFloorRow is the handler-level twin of
// TestConfinementFloorCheck (setup_checks_test.go): proves the row actually
// reaches /setup/status wired to the real DefaultPolicy and runner
// Capabilities, not just the pure function in isolation.
func TestSetupStatus_ConfinementFloorRow(t *testing.T) {
	t.Run("floor unadvertised: warn row present", func(t *testing.T) {
		srv := New(Config{
			AdminToken:    adminToken,
			Runner:        k8sRunner{}, // advertises only CC1
			DefaultPolicy: types.RunPolicySpec{MinConfinementClass: types.CC2},
		})
		code, st := decodeSetup(t, srv, adminToken)
		if code != http.StatusOK {
			t.Fatalf("code = %d, want 200", code)
		}
		var found *SetupCheck
		for i := range st.Checks {
			if st.Checks[i].ID == "confinement_floor" {
				found = &st.Checks[i]
			}
		}
		if found == nil {
			t.Fatalf("checks missing confinement_floor; got %+v", st.Checks)
		}
		if found.Status != "warn" {
			t.Errorf("status = %q, want warn", found.Status)
		}
		if found.Fix == "" {
			t.Error("warn row must carry a Fix")
		}
	})

	t.Run("floor advertised: no row", func(t *testing.T) {
		srv := New(Config{
			AdminToken:    adminToken,
			Runner:        &fakeRunner{}, // advertises CC1, CC2, CC3
			DefaultPolicy: types.RunPolicySpec{MinConfinementClass: types.CC2},
		})
		code, st := decodeSetup(t, srv, adminToken)
		if code != http.StatusOK {
			t.Fatalf("code = %d, want 200", code)
		}
		for _, c := range st.Checks {
			if c.ID == "confinement_floor" {
				t.Errorf("floor IS advertised; must not carry a confinement_floor row: %+v", c)
			}
		}
	})

	t.Run("no floor configured: no row", func(t *testing.T) {
		srv := New(Config{AdminToken: adminToken, Runner: k8sRunner{}})
		code, st := decodeSetup(t, srv, adminToken)
		if code != http.StatusOK {
			t.Fatalf("code = %d, want 200", code)
		}
		for _, c := range st.Checks {
			if c.ID == "confinement_floor" {
				t.Errorf("no floor configured; must not carry a confinement_floor row: %+v", c)
			}
		}
	})
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

// llmProvenance must follow its priority (CLI login > api-key secret) and
// return the winning detail; a logged-in claude uses the precomputed
// subscription detail, everything else its own sentence. "" iff there is no
// signal (the lockstep that keeps readiness and the rendered detail from
// drifting).
func TestLLMProvenance_PriorityAndDetail(t *testing.T) {
	claudeLoggedIn := []SetupProvider{{Tool: "claude", Installed: true, LoggedIn: true}}

	// Logged-in claude wins and uses the injected subscription detail verbatim.
	if got := llmProvenance(claudeLoggedIn, []string{"anthropic-api-key"}, "SUB-DETAIL"); got != "SUB-DETAIL" {
		t.Errorf("claude winner detail = %q, want SUB-DETAIL (CLI login outranks secret)", got)
	}
	// Logged-in claude with no peeked detail falls back to a generic sentence.
	if got := llmProvenance(claudeLoggedIn, nil, ""); !strings.Contains(got, "claude CLI is logged in") {
		t.Errorf("generic claude detail = %q, want a 'logged in' sentence", got)
	}
	// codex login (non-claude) never consumes the claude detail.
	codex := []SetupProvider{{Tool: "codex", LoggedIn: true}}
	if got := llmProvenance(codex, nil, "SUB-DETAIL"); got == "SUB-DETAIL" || !strings.Contains(got, "codex") {
		t.Errorf("codex detail = %q, want a codex sentence, not the claude subscription detail", got)
	}
	// No CLI => a secret wins.
	if got := llmProvenance(nil, []string{"anthropic-api-key"}, ""); !strings.Contains(got, "anthropic-api-key") {
		t.Errorf("secret detail = %q, want it to name the secret", got)
	}
	// Nothing at all => "" (readiness false).
	if got := llmProvenance(nil, nil, ""); got != "" {
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

// claudeSubscriptionStagingCheck: fires only on a resident Claude login; a
// login whose DefaultPolicy ceiling doesn't bless the /home/agent/.claude mount
// is "detected but NOT staged" (the headless-`make setup` skip) => WARN naming
// `make stage-claude`; a blessed ceiling => ok; the macOS-Keychain login (which
// staging cannot read) gets the SSH-login remedy instead.
func TestClaudeSubscriptionStagingCheck(t *testing.T) {
	// No resident login => no row (llm_provider already covers "add one").
	if _, ok := claudeSubscriptionStagingCheck(false, false, ""); ok {
		t.Fatalf("no-login case should produce no claude_subscription_staging row")
	}

	// Logged in, ceiling does not bless the mount => WARN with the stage-claude fix.
	chk, ok := claudeSubscriptionStagingCheck(true, false, "~/.claude/.credentials.json")
	if !ok || chk.Status != "warn" || chk.ID != "claude_subscription_staging" {
		t.Fatalf("logged-in-not-staged: ok=%v status=%q id=%q, want warn row", ok, chk.Status, chk.ID)
	}
	if !strings.Contains(chk.Fix, "make stage-claude") {
		t.Errorf("warn Fix should name `make stage-claude`; got %q", chk.Fix)
	}

	// Keychain login: staging can't read it — the Fix must carry the SSH remedy.
	chk, ok = claudeSubscriptionStagingCheck(true, false, "macOS Keychain (Claude Code-credentials)")
	if !ok || chk.Status != "warn" || !strings.Contains(chk.Fix, "SSH") || !strings.Contains(chk.Fix, "make stage-claude") {
		t.Fatalf("keychain login: ok=%v status=%q fix=%q, want warn with SSH + stage-claude remedy", ok, chk.Status, chk.Fix)
	}

	// Blessed ceiling (staging ran, run-host.sh picked the subscription ceiling) => ok.
	if chk, ok := claudeSubscriptionStagingCheck(true, true, ""); !ok || chk.Status != "ok" {
		t.Fatalf("staged: ok=%v status=%q, want ok", ok, chk.Status)
	}
}

// TestClaudeSubscriptionStagingCheck_NoResidentClaudeHome is the B4 k8s
// verify-don't-implement item: claude_subscription_staging is gated on
// claudeLoginSignal, which is gated on setupProviders -> DetectCLIProviders
// reading $HOME/.claude/.credentials.json — a k8s pod's home directory is a
// fresh container filesystem with no such file (nothing resident survives a
// pod restart), so the row must never appear. End to end through
// handleSetupStatus (not just the pure claudeSubscriptionStagingCheck unit
// above), on a k8s-shaped Runner, so a future change to the detection wiring
// itself would be caught here too.
//
// Investigated edge case per the brief (host-mounted ~/.claude into a k8s
// pod): no Helm value or documented deployment pattern mounts a Claude
// credential into the wardynd pod (grepped deploy/helm — nothing). If an
// operator did so anyway, DetectCLIProviders would honestly detect it and
// this check WOULD fire — its underlying claim ("detected but not staged for
// the per-run mount") stays true regardless of platform; only its Fix
// string's host-oriented remedy (`make stage-claude`) would read oddly. That
// is a copy nit on a deliberately non-standard setup, not a false-positive
// worth suppression code for — so none was added.
func TestClaudeSubscriptionStagingCheck_NoResidentClaudeHome(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no .claude, no .codex — a fresh pod's $HOME
	srv := New(Config{AdminToken: adminToken, Runner: k8sRunner{networkPolicy: true}})
	code, st := decodeSetup(t, srv, adminToken)
	if code != http.StatusOK {
		t.Fatalf("code = %d, want 200", code)
	}
	for _, c := range st.Checks {
		if c.ID == "claude_subscription_staging" {
			t.Fatalf("claude_subscription_staging must never fire with no resident ~/.claude; got %+v", c)
		}
	}
}

// agentImageCheck: the shipped ghcr/compose-demo convention images are known
// Node-only by construction => warn naming WARDYN_AGENT_IMAGES; any operator
// override is assumed provisioned on purpose => info, not a red.
func TestAgentImageCheck(t *testing.T) {
	if chk := agentImageCheck(nil); chk.Status != "warn" || !strings.Contains(chk.Detail, "ghcr.io/cjohnstoniv/agent-claude-code:") {
		t.Errorf("nil images (ghcr fallback): status=%q detail=%q, want warn naming the ghcr ref", chk.Status, chk.Detail)
	}
	if chk := agentImageCheck(map[string]string{"claude-code": "wardyn/agent-claude-code:local"}); chk.Status != "warn" {
		t.Errorf("compose demo convention image: status=%q, want warn", chk.Status)
	}
	if chk := agentImageCheck(map[string]string{"claude-code": "wardyn/agent-full:local"}); chk.Status != "info" {
		t.Errorf("operator override image: status=%q, want info (not a red)", chk.Status)
	}
	for _, chk := range []SetupCheck{agentImageCheck(nil), agentImageCheck(map[string]string{"claude-code": "custom:tag"})} {
		if chk.ID != "agent_image" {
			t.Errorf("check id = %q, want agent_image", chk.ID)
		}
	}
}
