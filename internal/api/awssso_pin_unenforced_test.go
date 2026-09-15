// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// perUserSSORow is the roster shape S-3 is about: each person signs in, and
// this deployment stores whatever account and role that sign-in names.
func perUserSSORow(account string) types.SiteConfig {
	return agentRoster(types.AgentProvider{
		ID: modelAccessAgent, Mechanism: types.AgentMechanismBedrockSSO,
		CredentialSource: types.CredentialSourcePerUser,
		SSOStartURL:      "https://my-sso.awsapps.com/start",
		SSOAccountID:     account,
		SSORoleName:      map[bool]string{true: "BedrockRunner", false: ""}[account != ""],
	})
}

// TestBedrockSSOPinUnenforced (lens-S S-3) is the predicate behind both the
// boot warning and the bedrock_provider row: NOTHING server-side constrains
// which account/role a sign-in may store — no roster pin, and a configured
// model that names no account. It is the configuration the code itself calls
// the common case, and finding 1's original failure mode survives on it.
func TestBedrockSSOPinUnenforced(t *testing.T) {
	const bareModel = "us.anthropic.claude-sonnet-4-5-20250929-v1:0"
	const arnModel = "arn:aws:bedrock:us-east-1:111111111111:inference-profile/us.anthropic.claude-v1:0"
	for name, c := range map[string]struct {
		site  types.SiteConfig
		model string
		want  bool
	}{
		"unpinned row + bare model":      {site: perUserSSORow(""), model: bareModel, want: true},
		"unpinned row + no model at all": {site: perUserSSORow(""), model: "", want: true},
		"unpinned row + account ARN":     {site: perUserSSORow(""), model: arnModel},
		"pinned row + bare model":        {site: perUserSSORow("111111111111"), model: bareModel},
		"no roster at all":               {site: types.SiteConfig{}, model: bareModel},
		"shared row": {model: bareModel, site: agentRoster(types.AgentProvider{
			ID: modelAccessAgent, Mechanism: types.AgentMechanismBedrockSSO,
		})},
		"disabled row": {model: bareModel, site: agentRoster(types.AgentProvider{
			ID: modelAccessAgent, Mechanism: types.AgentMechanismBedrockSSO,
			CredentialSource: types.CredentialSourcePerUser, Disabled: true,
		})},
	} {
		t.Run(name, func(t *testing.T) {
			if got := BedrockSSOPinUnenforced(c.site, c.model); got != c.want {
				t.Errorf("BedrockSSOPinUnenforced = %v, want %v", got, c.want)
			}
		})
	}
}

// TestBedrockProviderCheck_UnenforcedPinWarns (lens-S S-3). The ONLY audible
// signal before this was BedrockModelARNNamesNoAccount, which fires exclusively
// when the model LOOKS like an ARN — the typo case, never the bare-id case the
// code itself calls "most often" true. So the deployment where a sandbox-chosen
// account AND role are stored unchecked was the one that said nothing at all.
func TestBedrockProviderCheck_UnenforcedPinWarns(t *testing.T) {
	srv := New(Config{
		BedrockRegion: "us-east-1", BedrockModel: "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
		Secrets: &memSecrets{m: map[string][]byte{}},
	})
	// A caller whose OWN sign-in is captured, so every other arm reads ok — the
	// row that used to say "Bedrock is configured" and nothing else.
	bedrock := srv.setupBedrock(context.Background(), map[string]bool{}, awsSSOScope{})
	bedrock.SSOPresent, bedrock.Ready = true, true

	chk, ok := bedrockProviderCheck(bedrock, true)
	if !ok {
		t.Fatal("a region+model-configured Bedrock row must always surface a check")
	}
	if chk.Status != "warn" {
		t.Errorf("status = %q, want warn — nothing constrains which account a sign-in stores", chk.Status)
	}
	if chk.Detail != bedrockUnenforcedPinDetail || chk.Fix != bedrockUnenforcedPinFix {
		t.Errorf("row text drifted from the DRAFT sentences:\n got %+v", chk)
	}
	if !strings.Contains(chk.Fix, "sso_account_id") {
		t.Errorf("Fix = %q, want it to name the pin", chk.Fix)
	}
	// And the same row with the pin enforced is untouched.
	if chk, _ := bedrockProviderCheck(bedrock, false); chk.Status != "ok" {
		t.Errorf("a pinned deployment's row = %+v, want the unchanged ok row", chk)
	}
}
