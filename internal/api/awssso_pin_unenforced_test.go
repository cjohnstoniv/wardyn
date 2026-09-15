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
//
// The residual is APPENDED to whichever row the deployment was already showing,
// and it fires whether or not this caller's own sign-in has landed yet: it is a
// fact about the ROSTER, and a warning that waits for readiness arrives only
// after the first unchecked capture is already stored.
func TestBedrockProviderCheck_UnenforcedPinWarns(t *testing.T) {
	srv := New(Config{
		BedrockRegion: "us-east-1", BedrockModel: "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
		Secrets: &memSecrets{m: map[string][]byte{}},
	})
	base := srv.setupBedrock(context.Background(), map[string]bool{}, awsSSOScope{})

	for name, ready := range map[string]bool{
		"this caller has already captured": true,
		"nobody has signed in yet":         false,
	} {
		t.Run(name, func(t *testing.T) {
			bedrock := base
			bedrock.SSOPresent, bedrock.Ready = ready, ready

			chk, ok := bedrockProviderCheck(bedrock, perUserSSORow(""), true)
			if !ok {
				t.Fatal("a region+model-configured Bedrock row must always surface a check")
			}
			if chk.Status != "warn" {
				t.Errorf("status = %q, want warn — nothing constrains which account a sign-in stores", chk.Status)
			}
			if !strings.Contains(chk.Detail, bedrockUnenforcedPinDetail) {
				t.Errorf("detail = %q, want it to carry the DRAFT residual sentence", chk.Detail)
			}
			if !strings.Contains(chk.Fix, bedrockUnenforcedPinFix) {
				t.Errorf("fix = %q, want it to carry the DRAFT pin remedy", chk.Fix)
			}
			if !strings.Contains(chk.Fix, "sso_account_id") {
				t.Errorf("Fix = %q, want it to name the pin", chk.Fix)
			}
			// APPENDED, not substituted: the row this deployment was already
			// showing is still the only place the console names the live
			// region/model (ready) or what is still missing (not ready).
			plain, _ := bedrockProviderCheck(bedrock, types.SiteConfig{}, true)
			if !strings.HasPrefix(chk.Detail, plain.Detail) {
				t.Errorf("detail = %q, want it to keep %q and append the residual", chk.Detail, plain.Detail)
			}
			if ready && !strings.Contains(chk.Detail, "us-east-1") {
				t.Errorf("detail = %q, lost the configured region/model sentence", chk.Detail)
			}
			// And with the pin enforced the row is untouched.
			if want := map[bool]string{true: "ok", false: "warn"}[ready]; plain.Status != want {
				t.Errorf("a pinned deployment's row = %+v, want status %q", plain, want)
			}
		})
	}
}

// TestBedrockProviderCheck_PinDisagreeingWithTheModelWarns (V1-r2 fix-s2 review
// R-04).
//
// S2-09 made a pin that disagrees with the configured model's account LEGAL —
// it is the admin's deliberate answer, and a resource-shared inference profile
// legitimately lives elsewhere. What it must not be is INVISIBLE: with the save
// door's refusal gone, the only remaining signal was a line in the daemon
// journal, so an admin who pinned the wrong account saw a 200, a green row, and
// found out at run time as an IAM 403 — the reporting operator's own complaint,
// one level up.
func TestBedrockProviderCheck_PinDisagreeingWithTheModelWarns(t *testing.T) {
	const arnModel = "arn:aws:bedrock:us-east-1:222222222222:inference-profile/us.anthropic.claude-v1:0"
	srv := New(Config{
		BedrockRegion: "us-east-1", BedrockModel: arnModel,
		Secrets: &memSecrets{m: map[string][]byte{}},
	})
	bedrock := srv.setupBedrock(context.Background(), map[string]bool{}, awsSSOScope{})
	bedrock.SSOPresent, bedrock.Ready = true, true

	// The disagreement the save door now accepts.
	pin, modelAccount := bedrockPinDisagreement(perUserSSORow("111111111111"), arnModel)
	if pin != "111111111111" || modelAccount != "222222222222" {
		t.Fatalf("bedrockPinDisagreement = %q/%q, want 111111111111/222222222222", pin, modelAccount)
	}
	chk, ok := bedrockProviderCheck(bedrock, perUserSSORow("111111111111"), true)
	if !ok {
		t.Fatal("a region+model-configured Bedrock row must always surface a check")
	}
	if chk.Status != "warn" {
		t.Errorf("status = %q, want warn — the pinned account is not the model's", chk.Status)
	}
	for _, want := range []string{"111111111111", "222222222222"} {
		if !strings.Contains(chk.Detail, want) {
			t.Errorf("detail = %q, want it to name account %s", chk.Detail, want)
		}
	}
	if !strings.Contains(chk.Fix, "sso_account_id") || !strings.Contains(chk.Fix, "WARDYN_BEDROCK_MODEL") {
		t.Errorf("fix = %q, want both remedies named", chk.Fix)
	}
	// APPENDED, never substituted — the row still names the live region/model.
	plain, _ := bedrockProviderCheck(bedrock, types.SiteConfig{}, true)
	if !strings.HasPrefix(chk.Detail, plain.Detail) || plain.Status != "ok" {
		t.Errorf("agreeing deployment row = %+v; disagreeing detail = %q", plain, chk.Detail)
	}

	// The AGREEING pin says nothing at all.
	if a, b := bedrockPinDisagreement(perUserSSORow("222222222222"), arnModel); a != "" || b != "" {
		t.Errorf("an agreeing pin reported a disagreement (%q/%q)", a, b)
	}
	// Neither does a bare model id, which names no account to disagree with.
	if a, b := bedrockPinDisagreement(perUserSSORow("111111111111"), "us.anthropic.claude-sonnet-4-5-20250929-v1:0"); a != "" || b != "" {
		t.Errorf("a bare model id reported a disagreement (%q/%q)", a, b)
	}
}
