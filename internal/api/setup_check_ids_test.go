// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// This file pins the SET of SetupCheck.ID values GET /api/v1/setup/status emits
// across a small matrix of fixtures (see TestSetupCheckIds_Golden). Callers key
// real behavior off specific ids — most notably `wardyn subscription` (see
// cmd/wardyn/subscription.go), which decodes the raw JSON and looks for
// "harness_credential" rather than sharing a Go type with internal/api — so a
// check silently renamed or dropped is a real regression a full-content diff
// would bury among prose/detail wording changes. TestSetupCheckIds_Golden exists
// to make an id rename/drop loud; TestSetupCheckIds_HarnessCredentialContract
// below is the named assertion for that one CLI-load-bearing id.
//
// Regenerate with: WARDYN_UPDATE_GOLDEN=1 go test ./internal/api/ -run 'Golden|CheckIds'

// setupCheckIdsStore is a minimal fake satisfying the store calls
// handleSetupStatus makes when a Store is configured: GetSiteConfig (feeds the
// site_config/artifact_repo rows) and ListRuns (feeds has_runs). Embeds a nil
// store.Store for everything else — handleSetupStatus calls nothing further
// when Runner/Secrets/GitHubRulesets are unset, which every fixture below
// leaves unset. GetCapabilityEnforcement feeds the #19b permissions_posture
// row — an empty map (every kind fail-open) is the realistic zero-config
// answer, same as GetSiteConfig/ListRuns returning zero values above.
type setupCheckIdsStore struct {
	store.Store
	sc types.SiteConfig
}

func (s setupCheckIdsStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return s.sc, nil
}
func (s setupCheckIdsStore) ListRuns(context.Context) ([]types.AgentRun, error) {
	return nil, nil
}
func (s setupCheckIdsStore) GetCapabilityEnforcement(context.Context) (map[string]bool, error) {
	return nil, nil
}

// setupCheckIds runs GET /api/v1/setup/status and returns the deduped, sorted
// set of emitted check ids.
//
// It neutralizes two sources of REAL host-machine detection that
// handleSetupStatus performs and that no Config field controls, so the golden
// reflects the fixture, not whatever machine happens to run `go test`:
//
//   - setup.DetectCLIProviders reads ~/.claude/.credentials.json and
//     ~/.codex/auth.json under $HOME. Without resetting HOME, a sandbox that
//     happens to have a resident Claude/Codex CLI login (this very agent
//     harness, for one) spuriously adds composer_llm_ceiling /
//     claude_subscription_staging to every fixture regardless of Config.
//   - setup.DetectPlatform reports the REAL host OS/WSL-ness, which $HOME
//     cannot neutralize and which this golden has no business asserting
//     (none of the fixtures below are about platform) — its ids are dropped
//     from the returned set so a WSL2 dev box and a plain Linux CI runner
//     produce the same golden.
func setupCheckIds(t *testing.T, srv *Server) []string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	code, st := decodeSetup(t, srv, adminToken)
	if code != 200 {
		t.Fatalf("GET /setup/status: code = %d", code)
	}
	hostOnly := map[string]bool{"platform_wsl": true, "platform_macos": true}
	seen := map[string]bool{}
	for _, c := range st.Checks {
		if !hostOnly[c.ID] {
			seen[c.ID] = true
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// TestSetupCheckIds_Golden pins the id SET across a small fixture matrix: a
// bare config, one with a Store wired (site-config/has-runs rows become
// possible), one with the Bedrock knobs touched, and one with both GitHub App
// secrets present (the scm_provider "safest lane" row).
func TestSetupCheckIds_Golden(t *testing.T) {
	got := map[string][]string{
		"bare": setupCheckIds(t, New(Config{AdminToken: adminToken})),

		// B4: a k8s-shaped Runner (see k8sRunner in setup_test.go) surfaces the
		// new k8s_egress_containment row — absent on every other fixture here,
		// which all leave Runner unset (Driver "none").
		"with_k8s_runner": setupCheckIds(t, New(Config{
			AdminToken: adminToken,
			Runner:     k8sRunner{networkPolicy: true},
		})),

		"with_store": setupCheckIds(t, New(Config{
			AdminToken: adminToken,
			Store: setupCheckIdsStore{sc: types.SiteConfig{
				UpstreamProxySecretRef: "corp-proxy-url",
				EgressRedirects:        []types.EgressRedirect{{From: "https://registry.npmjs.org/", To: "https://artifactory.corp/npm", Ecosystem: "npm"}},
				ScmHosts:               []string{"ghes.corp.example"},
			}},
		})),

		"with_bedrock_knobs": setupCheckIds(t, New(Config{
			AdminToken:    adminToken,
			BedrockRegion: "us-east-1", BedrockModel: "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
			Secrets: &memSecrets{m: map[string][]byte{
				bedrockAccessKeyIDSecret:     []byte("AKIATESTTESTTESTTEST"),
				bedrockSecretAccessKeySecret: []byte("wJalrXUtnFEMItesttesttesttesttesttestKEY"),
			}},
		})),

		"with_github_app_secrets": setupCheckIds(t, New(Config{
			AdminToken: adminToken,
			Secrets: &memSecrets{m: map[string][]byte{
				secretGitHubAppID:  []byte("123456"),
				secretGitHubAppKey: []byte("-----BEGIN PRIVATE KEY-----\ntest\n-----END PRIVATE KEY-----\n"),
			}},
		})),

		// OIDC configured (a zero-value Authenticator — the rbac_test.go idiom;
		// Middleware/decodeSession never touch its unset provider/verifier), the
		// role map left unset, and an https redirect with WARDYN_TLS_TERMINATED
		// not reflected in OIDCSecureCookies: surfaces BOTH new B1 checks,
		// sso_rbac and tls_cookie_posture, each as a WARN.
		"with_oidc": setupCheckIds(t, New(Config{
			AdminToken:      adminToken,
			OIDC:            &oidc.Authenticator{},
			OIDCRedirectURL: "https://wardyn.example.com/auth/callback",
		})),
	}
	compareOrUpdateGolden(t, "testdata/setup_check_ids_golden.json", got)
}

// TestSetupCheckIds_HarnessCredentialContract pins that a captured Wardyn-
// managed Claude subscription (container-login setup-token) makes
// /setup/status emit a check with id EXACTLY "harness_credential" —
// cmd/wardyn/subscription.go decodes the raw JSON and looks for that literal
// string (there is no shared Go type gating this at compile time), so a rename
// here would silently break `wardyn subscription status` with no build failure
// to catch it.
func TestSetupCheckIds_HarnessCredentialContract(t *testing.T) {
	blob := managedCredBlob{Token: "sk-ant-oat01-test", CapturedAt: time.Now().UTC()}
	raw, err := json.Marshal(blob)
	if err != nil {
		t.Fatalf("marshal managed blob: %v", err)
	}
	srv := New(Config{
		AdminToken: adminToken,
		Secrets:    &memSecrets{m: map[string][]byte{harnessCredSecretName("anthropic"): raw}},
	})
	ids := setupCheckIds(t, srv)
	if !slices.Contains(ids, "harness_credential") {
		t.Fatalf("checks = %v, want \"harness_credential\" present when a managed subscription blob is stored "+
			"(`wardyn subscription status` depends on this exact id)", ids)
	}
}
