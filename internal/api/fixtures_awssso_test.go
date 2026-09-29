// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/subscription"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Shared fixtures for the AWS sign-in and golden-file tests.

// awsSSOTestFixedNow is the reference "now" the ssoInject tests pin cfg.Now
// to, so expired()/not-expired is deterministic regardless of wall-clock time.
var awsSSOTestFixedNow = time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

// putAWSSSOBlob stores a valid (or deliberately expired) captured SSO blob
// directly into the test server's memSecrets.
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
		// A REAL capture records the OIDC client registration's expiry
		// (wardyn-aws-sso writes it), and it is far enough out that
		// registrationLapsed answers exactly what the ZERO value here answered
		// before: live. Carrying it makes this fixture the ORDINARY shape, so a
		// test asserting the refresh row NAMES the registration expiry asserts
		// the ordinary case rather than the zero-value one this test omits the key
		// for. The cases that want a zero value still set it explicitly.
		RegistrationExpiresAt: awsSSOTestFixedNow.Add(90 * 24 * time.Hour),
	}
	storeSSOBlob(t, s, blob)
	return blob
}

// storeSSOBlob writes an already-built blob over whatever putAWSSSOBlob stored,
// for the cases that need a field putAWSSSOBlob's fixture does not vary (no
// refresh token, a lapsed registration, a rotated pair read back after a
// refresh). It lands where a sign-in to awsSSOTestProvider lands: the test
// owner's own namespace, under the provider's UID.
func storeSSOBlob(t *testing.T, s *Server, blob awsSSOBlob) {
	t.Helper()
	storeSSOBlobFor(t, s, awsSSOTestOwner, blob)
}

// storeSSOBlobFor is storeSSOBlob for another owner's sign-in to
// awsSSOTestProvider.
func storeSSOBlobFor(t *testing.T, s *Server, owner string, blob awsSSOBlob) {
	t.Helper()
	raw, err := json.Marshal(blob)
	if err != nil {
		t.Fatalf("marshal test SSO blob: %v", err)
	}
	if ms, ok := s.cfg.Secrets.(*memSecrets); ok && ms.owned == nil {
		ms.owned = map[string]map[string][]byte{}
	}
	scope := chosenProvider{provider: awsSSOTestProvider(), owner: owner}.awsScope()
	if err := s.cfg.Secrets.For(scope.owner).Put(context.Background(), scope.ssoSecret(), raw); err != nil {
		t.Fatalf("store test SSO blob: %v", err)
	}
}

// The provider and person every captured-session fixture is for.
const (
	awsSSOTestOwner       = "alice@example.com"
	awsSSOTestProviderUID = "u-bedrock-sso-test"
)

// awsSSOTestProvider is the bedrock_sso provider putAWSSSOBlob's blob was
// captured for: its region, portal and pin agree with the blob.
func awsSSOTestProvider() types.ModelProvider {
	return types.ModelProvider{ID: "bedrock-sso", UID: awsSSOTestProviderUID, Kind: types.ModelProviderBedrockSSO,
		Bedrock: &types.BedrockSettings{Region: "us-east-1", SSOStartURL: "https://example.awsapps.com/start",
			SSOAccountID: "123456789012", SSORoleName: "WardynBedrockRole"},
		Harnesses: []types.ProviderHarness{{Harness: "claude-code", Model: "us.anthropic.claude-sonnet-4-5-20250929-v1:0"}}}
}

