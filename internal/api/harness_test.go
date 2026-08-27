// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"reflect"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/version"
)

// TestAgentImage pins agentImage's behavior across the harness-catalog rewire:
// a WARDYN_AGENT_IMAGES override always wins (even for an agent the catalog has
// never heard of), and the ghcr.io fallback convention is byte-identical to the
// pre-catalog code for every id whose ImageKey equals its ID.
//
// claude-code is the ONE row where it does not, since 0.7: its ImageKey points
// at `base`, because agent-claude-code is not published and the old fallback
// therefore 404'd on every published install. That divergence is the point of
// the re-point, so it is asserted explicitly rather than folded into the loop.
func TestAgentImage(t *testing.T) {
	ids := []string{"codex-cli", "none", "oracle", "some-random-string"}
	// Convention images carry the DAEMON'S OWN version, not a floating :latest a
	// later release could re-point under a version-pinned fleet (D19).
	tag := ":" + version.Version

	t.Run("ghcr fallback: no WARDYN_AGENT_IMAGES override", func(t *testing.T) {
		for _, id := range ids {
			if got, want := agentImage(id, nil), "ghcr.io/cjohnstoniv/agent-"+id+tag; got != want {
				t.Errorf("agentImage(%q, nil) = %q, want %q", id, got, want)
			}
		}
	})

	t.Run("claude-code falls back to the PUBLISHED base image", func(t *testing.T) {
		if got, want := agentImage("claude-code", nil), "ghcr.io/cjohnstoniv/agent-base"+tag; got != want {
			t.Errorf("agentImage(\"claude-code\", nil) = %q, want %q — agent-claude-code is not published, so any other fallback 404s on a published install", got, want)
		}
	})

	t.Run("WARDYN_AGENT_IMAGES override wins for its own id only", func(t *testing.T) {
		images := map[string]string{"claude-code": "example.com/custom/claude:v9"}
		if got, want := agentImage("claude-code", images), "example.com/custom/claude:v9"; got != want {
			t.Errorf("agentImage(claude-code, override) = %q, want %q", got, want)
		}
		// Every other id is untouched by an override naming a different agent —
		// each still falls through to its own ghcr convention.
		for _, id := range []string{"codex-cli", "none", "oracle", "some-random-string"} {
			if got, want := agentImage(id, images), "ghcr.io/cjohnstoniv/agent-"+id+tag; got != want {
				t.Errorf("agentImage(%q, override-for-claude-code) = %q, want %q", id, got, want)
			}
		}
	})
}

// TestAgentImageTag pins the version-tag fallback (D19): a build with a version
// string tags the convention image with it (per-semver publish); only an empty
// version string falls back to the floating :latest.
func TestAgentImageTag(t *testing.T) {
	if got := agentImageTag("0.6.0"); got != "0.6.0" {
		t.Errorf("agentImageTag(%q) = %q, want the version tag", "0.6.0", got)
	}
	if got := agentImageTag(""); got != "latest" {
		t.Errorf("agentImageTag(\"\") = %q, want \"latest\" (last resort)", got)
	}
}

// TestAgentLLMProvider pins agentLLMProvider's outputs literally as the old
// claude-code/codex-cli switch produced them, for every id in the shared
// rewire test set.
func TestAgentLLMProvider(t *testing.T) {
	tests := []struct {
		agent  string
		want   llmProvider
		wantOK bool
	}{
		{"claude-code", llmProvider{host: "api.anthropic.com", header: "x-api-key", format: "%s", secret: "anthropic-api-key"}, true},
		{"codex-cli", llmProvider{host: "api.openai.com", header: "Authorization", format: "Bearer %s", secret: "openai-api-key"}, true},
		{"none", llmProvider{}, false},
		{"oracle", llmProvider{}, false},
		{"some-random-string", llmProvider{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.agent, func(t *testing.T) {
			got, ok := agentLLMProvider(tc.agent)
			if ok != tc.wantOK {
				t.Fatalf("agentLLMProvider(%q) ok = %v, want %v", tc.agent, ok, tc.wantOK)
			}
			if got != tc.want {
				t.Errorf("agentLLMProvider(%q) = %+v, want %+v", tc.agent, got, tc.want)
			}
		})
	}
}

// TestAgentHarnessLoginCatalogRewire pins agentHarnessLogin's outputs
// literally as the old claude-code/aws-sso switch produced them, for every id
// in the shared rewire test set, PLUS the aws-sso auxiliary row the rewire is
// required to leave untouched (harnessLogin has a slice field, so equality is
// reflect.DeepEqual, not ==). Complements the pre-existing TestAgentHarnessLogin
// in harnesscred_test.go, which this rewire must keep passing unmodified.
func TestAgentHarnessLoginCatalogRewire(t *testing.T) {
	claudeCode := harnessLogin{
		provider: "anthropic",
		agent:    "claude-code",
		// The login lane does NOT follow the catalog's ImageKey re-point to
		// `base`: a login sandbox must carry the vendor CLI it is logging into,
		// and agent-base ships none, so the box would come up with `claude` not
		// on PATH and the flow could never complete.
		loginImageKey: "claude-code",
		secretName:    harnessCredSecretName("anthropic"),
		sentinel:      types.ManagedOAuthSecret,
		injectHost:    subscriptionInjectionHost,
		tokenPrefix:   "sk-ant-oat",
		egress:        []string{"claude.com", "platform.claude.com", "console.anthropic.com", "api.anthropic.com"},
	}
	awsSSO := harnessLogin{
		provider:          awsSSOProvider,
		agent:             awsSSOAgent,
		secretName:        harnessCredSecretName(awsSSOProvider),
		sentinel:          "",
		injectHost:        "",
		tokenPrefix:       "",
		egress:            []string{"*.awsapps.com"},
		regionalSSOEgress: true,
		captureViaHelper:  true,
	}

	tests := []struct {
		agent  string
		want   harnessLogin
		wantOK bool
	}{
		{"claude-code", claudeCode, true},
		{"codex-cli", harnessLogin{}, false},
		{"none", harnessLogin{}, false},
		{"oracle", harnessLogin{}, false},
		{"some-random-string", harnessLogin{}, false},
		{awsSSOAgent, awsSSO, true}, // the auxiliary entry the rewire must leave untouched
	}
	for _, tc := range tests {
		t.Run(tc.agent, func(t *testing.T) {
			got, ok := agentHarnessLogin(tc.agent)
			if ok != tc.wantOK {
				t.Fatalf("agentHarnessLogin(%q) ok = %v, want %v", tc.agent, ok, tc.wantOK)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("agentHarnessLogin(%q) = %+v, want %+v", tc.agent, got, tc.want)
			}
		})
	}
}

// TestHarnessLoginByProvider pins the provider-keyed lookup that both
// agentHarnessLogin paths (the harness catalog for claude-code, the untouched
// direct case for aws-sso) must still resolve through.
func TestHarnessLoginByProvider(t *testing.T) {
	if _, ok := harnessLoginByProvider("anthropic"); !ok {
		t.Error(`harnessLoginByProvider("anthropic") ok = false, want true`)
	}
	if _, ok := harnessLoginByProvider(awsSSOProvider); !ok {
		t.Errorf("harnessLoginByProvider(%q) ok = false, want true", awsSSOProvider)
	}
	if _, ok := harnessLoginByProvider("bogus-provider"); ok {
		t.Error(`harnessLoginByProvider("bogus-provider") ok = true, want false`)
	}
}
