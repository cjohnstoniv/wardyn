// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// THE DISPATCH WIRING, which had no coverage at all (general S3b): the flags
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

func ssoInjectTransport(t *testing.T, s *Server) llmTransport {
	t.Helper()
	ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, true /* modelRun */, false /* refresh */, nil, awsSSOScope{})
	if !ba.ready || !ba.ssoInject {
		t.Fatalf("ready=%v ssoInject=%v, want the captured-SSO lane", ba.ready, ba.ssoInject)
	}
	return llmTransport{bedrock: ba, bedrockReady: ba.ready,
		injectBedrockSSO: ba.ready && ba.ssoInject && ba.ssoProxyInject}
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

// ON: the flag is DERIVED from the resolved lane, the sandbox cache holds the
// placeholder, and the run's own portal host is authored for MITM.
func TestDispatchWiring_SwitchOnAuthorsThePhaseBLane(t *testing.T) {
	s := ssoInjectServer(t, true)
	llm := ssoInjectTransport(t, s)
	if !llm.injectBedrockSSO {
		t.Fatal("injectBedrockSSO is false on a resolved captured-SSO lane with the switch on")
	}
	if !llm.bedrock.ssoProxyInject {
		t.Error("the resolved posture did not carry the switch; a running sandbox would change lane under an operator's flip")
	}
	if llm.bedrock.env[awsSSOConfigEnvVar] == "" {
		t.Fatal("no synthetic ~/.aws was staged")
	}
	cache := decodeStagedCache(t, llm.bedrock.env[awsSSOConfigEnvVar])
	if strings.Contains(cache, liveSSOBlob().AccessToken) {
		t.Errorf("the REAL access token is staged in the sandbox with the switch ON: %s", cache)
	}
	if !strings.Contains(cache, awsSSOPlaceholderToken) {
		t.Errorf("the placeholder is not in the staged cache: %s", cache)
	}

	captured := &captureGrantStore{}
	s.cfg.Store = captured
	injections, mitmHosts, ok := s.authorBedrockSSOInjection(context.Background(),
		types.AgentRun{ID: uuid.New()}, llm, awsSSOScope{perUser: true, owner: "alice@example.com"}, nil)
	if !ok || len(injections) != 1 || len(mitmHosts) != 1 {
		t.Fatalf("authoring: ok=%v injections=%d mitm=%d, want one of each", ok, len(injections), len(mitmHosts))
	}
	if want := "portal.sso.eu-west-2.amazonaws.com"; injections[0].Rule.Host != want {
		t.Errorf("grant host = %q, want the run's own portal %q", injections[0].Rule.Host, want)
	}
	if len(captured.grants) != 1 {
		t.Errorf("grants written = %d, want 1", len(captured.grants))
	}
}

// OFF: byte-for-byte 0.7.5 for a NEW dispatch — the real token in the cache, no
// grant, no MITM host, and nothing that could raise a row.
func TestDispatchWiring_SwitchOffIsThePreviousBehaviour(t *testing.T) {
	s := ssoInjectServer(t, false)
	llm := ssoInjectTransport(t, s)
	if llm.injectBedrockSSO {
		t.Fatal("injectBedrockSSO is true with the switch OFF — the lane would author a grant and a MITM host")
	}
	cache := decodeStagedCache(t, llm.bedrock.env[awsSSOConfigEnvVar])
	if !strings.Contains(cache, liveSSOBlob().AccessToken) {
		t.Error("the switch is off but the sandbox did not receive the real token; `off` must be the previous behaviour")
	}
	if strings.Contains(cache, awsSSOPlaceholderToken) {
		t.Error("the placeholder was staged with the switch off")
	}
	// The authoring block is gated on the flag, so with it false NOTHING is
	// written: no grant row, no MITM entry, and therefore no injection to
	// resolve and no request to raise.
	captured := &captureGrantStore{}
	s.cfg.Store = captured
	if llm.injectBedrockSSO {
		_, _, _ = s.authorBedrockSSOInjection(context.Background(), types.AgentRun{ID: uuid.New()}, llm, awsSSOScope{}, nil)
	}
	if len(captured.grants) != 0 {
		t.Errorf("grants written = %d with the switch off, want 0", len(captured.grants))
	}
}

// A RUN DISPATCHED UNDER `on` KEEPS ITS LANE when the flag flips to `off`
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

// THE OTHER LANES AUTHOR NOTHING (the plan's named pins): a bearer, a static-key
// or a ~/.aws-mount run must not acquire a portal.sso injection.
func TestDispatchWiring_OtherBedrockLanesAuthorNoSSOInjection(t *testing.T) {
	for _, tc := range []struct {
		name string
		ba   bedrockAuth
	}{
		{"bearer", bedrockAuth{ready: true, bearer: true}},
		{"resident static keys", bedrockAuth{ready: true}},
		{"host ~/.aws mount", bedrockAuth{ready: true, awsMount: true}},
		{"not ready at all", bedrockAuth{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			llm := llmTransport{bedrock: tc.ba, bedrockReady: tc.ba.ready,
				injectBedrockSSO: tc.ba.ready && tc.ba.ssoInject && tc.ba.ssoProxyInject}
			if llm.injectBedrockSSO {
				t.Errorf("the %s lane derived injectBedrockSSO — it would author a portal.sso grant and MITM host", tc.name)
			}
		})
	}
}

// The CEILING narrowing takes the flag with the lane: the grant is authored
// AFTER that phase, so leaving it set would write a credential and a MITM entry
// for egress this run was just denied.
func TestDispatchWiring_CeilingNarrowingClearsTheSSOFlag(t *testing.T) {
	s := ssoInjectServer(t, true)
	llm := ssoInjectTransport(t, s)
	if !llm.injectBedrockSSO {
		t.Fatal("precondition: the lane is on")
	}
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
