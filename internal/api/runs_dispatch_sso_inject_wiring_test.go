// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The dispatch wiring, which had no coverage at all (general S3b): the flags
// that decide whether Phase B happens were reachable only through
// authorBedrockSSOInjection called by hand, so the derivation, the kill switch
// and the other lanes' silence were asserted nowhere.

// ssoInjectServer is a Bedrock deployment whose captured-SSO credential is
// stored and live — the lane resolveBedrockAuth selects for ssoInject.
func ssoInjectServer(t *testing.T, proxyInject bool) *Server {
	t.Helper()
	s := fullyConfiguredBedrockServer()
	s.cfg.BedrockRegion = "eu-west-2"
	s.cfg.AWSSSOProxyInject = proxyInject
	s.cfg.Now = func() time.Time { return time.Now().UTC() }
	s.cfg.MaskRegistry = secretmask.NewRegistry()
	blob := liveSSOBlob()
	raw, err := json.Marshal(blob)
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.Secrets.(*memSecrets).m[harnessCredSecretName(awsSSOProvider)] = raw
	return s
}

// dispatchLLM drives the REAL phase — resolveLLMInjections — which is what
// dispatchRun calls: it resolves the transport (so the injectBedrockSSO
// DERIVATION runs), provisions the MITM CA (so that condition runs) and executes
// the authoring block. Nothing here re-types a predicate.
//
// The first shape of these tests built an llmTransport by hand with
// `injectBedrockSSO: ba.ready && ba.ssoInject && ba.ssoProxyInject` and called
// authorBedrockSSOInjection directly, so forcing the real derivation to false
// and detaching the CA condition and the authoring block left every one of them
// green (round-2 F3). A test that re-types the thing it pins pins nothing.
func dispatchLLM(t *testing.T, s *Server, sso awsSSOScope) (dispatchLLMPlan, *captureGrantStore, map[string]string, bool) {
	t.Helper()
	captured := &captureGrantStore{}
	s.cfg.Store = captured
	run := types.AgentRun{ID: uuid.New(), Agent: "claude-code", CreatedBy: "alice@example.com"}
	policy := types.RunPolicySpec{}
	sandboxEnv := map[string]string{}
	site := types.SiteConfig{}
	if sso.perUser {
		site = agentRoster(types.AgentProvider{
			ID: "claude-code", Mechanism: types.AgentMechanismBedrockSSO,
			CredentialSource: types.CredentialSourcePerUser,
		})
	}
	plan, ok := s.resolveLLMInjections(context.Background(), run,
		dispatchParams{Interactive: false, TaskMode: ""},
		&policy, sandboxEnv, nil, "", artifactRedirectPlan{}, false, site, true, false)
	return plan, captured, sandboxEnv, ok
}

// decodeStagedCache pulls the SSO token-cache file out of the staged
// WARDYN_AWS_SSO_CONFIG_B64 records.
//
// DECODED, not substring-matched on the env var: encodeArtifactConfig
// base64s each record, so `strings.Contains(env, token)` is false whatever the
// file says — the negative assertion would have passed on a cache carrying the
// real token in plain sight.
func decodeStagedCache(t *testing.T, env string) string {
	t.Helper()
	for _, rec := range strings.Split(env, "\n") {
		path, b64, found := strings.Cut(rec, "\t")
		if !found || !strings.Contains(path, "sso/cache/") {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			t.Fatalf("staged cache record is not base64: %v", err)
		}
		return string(raw)
	}
	t.Fatalf("no sso token cache in the staged records: %q", env)
	return ""
}

