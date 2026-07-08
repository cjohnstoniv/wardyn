// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"sort"
	"testing"
)

// TestResolveBedrockAuth_NotConfigured is the common case: an operator who
// never touches Bedrock gets no env/hosts and ready=false, regardless of
// agent or subscription state.
func TestResolveBedrockAuth_NotConfigured(t *testing.T) {
	s := &Server{cfg: Config{Secrets: &memSecrets{m: map[string][]byte{}}}}
	ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, true /* modelRun */)
	if ba.ready || ba.env != nil || ba.egressHosts != nil {
		t.Fatalf("unconfigured Bedrock: got ready=%v env=%v hosts=%v, want false/nil/nil", ba.ready, ba.env, ba.egressHosts)
	}
}

// TestResolveBedrockAuth_MissingCreds: region+model set but the AWS secret(s)
// aren't stored — a real but non-fatal misconfiguration. The run must not get
// Bedrock (so it falls back to the existing api-key path) rather than crash.
func TestResolveBedrockAuth_MissingCreds(t *testing.T) {
	s := &Server{cfg: Config{
		BedrockRegion: "us-east-1",
		BedrockModel:  "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
		Secrets:       &memSecrets{m: map[string][]byte{}}, // no aws-* secrets stored
	}}
	ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, true /* modelRun */)
	if ba.ready {
		t.Fatal("ready = true with no AWS credential secrets stored; want false (degrade, don't break the run)")
	}
}

// TestResolveBedrockAuth_SubscriptionPreempts: subscription (the resident
// Claude OAuth mount) and Bedrock are mutually exclusive transports;
// subscription must win even when Bedrock is fully configured.
func TestResolveBedrockAuth_SubscriptionPreempts(t *testing.T) {
	s := fullyConfiguredBedrockServer()
	ba := s.resolveBedrockAuth(context.Background(), "claude-code", true /* subscriptionActive */, true /* modelRun */)
	if ba.ready {
		t.Fatal("ready = true with subscription active; want false (subscription pre-empts Bedrock)")
	}
}

// TestResolveBedrockAuth_NonClaudeAgent: Bedrock is a Claude-only transport
// (mirrors the existing AgentAnthropicModel gate); a Codex run never gets it.
func TestResolveBedrockAuth_NonClaudeAgent(t *testing.T) {
	s := fullyConfiguredBedrockServer()
	ba := s.resolveBedrockAuth(context.Background(), "codex-cli", false, true /* modelRun */)
	if ba.ready {
		t.Fatal("ready = true for codex-cli; want false (Bedrock wiring is Claude-only)")
	}
}

// TestResolveBedrockAuth_Ready is the golden path: region + model + both
// required AWS secrets present yields the exact env contract claude-code
// honors for Bedrock, plus both regional egress hosts (data-plane +
// control-plane — omitting the latter 403s an inference-profile model id).
func TestResolveBedrockAuth_Ready(t *testing.T) {
	s := fullyConfiguredBedrockServer()
	ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, true /* modelRun */)
	if !ba.ready {
		t.Fatal("ready = false for a fully-configured Bedrock server; want true")
	}
	if ba.bearer {
		t.Fatal("bearer = true with only SigV4 access keys stored; want resident (access-key) mode")
	}
	want := map[string]string{
		"CLAUDE_CODE_USE_BEDROCK": "1",
		"AWS_REGION":              "us-east-1",
		"AWS_DEFAULT_REGION":      "us-east-1",
		"ANTHROPIC_MODEL":         "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
		"AWS_ACCESS_KEY_ID":       "AKIATESTTESTTESTTEST",
		"AWS_SECRET_ACCESS_KEY":   "wJalrXUtnFEMItesttesttesttesttesttestKEY",
	}
	for k, v := range want {
		if ba.env[k] != v {
			t.Errorf("env[%q] = %q, want %q", k, ba.env[k], v)
		}
	}
	if _, ok := ba.env["AWS_SESSION_TOKEN"]; ok {
		t.Error("AWS_SESSION_TOKEN present with no session-token secret stored; want absent, not empty-string")
	}
	if _, ok := ba.env["AWS_BEARER_TOKEN_BEDROCK"]; ok {
		t.Error("AWS_BEARER_TOKEN_BEDROCK present in resident mode; want absent")
	}
	hosts := append([]string{}, ba.egressHosts...)
	sort.Strings(hosts)
	wantHosts := []string{"bedrock-runtime.us-east-1.amazonaws.com", "bedrock.us-east-1.amazonaws.com"}
	sort.Strings(wantHosts)
	if len(hosts) != len(wantHosts) || hosts[0] != wantHosts[0] || hosts[1] != wantHosts[1] {
		t.Errorf("egress hosts = %v, want %v (data-plane + control-plane)", hosts, wantHosts)
	}
}

