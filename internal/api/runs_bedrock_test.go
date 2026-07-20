// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
)

// awsSSOTestFixedNow is the reference "now" the ssoInject tests pin cfg.Now
// to, so expired()/not-expired is deterministic regardless of wall-clock time.
var awsSSOTestFixedNow = time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

// putAWSSSOBlob stores a valid (or deliberately expired) captured SSO blob
// directly into the test server's memSecrets, mirroring how bedrock_test.go
// seeds bedrockAPIKeySecret / bedrockAccessKeyIDSecret.
func putAWSSSOBlob(t *testing.T, s *Server, expiresAt time.Time) awsSSOBlob {
	t.Helper()
	blob := awsSSOBlob{
		AccessToken:  "sso-access-token-1234567890",
		RefreshToken: "sso-refresh-token-1234567890",
		ClientID:     "sso-client-id",
		ClientSecret: "sso-client-secret-1234567890",
		StartURL:     "https://example.awsapps.com/start",
		Region:       "us-east-1",
		AccountID:    "123456789012",
		RoleName:     "WardynBedrockRole",
		ExpiresAt:    expiresAt,
		CapturedAt:   awsSSOTestFixedNow.Add(-time.Hour),
	}
	raw, err := json.Marshal(blob)
	if err != nil {
		t.Fatalf("marshal test SSO blob: %v", err)
	}
	s.cfg.Secrets.(*memSecrets).m[harnessCredSecretName(awsSSOProvider)] = raw
	return blob
}

// decodeSSOFiles parses the awsSSOConfigEnvVar payload back into a
// path->content map (mirrors what agent-run-lib.sh's materialize function
// would do), for asserting on the generated file contents.
func decodeSSOFiles(t *testing.T, payload string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, line := range strings.Split(payload, "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			t.Fatalf("malformed record in %s: %q", awsSSOConfigEnvVar, line)
		}
		content, err := base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			t.Fatalf("base64 decode %s: %v", parts[0], err)
		}
		out[parts[0]] = string(content)
	}
	return out
}

// TestResolveBedrockAuth_SSOInject_WinsOverMountAndStaticKeys: a present,
// non-expired captured SSO blob wins over both the ~/.aws host mount and
// static SigV4 keys (precedence: bearer > ssoInject > awsMount > static-keys).
func TestResolveBedrockAuth_SSOInject_WinsOverMountAndStaticKeys(t *testing.T) {
	dir := t.TempDir()
	s := fullyConfiguredBedrockServer() // has static AWS keys stored
	s.cfg.BedrockAWSConfigDir = dir     // and a mount configured
	s.cfg.MaskRegistry = secretmask.NewRegistry()
	s.cfg.Now = func() time.Time { return awsSSOTestFixedNow }
	blob := putAWSSSOBlob(t, s, awsSSOTestFixedNow.Add(time.Hour)) // not expired

	ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, true /* modelRun */, nil)
	if !ba.ready || !ba.ssoInject {
		t.Fatalf("ready=%v ssoInject=%v, want both true", ba.ready, ba.ssoInject)
	}
	if ba.bearer || ba.awsMount {
		t.Fatalf("bearer=%v awsMount=%v, want both false (ssoInject beats the mount and static keys)", ba.bearer, ba.awsMount)
	}
	for _, k := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_BEARER_TOKEN_BEDROCK"} {
		if _, ok := ba.env[k]; ok {
			t.Errorf("env[%q] present in ssoInject mode; want absent (no static keys, no bearer)", k)
		}
	}
	if ba.env["AWS_CONFIG_FILE"] != sandboxAWSDir+"/config" {
		t.Errorf("AWS_CONFIG_FILE = %q, want %q", ba.env["AWS_CONFIG_FILE"], sandboxAWSDir+"/config")
	}
	if ba.env["AWS_SHARED_CREDENTIALS_FILE"] != sandboxAWSDir+"/credentials" {
		t.Errorf("AWS_SHARED_CREDENTIALS_FILE = %q, want %q", ba.env["AWS_SHARED_CREDENTIALS_FILE"], sandboxAWSDir+"/credentials")
	}
	if ba.env["AWS_PROFILE"] != awsSSOProfileName {
		t.Errorf("AWS_PROFILE = %q, want %q", ba.env["AWS_PROFILE"], awsSSOProfileName)
	}
	payload := ba.env[awsSSOConfigEnvVar]
	if payload == "" {
		t.Fatal("WARDYN_AWS_SSO_CONFIG_B64 empty, want the generated config + SSO cache file")
	}
	files := decodeSSOFiles(t, payload)
	cfgFile, ok := files[".aws/config"]
	if !ok {
		t.Fatal(".aws/config missing from the generated payload")
	}
	for _, want := range []string{
		"[sso-session " + awsSSOProfileName + "]",
		"sso_start_url = " + blob.StartURL,
		"sso_region = " + blob.Region,
		"[profile " + awsSSOProfileName + "]",
		"sso_account_id = " + blob.AccountID,
		"sso_role_name = " + blob.RoleName,
	} {
		if !strings.Contains(cfgFile, want) {
			t.Errorf(".aws/config missing %q; got:\n%s", want, cfgFile)
		}
	}
	cacheKey := ".aws/sso/cache/" + awsSSOCacheFileName(awsSSOProfileName) + ".json"
	cacheFile, ok := files[cacheKey]
	if !ok {
		var gotKeys []string
		for k := range files {
			gotKeys = append(gotKeys, k)
		}
		t.Fatalf("%s missing from the generated payload; got keys %v", cacheKey, gotKeys)
	}
	var cache map[string]any
	if err := json.Unmarshal([]byte(cacheFile), &cache); err != nil {
		t.Fatalf("SSO cache file is not valid JSON: %v", err)
	}
	if cache["accessToken"] != blob.AccessToken {
		t.Errorf("cache accessToken = %v, want %q", cache["accessToken"], blob.AccessToken)
	}
	if cache["startUrl"] != blob.StartURL {
		t.Errorf("cache startUrl = %v, want %q", cache["startUrl"], blob.StartURL)
	}
	hosts := strings.Join(ba.egressHosts, ",")
	for _, h := range []string{"bedrock-runtime.us-east-1.amazonaws.com", "oidc.us-east-1.amazonaws.com", "portal.sso.us-east-1.amazonaws.com"} {
		if !strings.Contains(hosts, h) {
			t.Errorf("egress hosts %v missing %q", ba.egressHosts, h)
		}
	}
	// The access token must be registered globally so it's masked from every
	// run's stream, not just the run that resolved it.
	snap := s.cfg.MaskRegistry.Snapshot(uuid.Nil)
	found := false
	for _, v := range snap {
		if string(v) == blob.AccessToken {
			found = true
		}
	}
	if !found {
		t.Error("captured SSO access token not registered in MaskRegistry (want AddGlobal)")
	}
}