// ON: the real dispatch phase derives the flag, provisions the CA, authors
// exactly one grant on the run's own portal host and the port-qualified MITM
// entry, and stages a placeholder cache.
func TestDispatchWiring_SwitchOnAuthorsThePhaseBLane(t *testing.T) {
	s := ssoInjectServer(t, true)
	plan, captured, sandboxEnv, ok := dispatchLLM(t, s, awsSSOScope{})
	if !ok {
		t.Fatal("the dispatch LLM phase refused an ssoInject run")
	}
	if !plan.llm.injectBedrockSSO {
		t.Fatal("resolveLLMTransport did not derive injectBedrockSSO on a resolved captured-SSO lane with the switch on")
	}

	// EXACTLY ONE api_key grant, on the BARE portal host.
	if len(captured.grants) != 1 {
		t.Fatalf("dispatch wrote %d grants, want exactly 1", len(captured.grants))
	}
	if got := captured.grants[0].Spec.Kind; got != types.GrantAPIKey {
		t.Errorf("grant kind = %q, want api_key", got)
	}
	var scope struct {
		Host       string `json:"host"`
		SecretName string `json:"secret_name"`
	}
	if err := json.Unmarshal(captured.grants[0].Spec.Scope, &scope); err != nil {
		t.Fatalf("grant scope: %v", err)
	}
	if want := "portal.sso.eu-west-2.amazonaws.com"; scope.Host != want {
		t.Errorf("grant host = %q, want the run's own portal %q", scope.Host, want)
	}
	if scope.SecretName != types.AWSSSOAccessTokenSecret {
		t.Errorf("grant secret = %q, want the AWS SSO sentinel", scope.SecretName)
	}

	// The port-qualified MITM entry reached the PLAN, which is what the runner
	// hands the sidecar.
	wantMITM := net.JoinHostPort("portal.sso.eu-west-2.amazonaws.com", "443")
	if len(plan.bedrockMITMHosts) != 1 || plan.bedrockMITMHosts[0] != wantMITM {
		t.Errorf("plan MITM hosts = %v, want exactly %q", plan.bedrockMITMHosts, wantMITM)
	}
	// …and the CA condition fired, or the tunnel is never terminated.
	if plan.mitmCACertPEM == "" || plan.mitmCAKeyPEM == "" {
		t.Error("the per-run MITM CA was not provisioned for an ssoInject dispatch — the portal tunnel would stay opaque")
	}
	if sandboxEnv["WARDYN_MITM_CA_CERT"] == "" && len(sandboxEnv) == 0 {
		t.Error("the dispatch staged no sandbox env at all")
	}

	// The staged cache holds the placeholder, never the session.
	cache := decodeStagedCache(t, plan.llm.bedrock.env[awsSSOConfigEnvVar])
	if strings.Contains(cache, liveSSOBlob().AccessToken) {
		t.Errorf("the REAL access token is staged in the sandbox with the switch ON: %s", cache)
	}
	if !strings.Contains(cache, awsSSOPlaceholderToken) {
		t.Errorf("the placeholder is not in the staged cache: %s", cache)
	}
}

// OFF: a NEW dispatch is the previous behaviour — the real token in the cache,
// and no grant, no MITM entry, nothing that could raise a row.
func TestDispatchWiring_SwitchOffIsThePreviousBehaviour(t *testing.T) {
	s := ssoInjectServer(t, false)
	plan, captured, _, ok := dispatchLLM(t, s, awsSSOScope{})
	if !ok {
		t.Fatal("the dispatch LLM phase refused an ssoInject run with the switch off")
	}
	if plan.llm.injectBedrockSSO {
		t.Fatal("the derivation produced injectBedrockSSO with the switch OFF")
	}
	if len(captured.grants) != 0 {
		t.Errorf("dispatch wrote %d grants with the switch off, want 0", len(captured.grants))
	}
	if len(plan.bedrockMITMHosts) != 0 {
		t.Errorf("dispatch authored MITM hosts %v with the switch off, want none", plan.bedrockMITMHosts)
	}
	cache := decodeStagedCache(t, plan.llm.bedrock.env[awsSSOConfigEnvVar])
	if !strings.Contains(cache, liveSSOBlob().AccessToken) {
		t.Error("the switch is off but the sandbox did not receive the real token; `off` must be the previous behaviour")
	}
	if strings.Contains(cache, awsSSOPlaceholderToken) {
		t.Error("the placeholder was staged with the switch off")
	}
}

