// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/setup"
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

// TestSetupStatus_MemberRedactionPreservesLLMReady: a member's response drops
// checks/providers/secret-names/runner-detail (item 2's redaction) but
// LLMReady survives it — computed BEFORE redaction (here, from an enabled model
// provider serving an agent), matching exactly what an admin sees for the
// identical server state. Without this a member's console has no way to answer
// "is there any LLM access at all" once the redacted detail is gone.
func TestSetupStatus_MemberRedactionPreservesLLMReady(t *testing.T) {
	h := newHarness(t)
	st := &integStore{govEscapeStore: newGovEscapeStore(&capStore{}), site: types.SiteConfig{
		ModelProviders: &types.ModelProviders{Providers: []types.ModelProvider{{
			ID: "corp-key", UID: "u-corp-key", Kind: types.ModelProviderAnthropicAPIKey,
			Harnesses: []types.ProviderHarness{{Harness: "claude-code"}},
		}}},
	}}
	cfg := baseTestConfig(h, st)
	cfg.Runner = &fakeRunner{}
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	cfg.OIDC = &oidc.Authenticator{}
	srv := New(cfg)

	adminCode, adminSt := decodeSetupSSO(t, srv, ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin))
	if adminCode != http.StatusOK {
		t.Fatalf("admin: code = %d, want 200", adminCode)
	}
	if !adminSt.LLMReady {
		t.Fatalf("admin: llm_ready = false, want true (an enabled model provider serves claude-code)")
	}
	if len(adminSt.Checks) == 0 {
		t.Fatalf("admin: checks unexpectedly empty — the fixture is not exercising the signal this test needs")
	}

	memberCode, memberSt := decodeSetupSSO(t, srv, ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser))
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
	// demos for every member.
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

