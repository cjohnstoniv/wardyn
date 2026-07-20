// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"sort"
	"strings"
	"testing"
)

// TestResolveBedrockAuth_NotConfigured is the common case: an operator who
// never touches Bedrock gets no env/hosts and ready=false, regardless of
// agent or subscription state.
func TestResolveBedrockAuth_NotConfigured(t *testing.T) {
	s := &Server{cfg: Config{Secrets: &memSecrets{m: map[string][]byte{}}}}
	ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, true /* modelRun */, nil)
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
	ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, true /* modelRun */, nil)
	if ba.ready {
		t.Fatal("ready = true with no AWS credential secrets stored; want false (degrade, don't break the run)")
	}
}

// TestResolveBedrockAuth_SubscriptionPreempts: subscription (the resident
// Claude OAuth mount) and Bedrock are mutually exclusive transports;
// subscription must win even when Bedrock is fully configured.
func TestResolveBedrockAuth_SubscriptionPreempts(t *testing.T) {
	s := fullyConfiguredBedrockServer()
	ba := s.resolveBedrockAuth(context.Background(), "claude-code", true /* subscriptionActive */, true /* modelRun */, nil)
	if ba.ready {
		t.Fatal("ready = true with subscription active; want false (subscription pre-empts Bedrock)")
	}
}

// TestResolveBedrockAuth_NonClaudeAgent: Bedrock is a Claude-only transport
// (mirrors the existing AgentAnthropicModel gate); a Codex run never gets it.
func TestResolveBedrockAuth_NonClaudeAgent(t *testing.T) {
	s := fullyConfiguredBedrockServer()
	ba := s.resolveBedrockAuth(context.Background(), "codex-cli", false, true /* modelRun */, nil)
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
	ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, true /* modelRun */, nil)
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
	ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, true /* modelRun */, nil)
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
	ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, false /* modelRun */, nil)
	if ba.ready || ba.env != nil || ba.egressHosts != nil {
		t.Fatalf("verify/scan run (modelRun=false): got ready=%v env=%v hosts=%v, want false/nil/nil (no resident AWS creds)", ba.ready, ba.env, ba.egressHosts)
	}
}

// TestResolveBedrockAuth_OptionalSessionToken: an STS/AssumeRole caller stores
// a session token too; it must ride along as AWS_SESSION_TOKEN.
func TestResolveBedrockAuth_OptionalSessionToken(t *testing.T) {
	s := fullyConfiguredBedrockServer()
	s.cfg.Secrets.(*memSecrets).m[bedrockSessionTokenSecret] = []byte("FQoGZXIvYXdzEtemp-session-token")
	ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, true /* modelRun */, nil)
	if !ba.ready {
		t.Fatal("ready = false; want true")
	}
	if ba.env["AWS_SESSION_TOKEN"] != "FQoGZXIvYXdzEtemp-session-token" {
		t.Errorf("AWS_SESSION_TOKEN = %q, want the stored session token", ba.env["AWS_SESSION_TOKEN"])
	}
}

// TestResolveBedrockAuth_AWSDirMount: host-mode ~/.aws mount path. When
// BedrockAWSConfigDir names an existing dir and no bearer secret is present, the
// run gets the mount (NO resident static keys in env), the SDK-config env pointing
// at the mount, and the SSO egress hosts so the SDK can exchange an SSO token.
func TestResolveBedrockAuth_AWSDirMount(t *testing.T) {
	dir := t.TempDir() // a real, existing dir so the os.Stat fail-safe passes
	s := &Server{cfg: Config{
		BedrockRegion:       "us-east-1",
		BedrockModel:        "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
		BedrockAWSConfigDir: dir,
		BedrockAWSProfile:   "bedrock-sso",
		Secrets:             &memSecrets{m: map[string][]byte{}}, // NO static keys stored
	}}
	ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, true /* modelRun */, nil)
	if !ba.ready || !ba.awsMount {
		t.Fatalf("ready=%v awsMount=%v, want both true", ba.ready, ba.awsMount)
	}
	if ba.bearer {
		t.Fatal("bearer=true on the mount path; want false")
	}
	if ba.awsMountSource != dir {
		t.Errorf("awsMountSource = %q, want %q", ba.awsMountSource, dir)
	}
	// No resident credentials of any kind in env.
	for _, k := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_BEARER_TOKEN_BEDROCK"} {
		if _, ok := ba.env[k]; ok {
			t.Errorf("env[%q] present on the mount path; want absent (SDK reads the mount, nothing resident)", k)
		}
	}
	if ba.env["AWS_CONFIG_FILE"] != "/home/agent/.aws/config" || ba.env["AWS_SHARED_CREDENTIALS_FILE"] != "/home/agent/.aws/credentials" {
		t.Errorf("SDK config env not pointed at the mount: config=%q creds=%q", ba.env["AWS_CONFIG_FILE"], ba.env["AWS_SHARED_CREDENTIALS_FILE"])
	}
	if ba.env["AWS_PROFILE"] != "bedrock-sso" {
		t.Errorf("AWS_PROFILE = %q, want the configured profile", ba.env["AWS_PROFILE"])
	}
	hosts := strings.Join(ba.egressHosts, ",")
	for _, h := range []string{"bedrock-runtime.us-east-1.amazonaws.com", "oidc.us-east-1.amazonaws.com", "portal.sso.us-east-1.amazonaws.com"} {
		if !strings.Contains(hosts, h) {
			t.Errorf("egress hosts %v missing %q (needed for SSO cred exchange)", ba.egressHosts, h)
		}
	}
}