// A run dispatched under `on` KEEPS ITS LANE when the flag flips to `off`
// (Codex #5 / the plan's on->off restart case). The switch is read ONCE, at
// dispatch; the resolver and the capture-side resolution never consult it, so a
// HELD run's grant still resolves and its hold still works after the flip.
func TestDispatchWiring_AFlipDoesNotChangeALaneUnderARunningRun(t *testing.T) {
	// Dispatched under ON.
	f := newReauthFixture(t, nil)
	f.putBlob(t, "alice@example.com", deadSSOBlob())
	if w := f.resolve(t); w.Code != http.StatusLocked {
		t.Fatalf("the held run's resolve: code = %d, want 423", w.Code)
	}
	ap := onlyReauthRow(t, f.srv)

	// The operator flips the switch OFF and the daemon restarts.
	f.srv.cfg.AWSSSOProxyInject = false

	// The HELD run is untouched: its request is still open and still answers
	// 423, because nothing on the resolve path reads the switch.
	if w := f.resolve(t); w.Code != http.StatusLocked {
		t.Errorf("after the flip the held run's resolve: code = %d, want 423 — its authored lane must survive", w.Code)
	}
	// …and the owner's sign-in still resolves it.
	loginRun := types.AgentRun{ID: uuid.New(), CreatedBy: "alice@example.com", CreatedAt: ap.RequestedAt.Add(time.Second)}
	f.st.loginRun = loginRun
	f.putBlob(t, "alice@example.com", liveSSOBlob())
	f.srv.resolvePendingReauth(context.Background(), awsSSOScope{perUser: true, owner: "alice@example.com"}, "alice@example.com", loginRun)
	got, err := f.srv.cfg.Approvals.Get(context.Background(), ap.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != types.ApprovalApproved {
		t.Errorf("after the flip the sign-in left the request %s, want APPROVED — a held run must still be recoverable", got.State)
	}
	if w := f.resolve(t); w.Code != http.StatusOK {
		t.Errorf("after the flip and the sign-in: code = %d, want 200 — the run resumes on its authored lane", w.Code)
	}
}

// The other lanes author nothing, through the real dispatch: a bearer, a
// static-key or a ~/.aws-mount run must acquire no portal.sso grant and no MITM
// entry for it.
func TestDispatchWiring_OtherBedrockLanesAuthorNoSSOInjection(t *testing.T) {
	portal := "portal.sso.eu-west-2.amazonaws.com"
	for _, tc := range []struct {
		name  string
		setup func(*Server)
	}{
		{"bearer", func(s *Server) {
			s.cfg.Secrets.(*memSecrets).m[bedrockAPIKeySecret] = []byte("bedrock-bearer-token-xyz")
		}},
		{"resident static keys", func(s *Server) {
			delete(s.cfg.Secrets.(*memSecrets).m, harnessCredSecretName(awsSSOProvider))
		}},
		{"host ~/.aws mount", func(s *Server) {
			delete(s.cfg.Secrets.(*memSecrets).m, harnessCredSecretName(awsSSOProvider))
			dir := t.TempDir()
			s.cfg.BedrockAWSConfigDir = dir
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := ssoInjectServer(t, true) // the switch is ON: only the LANE differs
			tc.setup(s)
			plan, captured, _, ok := dispatchLLM(t, s, awsSSOScope{})
			if !ok {
				t.Fatalf("the dispatch LLM phase refused the %s lane", tc.name)
			}
			if plan.llm.injectBedrockSSO {
				t.Errorf("the %s lane derived injectBedrockSSO", tc.name)
			}
			for _, g := range captured.grants {
				var sc struct {
					Host       string `json:"host"`
					SecretName string `json:"secret_name"`
				}
				_ = json.Unmarshal(g.Spec.Scope, &sc)
				if sc.Host == portal || sc.SecretName == types.AWSSSOAccessTokenSecret {
					t.Errorf("the %s lane authored an AWS SSO injection: %+v", tc.name, sc)
				}
			}
			for _, h := range plan.bedrockMITMHosts {
				if strings.HasPrefix(h, portal) {
					t.Errorf("the %s lane authored a portal.sso MITM entry: %q", tc.name, h)
				}
			}
		})
	}
}

// The CEILING narrowing takes the flag with the lane: the grant is authored
// AFTER that phase, so leaving it set would write a credential and a MITM entry
// for egress this run was just denied.
func TestDispatchWiring_CeilingNarrowingClearsTheSSOFlag(t *testing.T) {
	s := ssoInjectServer(t, true)
	plan, _, _, ok := dispatchLLM(t, s, awsSSOScope{})
	if !ok || !plan.llm.injectBedrockSSO {
		t.Fatal("precondition: the real dispatch derived the lane")
	}
	llm := plan.llm
	var mitm []string
	lanes := narrowCeilingBedrockLane(
		dispatchCeiling{deny: llm.bedrock.egressHosts}, &llm, &mitm, map[string]string{})
	if len(lanes) == 0 {
		t.Fatal("the ceiling did not narrow a denied Bedrock lane")
	}
	if llm.injectBedrockSSO {
		t.Error("the narrowed lane kept injectBedrockSSO — dispatch would author a grant for egress the ceiling denied")
	}
}
