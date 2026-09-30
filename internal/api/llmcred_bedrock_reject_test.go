// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// siteOnlyStore answers the one read denyAlwaysReject makes: the site config.
type siteOnlyStore struct {
	store.Store
	sc types.SiteConfig
}

func (s siteOnlyStore) GetSiteConfig(context.Context) (types.SiteConfig, error) { return s.sc, nil }

// TestDenyAlwaysReject_BedrockLaneIsGuarded: denyAlwaysReject — the guard
// whose whole job is to refuse a deny·always that permanently breaks a
// workspace's model access — must count a Bedrock provider's hosts as model
// hosts: its bearer lane TLS-MITMs bedrock-runtime and injects the
// Authorization header proxy-side, which is exactly the "proxy-side credential
// injection refuses a denied host" failure the guard's own message names. The
// hosts come from the provider row only; the boot Bedrock region is retired.
//
// The anthropic row is here so a fix that simply refused everything could not
// pass, and the unrelated row so the guard still lets ordinary hosts through.
func TestDenyAlwaysReject_BedrockLaneIsGuarded(t *testing.T) {
	bedrock := func(baseURL string) types.SiteConfig {
		return types.SiteConfig{ModelProviders: &types.ModelProviders{Providers: []types.ModelProvider{{
			ID: "bedrock", UID: "uid-bedrock", Kind: types.ModelProviderBedrockBearer,
			Bedrock:   &types.BedrockSettings{Region: "us-east-1", BaseURL: baseURL},
			Harnesses: []types.ProviderHarness{{Harness: "claude-code", Model: "us.anthropic.claude-x"}},
		}}}}
	}
	for _, tc := range []struct {
		name    string
		sc      types.SiteConfig
		host    string
		refused bool
	}{
		{"anthropic (already guarded)", types.SiteConfig{}, "api.anthropic.com", true},
		{"bedrock data plane", bedrock(""), "bedrock-runtime.us-east-1.amazonaws.com", true},
		{"bedrock control plane", bedrock(""), "bedrock.us-east-1.amazonaws.com", true},
		{"bedrock data plane, trailing dot + case", bedrock(""), "Bedrock-Runtime.US-East-1.amazonaws.com.", true},
		{
			"provider bedrock.base_url override host",
			bedrock("https://vpce-abc.bedrock-runtime.us-east-1.vpce.amazonaws.com"),
			"vpce-abc.bedrock-runtime.us-east-1.vpce.amazonaws.com", true,
		},
		{"an ordinary app host stays decidable", bedrock(""), "api.example.com", false},
		{"bedrock host with no Bedrock provider stays decidable", types.SiteConfig{}, "bedrock-runtime.us-east-1.amazonaws.com", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{cfg: Config{Store: siteOnlyStore{sc: tc.sc}}}
			why := s.denyAlwaysReject(context.Background(), types.Workspace{}, tc.host)
			if refused := why != ""; refused != tc.refused {
				t.Fatalf("denyAlwaysReject(%q) = %q (refused=%v), want refused=%v", tc.host, why, refused, tc.refused)
			}
			if tc.refused && !strings.Contains(why, "permanently break model access") {
				t.Errorf("refusal for %q did not come from the model-provider arm: %q", tc.host, why)
			}
		})
	}
}
