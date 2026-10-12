// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// An AWS sign-in serves only the run's primary harness: its sink and renewal
// hold resolve the run's own provider alone.

// TestHarnessSignIn_OnlyThePrimaryHarness: Claude Code on an AWS sign-in beside
// a primary harness is refused at every door, naming Claude Code, before any
// credential is read; as the primary it is not.
func TestHarnessSignIn_OnlyThePrimaryHarness(t *testing.T) {
	sso := ssoProvider()
	site := types.SiteConfig{ModelProviders: providerBlock(sso)}
	want := harnessSignInNotPrimaryMsg("Claude Code", sso.ID)
	beside := twoHarnessRequest("",
		client.IncludedTool{ID: "none", Kind: client.IncludedToolHarness},
		client.IncludedTool{ID: "claude-code", Kind: client.IncludedToolHarness, ModelProvider: sso.ID})
	primary := twoHarnessRequest("",
		client.IncludedTool{ID: "claude-code", Kind: client.IncludedToolHarness, ModelProvider: sso.ID},
		client.IncludedTool{ID: "none", Kind: client.IncludedToolHarness})
	for _, mode := range []foldMode{foldCreate, foldPreflight, foldPreview} {
		srv := providerRunFixture(t, site, &capStore{}, nil)
		w, _, ok := harnessFold(srv, memberRequest(t), mode, beside)
		if ok || w.Code != http.StatusUnprocessableEntity || errorReason(w) != reasonRequestFieldUnavailable ||
			bodyTool(t, w) != "claude-code" || refusalBody(t, w) != want {
			t.Fatalf("mode %d, beside the primary: ok=%v %d %s, want 422 %s naming claude-code", mode, ok, w.Code, w.Body.String(), reasonRequestFieldUnavailable)
		}
		w, _, ok = harnessFold(srv, memberRequest(t), mode, primary)
		if errorReason(w) == reasonRequestFieldUnavailable || (mode == foldPreview && !ok) {
			t.Fatalf("mode %d, as the primary: ok=%v %d %s, want no harness-set refusal", mode, ok, w.Code, w.Body.String())
		}
	}
}

// TestDispatch_SignInOnlyOnThePrimaryLane: a provider that became an AWS
// sign-in after create is refused at dispatch on any harness but the primary,
// before its credential is read, and the run fails naming it.
func TestDispatch_SignInOnlyOnThePrimaryLane(t *testing.T) {
	sso := ssoProvider()
	sso.UID = "uid-sso"
	codex := mpKeyProvider("openai", "uid-b", types.ModelProviderOpenAIAPIKey, types.ProviderHarness{Harness: laneSecondHarness, Model: "gpt-test"})
	st := &bearerGuardStore{
		run:  types.AgentRun{ID: uuid.New(), Agent: laneSecondHarness, CreatedBy: mpOwner, ModelProviderID: codex.ID},
		site: types.SiteConfig{ModelProviders: providerBlock(codex, sso)},
	}
	h, sec := bearerGuardHarness(t, st)
	_ = sec.For(mpOwner).Put(t.Context(), providerSecretName(codex.UID, providerKeyPart), []byte(laneOwnerOpenAIKey))
	rows := harnessRows{{Harness: laneSecondHarness, ModelProviderID: codex.ID, Starts: true}, {Harness: "claude-code", ModelProviderID: sso.ID}}
	_, _, grants, ok := dispatchLanes(t, h, st, rows, &types.RunPolicySpec{}, nil)
	if ok || len(grants) != 0 {
		t.Fatalf("dispatch ok=%v grants=%d, want refused before any grant", ok, len(grants))
	}
	if got := auditDatums(h, "run.create"); !strings.Contains(got, "uses an AWS sign-in") || !strings.Contains(got, sso.ID) {
		t.Errorf("the refusal's run.create row = %s, want the sign-in sentence naming %s", got, sso.ID)
	}
}

// TestDispatch_NoProviderDetailNamesTheHarness (DS1-S2 Opus I1): when the
// primary has no provider but another harness has one, the brokered-LLM 404
// names the primary instead of saying no provider serves the run.
func TestDispatch_NoProviderDetailNamesTheHarness(t *testing.T) {
	h, st, _, b, _ := twoKeyLanes(t)
	st.run.ModelProviderID = ""
	rows := harnessRows{{Harness: "claude-code", Starts: true}, {Harness: laneSecondHarness, ModelProviderID: b.ID}}
	plan, _, _, ok := dispatchLanes(t, h, st, rows, &types.RunPolicySpec{}, nil)
	if want := "no model provider serves Claude Code on this deployment, though this run's other harnesses have their own"; !ok || !strings.HasPrefix(plan.llmUnavailableDetail, want) {
		t.Fatalf("ok=%v detail = %q, want it to start %q", ok, plan.llmUnavailableDetail, want)
	}
}

// TestHarnessProviderPairsConflict_SSOPortalHost (DS1-S2 Fable L1): an
// endpoint on an AWS sign-in's portal host conflicts with it at create, as it
// did at dispatch before the predicate was shared.
func TestHarnessProviderPairsConflict_SSOPortalHost(t *testing.T) {
	sso := ssoProvider()
	portal := types.ModelProvider{ID: "gw", Kind: types.ModelProviderCustomEndpoint, BaseURL: "https://" + ssoPortalHost(sso.Bedrock.Region, ""),
		Harnesses: []types.ProviderHarness{{Harness: "codex-cli"}}}
	if !harnessProviderPairsConflict("claude-code", sso, "codex-cli", portal) {
		t.Error("an endpoint on the sign-in's portal host was admitted beside it")
	}
}