// TestRedactSetupStatusForUser_KeepsDemoSecretPresence is #850 item 1: a
// member's redacted secrets.present kept EVERY name at [], so
// setup/steps.ts's walkableDemos/stepOrder never offered a needsSecret demo
// even once an admin stored its exact seed secret — the demo's Start button
// stayed closed forever, for every member, regardless of the real state.
// demoSecretPresence narrows to the console demo catalog's own public secret
// names (safe: they carry no more information than the shipped client
// bundle already does) while every other name stays dropped, unchanged.
func TestRedactSetupStatusForUser_KeepsDemoSecretPresence(t *testing.T) {
	full := SetupStatus{
		Secrets: SetupSecrets{Present: []string{"wardyn-demo-key", "wardyn-demo-pat", "anthropic-api-key", "corp-proxy-url"}},
	}
	got := redactSetupStatusForUser(full)
	want := []string{"wardyn-demo-key", "wardyn-demo-pat"}
	if !slices.Equal(got.Secrets.Present, want) {
		t.Errorf("secrets.present = %v, want %v (demo seed secrets kept, every real secret name still dropped)", got.Secrets.Present, want)
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
// derived from plain stored secrets exactly like GET /integrations would.
func TestSetupStatus_IntegrationsAndHarnessesAdditive(t *testing.T) {
	srv := New(Config{
		AdminToken: adminToken,
		Secrets:    &memSecrets{m: map[string][]byte{secretGitHubAppID: []byte("123"), secretGitHubAppKey: []byte("key")}},
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
		if in.ID == "github_app" {
			found = true
		}
	}
	if !found {
		t.Errorf("Integrations = %+v, want a github_app entry derived from the stored secrets", st.Integrations)
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
		BaseCtx:       testBaseCtx(t),
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
	capsErr                   error // set to prove a failed Capabilities() call never reports a verdict
}

func (k8sRunner) Name() string { return "k8s" }
func (r k8sRunner) Capabilities(context.Context) (runner.Capabilities, error) {
	if r.capsErr != nil {
		return runner.Capabilities{}, r.capsErr
	}
	return runner.Capabilities{
		Driver:                    "k8s",
		ConfinementClasses:        []types.ConfinementClass{types.CC1},
		NetworkPolicy:             r.networkPolicy,
		NetworkPolicyAcknowledged: r.networkPolicyAcknowledged,
	}, nil
}

// TestK8sNetpolVerdict is the pure-function table test for k8sNetpolVerdict
// (extracted from setupRunnerInfo's former inline switch, now shared with
// handleHealthz's "network_policy" field): every live k8s Capabilities shape
// grades to its verdict, and a non-k8s driver always grades "" regardless of
// what its Capabilities happen to carry — the caller's own driver name gates
// this function, not anything inside caps.
func TestK8sNetpolVerdict(t *testing.T) {
	cases := []struct {
		name   string
		driver string
		caps   runner.Capabilities
		want   string
	}{
		{"k8s enforced", "k8s", runner.Capabilities{NetworkPolicy: true}, "enforced"},
		{"k8s unenforced", "k8s", runner.Capabilities{}, "unenforced"},
		{"k8s acknowledged", "k8s", runner.Capabilities{NetworkPolicyAcknowledged: true}, "acknowledged"},
		{"acknowledged wins over a stale NetworkPolicy=true", "k8s",
			runner.Capabilities{NetworkPolicy: true, NetworkPolicyAcknowledged: true}, "acknowledged"},
		{"docker driver: empty regardless of caps", "docker", runner.Capabilities{NetworkPolicy: true}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := k8sNetpolVerdict(tc.driver, tc.caps); got != tc.want {
				t.Errorf("k8sNetpolVerdict(%q, %+v) = %q, want %q", tc.driver, tc.caps, got, tc.want)
			}
		})
	}
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
			srv := New(Config{BaseCtx: testBaseCtx(t), AdminToken: adminToken, Runner: k8sRunner{networkPolicy: tc.netpol}})
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
	srv := New(Config{BaseCtx: testBaseCtx(t), AdminToken: adminToken, Runner: k8sRunner{networkPolicyAcknowledged: true}})
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
	srv := New(Config{BaseCtx: testBaseCtx(t), AdminToken: adminToken, Runner: &fakeRunner{}})
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
			BaseCtx:       testBaseCtx(t),
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
			BaseCtx:       testBaseCtx(t),
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
		srv := New(Config{BaseCtx: testBaseCtx(t), AdminToken: adminToken, Runner: k8sRunner{}})
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

// agentImageCheck: NEVER a warn. Both arms are info, and the row's whole job is
// carrying the RIGHT MESSAGE — the convention arm names the toolchain limit and
// how to lift it; the override arm names the harness and probe images.
//
// Both arms are info because the convention image is the SHIPPED DEFAULT: true
// of every stock install, documented rather than misconfigured, and clearable
// only by wiring an image a JS/Python operator never needs. Grading it warn
// made the first-run gate (which redirects on warn) hold every stock install in
// the funnel with no in-product way out. So severity cannot distinguish the two
// arms any more — the assertions below are on the detail text, which is what an
// operator actually reads.
func TestAgentImageCheck(t *testing.T) {
	// The ghcr fallback for claude-code resolves to agent-BASE since 0.7 (the
	// catalog row's ImageKey was re-pointed because agent-claude-code is not
	// published). agent-base carries node/npm/python3 but no Go, Java or Rust,
	// so the row must still SAY so — and must name the image the operator will
	// actually run, not the one that no longer resolves.
	chk := agentImageCheck(nil)
	if chk.Status != "info" || !strings.Contains(chk.Detail, "ghcr.io/cjohnstoniv/agent-base:") ||
		!strings.Contains(chk.Detail, "limited toolchain") {
		t.Errorf("nil images (ghcr fallback): status=%q detail=%q, want info naming the ghcr agent-base ref and its toolchain limit", chk.Status, chk.Detail)
	}
	// An info row that dropped its Fix would leave a real limitation with no
	// remedy — the reason this stayed a row at all rather than being deleted.
	if !strings.Contains(chk.Fix, "WARDYN_AGENT_IMAGES") {
		t.Errorf("convention image: Fix=%q, want it to name WARDYN_AGENT_IMAGES", chk.Fix)
	}
	// ...and the locally-built vendor images are still the limited ones they always were.
	for _, ref := range []string{"wardyn/agent-base:local", "wardyn/agent-claude-code:local"} {
		chk := agentImageCheck(map[string]string{"claude-code": ref})
		if chk.Status != "info" || !strings.Contains(chk.Detail, "limited toolchain") {
			t.Errorf("%s: status=%q detail=%q, want info naming the toolchain limit", ref, chk.Status, chk.Detail)
		}
	}
	if chk := agentImageCheck(map[string]string{"claude-code": "wardyn/agent-full:local"}); chk.Status != "info" {
		t.Errorf("operator override image: status=%q, want info (not a red)", chk.Status)
	}
	// The info row names BOTH the claude-code harness image and the distinct
	// `base` image the setup connectivity probe actually dispatches — naming
	// only "Configured claude-code agent image" would leave an operator
	// reading this row no way to know the probe uses a different image than
	// the one named here.
	if chk := agentImageCheck(map[string]string{"claude-code": "wardyn/agent-full:local"}); !strings.Contains(chk.Detail, "harness image") ||
		!strings.Contains(chk.Detail, "connectivity probe runs the `base` image") {
		t.Errorf("info detail = %q, want it to name the claude-code harness image AND the distinct base image the probe runs", chk.Detail)
	}
	for _, chk := range []SetupCheck{agentImageCheck(nil), agentImageCheck(map[string]string{"claude-code": "custom:tag"})} {
		if chk.ID != "agent_image" {
			t.Errorf("check id = %q, want agent_image", chk.ID)
		}
	}
}

// The redaction dropped the deployer's checklist (checks/providers/secret
// names/runner detail) but left three fields that describe the OPERATOR'S HOST
// rather than anything a member can act on:
//
//   - SCM: which git credentials sit on the wardynd host's disk — a gh CLI
//     session, ~/.git-credentials, ~/.netrc, and whether credential.helper is
//     the plaintext-ish "store"/"cache". That is a target list.
//   - HostProxy: the corporate proxy topology, host:port included, plus a
//     has_credentials flag saying the operator's proxy creds are on that box.
//
// Harness is reduced rather than dropped: deriveIntegrations (ui) reads
// provider/captured/expired to answer "is there a model path", which a member's
// own readiness needs; capture time, source run id, aging and renewability are
// operator credential-lifecycle detail with no member route to act on.
func TestRedactSetupStatusForMember_DropsHostCredentialPosture(t *testing.T) {
	full := SetupStatus{
		Ready: true, HasRuns: true, LLMReady: true, OnboardingComplete: true,
		Checks:    []SetupCheck{{ID: "runner", Status: "ok"}},
		Providers: []SetupProvider{{Tool: "claude", Installed: true}},
		Secrets:   SetupSecrets{Present: []string{"npm-token"}},
		Runner: SetupRunner{
			Driver: "docker", ConfinementClasses: []string{"CC2"},
			EphemeralDiskEnforcement: types.StorageEnforcementFilesystem,
		},
		SCM: setup.SCMPosture{
			GhCLI: true, CredentialHelper: "store", GitCredentialsFile: true, Netrc: true,
		},
		HostProxy: setup.HostProxyDetection{
			HTTPSProxy:     &setup.HostProxySetting{Value: "http://proxy.corp.example:3128", HasCredentials: true},
			HasCredentials: true,
		},
		Integrations: []SetupIntegration{{}},
	}
	got := redactSetupStatusForUser(full)

	if got.SCM != (setup.SCMPosture{}) {
		t.Errorf("scm = %+v, want zero — host git-credential posture is not a member's business", got.SCM)
	}
	if got.HostProxy.HTTPSProxy != nil || got.HostProxy.HasCredentials {
		t.Errorf("host_proxy = %+v, want zero — the corp proxy topology is operator posture", got.HostProxy)
	}
	// X3-F1: the strip is now DECLARED. Without this the console reads an empty
	// Checks list as "this deployment has no image builder / no sandbox runner"
	// and renders operator-shaped fix advice at a member who cannot act on it.
	if !got.ChecksRedacted {
		t.Error("checks_redacted = false — a member's stripped body must say the detail was withheld, not merely be empty")
	}
	// The ephemeral-disk enforcement word is an operator sizing answer (which
	// substrate binds a run's disk_mib), actionable only on surfaces a member
	// cannot reach — and the strip is structural, so this case is what keeps a
	// later field from riding through on the same struct.
	if got.Runner.EphemeralDiskEnforcement != "" {
		t.Errorf("runner.ephemeral_disk_enforcement = %q, want empty — the enforcement word is operator detail", got.Runner.EphemeralDiskEnforcement)
	}
	if got.Runner.Driver != "" {
		t.Errorf("runner.driver = %q, want empty", got.Runner.Driver)
	}
	// Still there: everything a member's own console needs.
	for name, ok := range map[string]bool{
		"ready":                      got.Ready,
		"llm_ready":                  got.LLMReady,
		"has_runs":                   got.HasRuns,
		"runner.confinement_classes": len(got.Runner.ConfinementClasses) == 1,
		"integrations":               len(got.Integrations) == 1,
	} {
		if !ok {
			t.Errorf("%s did not survive redaction", name)
		}
	}

	// Redaction must not scribble on the caller's own value — the handler keeps
	// using the pre-redaction slices nowhere, but a shared backing array is the
	// kind of aliasing bug that only shows up under a second caller.
	if full.SCM.CredentialHelper != "store" {
		t.Error("redaction mutated its input")
	}
}

// TestSetupStatus_MemberRunnerCarriesOnlyTheKubernetesBit is #1238: a member's
// redacted runner used to carry the confinement classes alone, so their
// console could not tell a Kubernetes install from a Docker host and named the
// /dev/kvm remedy for Vault. The substrate bit survives the member view — and
// it, and the start deadlines the run page's overdue bound follows, are the
// ONLY things added: the driver name, the disk-enforcement word and the
// per-class substrates stay operator-only.
func TestSetupStatus_MemberRunnerCarriesOnlyTheKubernetesBit(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct {
		name string
		rn   runner.Runner
		want bool
	}{
		{"kubernetes", k8sRunner{}, true},
		{"docker", &fakeRunner{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseTestConfig(h, &integStore{govEscapeStore: newGovEscapeStore(&capStore{})})
			cfg.Runner = tc.rn
			cfg.Secrets = &memSecrets{m: map[string][]byte{}}
			cfg.OIDC = &oidc.Authenticator{}
			srv := New(cfg)

			w := doSSO(t, srv, http.MethodGet, "/api/v1/setup/status", ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser), "")
			if w.Code != http.StatusOK {
				t.Fatalf("member: code = %d, want 200; body=%s", w.Code, w.Body.String())
			}
			var raw struct {
				Runner map[string]json.RawMessage `json:"runner"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
				t.Fatalf("decode: %v; body=%s", err, w.Body.String())
			}
			// "driver" is always serialized (no omitempty) — redaction blanks its value.
			for k, v := range raw.Runner {
				switch k {
				case "confinement_classes", "kubernetes", "sandbox_start":
				case "driver":
					if string(v) != `""` {
						t.Errorf("member runner.driver = %s, want the redacted empty string", v)
					}
				default:
					t.Errorf("member runner carries %q — only confinement_classes, the kubernetes bit and the start deadlines may reach a member", k)
				}
			}
			var st SetupStatus
			if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if st.Runner.Kubernetes != tc.want {
				t.Errorf("member runner.kubernetes = %v, want %v", st.Runner.Kubernetes, tc.want)
			}
			if st.Runner.Driver != "" || st.Runner.EphemeralDiskEnforcement != "" || st.Runner.ConfinementSubstrates != nil {
				t.Errorf("member runner leaked operator detail: %+v", st.Runner)
			}

			// The admin view carries the same bit, so the two never disagree.
			_, adminSt := decodeSetupSSO(t, srv, ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin))
			if adminSt.Runner.Kubernetes != tc.want {
				t.Errorf("admin runner.kubernetes = %v, want %v", adminSt.Runner.Kubernetes, tc.want)
			}
		})
	}
}