// TestResolveBedrockAuth_AWSDirMountMissingFailsSafe: a configured mount dir that
// doesn't exist must NOT enable the mount — it fails safe to the next path (here,
// no static keys → not ready), never mounting a nonexistent source.
func TestResolveBedrockAuth_AWSDirMountMissingFailsSafe(t *testing.T) {
	s := &Server{cfg: Config{
		BedrockRegion:       "us-east-1",
		BedrockModel:        "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
		BedrockAWSConfigDir: "/nonexistent/path/dot-aws",
		Secrets:             &memSecrets{m: map[string][]byte{}},
	}}
	ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, true, nil)
	if ba.ready || ba.awsMount {
		t.Fatalf("ready=%v awsMount=%v for a nonexistent mount dir; want both false (fail safe)", ba.ready, ba.awsMount)
	}
}

// TestResolveBedrockAuth_BearerBeatsMount: a bedrock-api-key bearer (never-resident)
// wins even when the ~/.aws mount is also configured.
func TestResolveBedrockAuth_BearerBeatsMount(t *testing.T) {
	dir := t.TempDir()
	s := &Server{cfg: Config{
		BedrockRegion:       "us-east-1",
		BedrockModel:        "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
		BedrockAWSConfigDir: dir,
		Secrets:             &memSecrets{m: map[string][]byte{bedrockAPIKeySecret: []byte("bearer-xyz")}},
	}}
	ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, true, nil)
	if !ba.bearer || ba.awsMount {
		t.Fatalf("bearer=%v awsMount=%v, want bearer preferred over the mount", ba.bearer, ba.awsMount)
	}
}

// TestResolveBedrockAuth_AWSSSORegionOverride: when the SSO region differs from the
// Bedrock region, the SSO egress hosts use the SSO region, not the Bedrock one.
func TestResolveBedrockAuth_AWSSSORegionOverride(t *testing.T) {
	dir := t.TempDir()
	s := &Server{cfg: Config{
		BedrockRegion:       "us-west-2",
		BedrockModel:        "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
		BedrockAWSConfigDir: dir,
		BedrockAWSSSORegion: "us-east-1",
		Secrets:             &memSecrets{m: map[string][]byte{}},
	}}
	ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, true, nil)
	hosts := strings.Join(ba.egressHosts, ",")
	if !strings.Contains(hosts, "portal.sso.us-east-1.amazonaws.com") {
		t.Errorf("SSO egress %v should use the SSO region us-east-1", ba.egressHosts)
	}
	if !strings.Contains(hosts, "bedrock-runtime.us-west-2.amazonaws.com") {
		t.Errorf("bedrock egress %v should use the bedrock region us-west-2", ba.egressHosts)
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
		// A ~/.aws mount or a bearer token is a valid credential source on its own —
		// region+model+either must read ready (the "needs setup" bug was gating on
		// CredsPresent alone). The mount/bearer flag alone (no region/model) is
		// configured-but-not-ready.
		{"mount only", SetupBedrock{AWSMount: true}, true, false},
		{"bearer only", SetupBedrock{BearerPresent: true}, true, false},
		{"ready via mount", SetupBedrock{Region: "us-east-1", Model: "m", AWSMount: true}, true, true},
		{"ready via bearer", SetupBedrock{Region: "us-east-1", Model: "m", BearerPresent: true}, true, true},
		{"region+model no creds", SetupBedrock{Region: "us-east-1", Model: "m"}, true, false},
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