// TestResolveBedrockAuth_SSOInject_ExpiredFallsThrough: an EXPIRED captured
// blob must NOT be used — the run falls through to the next credential mode
// (here, the resident static keys) rather than getting a dead token.
func TestResolveBedrockAuth_SSOInject_ExpiredFallsThrough(t *testing.T) {
	s := fullyConfiguredBedrockServer()
	s.cfg.MaskRegistry = secretmask.NewRegistry()
	s.cfg.Now = func() time.Time { return awsSSOTestFixedNow }
	putAWSSSOBlob(t, s, awsSSOTestFixedNow.Add(-time.Minute)) // already expired

	ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, true /* modelRun */, nil)
	if !ba.ready {
		t.Fatal("ready = false; want true (falls through to the resident static-key path)")
	}
	if ba.ssoInject {
		t.Fatal("ssoInject = true with an expired blob; want false (must not use a dead token)")
	}
	if ba.env["AWS_ACCESS_KEY_ID"] == "" {
		t.Error("expected fallthrough to the resident static-key path, but AWS_ACCESS_KEY_ID is absent")
	}
}

// TestResolveBedrockAuth_BearerBeatsSSOInject: an explicit bearer secret still
// wins over a present, valid captured SSO credential.
func TestResolveBedrockAuth_BearerBeatsSSOInject(t *testing.T) {
	s := fullyConfiguredBedrockServer()
	s.cfg.MaskRegistry = secretmask.NewRegistry()
	s.cfg.Now = func() time.Time { return awsSSOTestFixedNow }
	s.cfg.Secrets.(*memSecrets).m[bedrockAPIKeySecret] = []byte("bedrock-bearer-token-xyz")
	putAWSSSOBlob(t, s, awsSSOTestFixedNow.Add(time.Hour))

	ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, true /* modelRun */, nil)
	if !ba.bearer || ba.ssoInject {
		t.Fatalf("bearer=%v ssoInject=%v, want bearer preferred over ssoInject", ba.bearer, ba.ssoInject)
	}
}

// TestResolveBedrockAuth_SSOInject_AbsentBlob: no captured credential at all
// (the common case pre-login) falls through cleanly to the mount/static-key
// paths, same as before this mode existed.
func TestResolveBedrockAuth_SSOInject_AbsentBlob(t *testing.T) {
	s := fullyConfiguredBedrockServer()
	s.cfg.MaskRegistry = secretmask.NewRegistry()
	s.cfg.Now = func() time.Time { return awsSSOTestFixedNow }
	// No putAWSSSOBlob call: secret store has no aws-sso credential.

	ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, true /* modelRun */, nil)
	if !ba.ready || ba.ssoInject {
		t.Fatalf("ready=%v ssoInject=%v, want ready=true via the static-key fallback, ssoInject=false", ba.ready, ba.ssoInject)
	}
}