// TestResolveBedrockAuth_BearerPreferred: when a bedrock-api-key (bearer) secret
// is present it wins over SigV4 access keys — the never-resident path. The
// sandbox env carries only a PLACEHOLDER bearer (the real token is proxy-injected)
// and NO AWS access keys.
func TestResolveBedrockAuth_BearerPreferred(t *testing.T) {
	s := fullyConfiguredBedrockServer()
	s.cfg.Secrets.(*memSecrets).m[bedrockAPIKeySecret] = []byte("bedrock-bearer-token-xyz")
	ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, true /* modelRun */)
	if !ba.ready || !ba.bearer {
		t.Fatalf("ready=%v bearer=%v, want both true (bearer secret present)", ba.ready, ba.bearer)
	}
	if ba.env["AWS_BEARER_TOKEN_BEDROCK"] == "" || ba.env["AWS_BEARER_TOKEN_BEDROCK"] == "bedrock-bearer-token-xyz" {
		t.Errorf("AWS_BEARER_TOKEN_BEDROCK = %q, want a non-empty PLACEHOLDER (real token is proxy-injected, never resident)", ba.env["AWS_BEARER_TOKEN_BEDROCK"])
	}
	if _, ok := ba.env["AWS_ACCESS_KEY_ID"]; ok {
		t.Error("AWS_ACCESS_KEY_ID present in bearer mode; want absent (never resident)")
	}
	if ba.runtimeHost != "bedrock-runtime.us-east-1.amazonaws.com" {
		t.Errorf("runtimeHost = %q, want the data-plane host for MITM+inject", ba.runtimeHost)
	}
}

// TestResolveBedrockAuth_SkipsNonModelRun is the least-privilege regression
// (security review MED): a verify or scan run makes NO model call, so even a
// fully-configured Bedrock server must NOT hand it the resident AWS creds —
// modelRun=false yields ready=false so the creds never land in a sandbox that
// won't sign a Bedrock request. Same fixture as the golden path, modelRun flips.
func TestResolveBedrockAuth_SkipsNonModelRun(t *testing.T) {
	s := fullyConfiguredBedrockServer()
	ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, false /* modelRun */)
	if ba.ready || ba.env != nil || ba.egressHosts != nil {
		t.Fatalf("verify/scan run (modelRun=false): got ready=%v env=%v hosts=%v, want false/nil/nil (no resident AWS creds)", ba.ready, ba.env, ba.egressHosts)
	}
}

// TestResolveBedrockAuth_OptionalSessionToken: an STS/AssumeRole caller stores
// a session token too; it must ride along as AWS_SESSION_TOKEN.
func TestResolveBedrockAuth_OptionalSessionToken(t *testing.T) {
	s := fullyConfiguredBedrockServer()
	s.cfg.Secrets.(*memSecrets).m[bedrockSessionTokenSecret] = []byte("FQoGZXIvYXdzEtemp-session-token")
	ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, true /* modelRun */)
	if !ba.ready {
		t.Fatal("ready = false; want true")
	}
	if ba.env["AWS_SESSION_TOKEN"] != "FQoGZXIvYXdzEtemp-session-token" {
		t.Errorf("AWS_SESSION_TOKEN = %q, want the stored session token", ba.env["AWS_SESSION_TOKEN"])
	}
}

// fullyConfiguredBedrockServer returns a Server with region/model/creds all
// set — the shared golden-path fixture for the tests above.
func fullyConfiguredBedrockServer() *Server {
	return &Server{cfg: Config{
		BedrockRegion: "us-east-1",
		BedrockModel:  "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
		Secrets: &memSecrets{m: map[string][]byte{
			bedrockAccessKeyIDSecret:     []byte("AKIATESTTESTTESTTEST"),
			bedrockSecretAccessKeySecret: []byte("wJalrXUtnFEMItesttesttesttesttesttestKEY"),
		}},
	}}
}

// TestSetupBedrock_ReadyAndConfigured exercises the pure SetupStatus.Bedrock
// helpers the wizard's bedrock_provider check and llmDetail fold-in rely on.
func TestSetupBedrock_ReadyAndConfigured(t *testing.T) {
	cases := []struct {
		name           string
		b              SetupBedrock
		wantConfigured bool
		wantReady      bool
	}{
		{"untouched", SetupBedrock{}, false, false},
		{"region only", SetupBedrock{Region: "us-east-1"}, true, false},
		{"creds only", SetupBedrock{CredsPresent: true}, true, false},
		{"fully configured", SetupBedrock{Region: "us-east-1", Model: "m", CredsPresent: true}, true, true},
	}
	for _, c := range cases {
		if got := c.b.configured(); got != c.wantConfigured {
			t.Errorf("%s: configured() = %v, want %v", c.name, got, c.wantConfigured)
		}
		if got := c.b.ready(); got != c.wantReady {
			t.Errorf("%s: ready() = %v, want %v", c.name, got, c.wantReady)
		}
	}
}