// awsSSOTestScope is where the test owner's session for awsSSOTestProvider lives.
func awsSSOTestScope() awsSSOScope {
	return chosenProvider{provider: awsSSOTestProvider(), owner: awsSSOTestOwner}.awsScope()
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

// compareOrUpdateGolden marshals got as indented JSON and either writes it to
// path (WARDYN_UPDATE_GOLDEN=1) or compares it byte-for-byte against the
// existing file, failing with a diff-friendly message otherwise. Shared by
// every golden test in this package (small enough that a generic helper beats
// duplicating the read/write/compare dance per fixture).
func compareOrUpdateGolden(t *testing.T, path string, got any) {
	t.Helper()
	want, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("marshal golden %s: %v", path, err)
	}
	want = append(want, '\n')
	if goldenUpdate() {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		return
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (regenerate with WARDYN_UPDATE_GOLDEN=1 go test ./internal/api/ -run 'Golden|CheckIds')", path, err)
	}
	if string(onDisk) != string(want) {
		t.Errorf("golden mismatch for %s (if this is an INTENTIONAL behavior change, regenerate with "+
			"WARDYN_UPDATE_GOLDEN=1 go test ./internal/api/ -run 'Golden|CheckIds' and review the diff before committing "+
			"it):\n--- on disk ---\n%s\n--- recomputed ---\n%s", path, onDisk, want)
	}
}

// fakeSubProvider is a minimal subscription.Provider fake: Current and Peek
// both return the same fixed (token, err) pair — enough to model a wired
// resident/managed subscription for resolveLLMTransport, which only asks
// whether the provider is non-nil (injectSub) or Peek()s it (managedInjectReady).
type fakeSubProvider struct {
	tok subscription.Token
	err error
}

func (f fakeSubProvider) Current(context.Context) (subscription.Token, error) { return f.tok, f.err }
func (f fakeSubProvider) Peek() (subscription.Token, error)                   { return f.tok, f.err }

// wedgedSecrets is a store that is UP but cannot answer — a rotated age key, a
// Postgres blip. Every read errors; nothing is ErrNotFound.
type wedgedSecrets struct{ err error }

func (wedgedSecrets) Name() string                                { return "wedged" }
func (w wedgedSecrets) Put(context.Context, string, []byte) error { return w.err }
func (w wedgedSecrets) Get(context.Context, string) ([]byte, error) {
	return nil, w.err
}
func (w wedgedSecrets) Delete(context.Context, string) error   { return w.err }
func (w wedgedSecrets) List(context.Context) ([]string, error) { return nil, w.err }
func (w wedgedSecrets) For(string) secretstore.Store           { return w }
func (w wedgedSecrets) DeleteEverywhere(context.Context, []string) (int, error) {
	return 0, w.err
}
func (w wedgedSecrets) Holders(context.Context, []string) (map[string][]string, error) {
	return nil, w.err
}

// fullyConfiguredBedrockServer returns a Server with the boot Bedrock region
// and model set — the fixture the grant-author tests hand a built transport to.
func fullyConfiguredBedrockServer() *Server {
	return &Server{cfg: Config{
		BedrockRegion: "us-east-1",
		BedrockModel:  "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
		Secrets:       &memSecrets{m: map[string][]byte{}},
	}}
}

// bedrockOverrideBaseURL / bedrockOverrideHost are the shared PrivateLink
// fixture: a VPC-endpoint base URL and the bare host every Bedrock consumer
// must derive from it.
const (
	bedrockOverrideBaseURL = "https://vpce-0abc1234-bedrock-runtime.us-east-1.vpce.amazonaws.com"
	bedrockOverrideHost    = "vpce-0abc1234-bedrock-runtime.us-east-1.vpce.amazonaws.com"
)

// perUserPortal is the AWS access portal the sign-in fixtures launch against.
const perUserPortal = "https://acme.awsapps.com/start"

// goldenUpdate reports whether golden fixtures in this package should be
// rewritten rather than compared (WARDYN_UPDATE_GOLDEN=1).
func goldenUpdate() bool { return os.Getenv("WARDYN_UPDATE_GOLDEN") == "1" }

// awsSSOTestSite is a site whose one model provider is awsSSOTestProvider.
func awsSSOTestSite() types.SiteConfig {
	return types.SiteConfig{ModelProviders: providerBlock(awsSSOTestProvider())}
}

// awsSSOSignInPath is awsSSOTestProvider's sign-in door; it takes no body.
const awsSSOSignInPath = "/api/v1/model-providers/bedrock-sso/sign-in"
