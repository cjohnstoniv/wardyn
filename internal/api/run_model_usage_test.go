// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const decisionsPath = "/api/v1/internal/decisions"

func usageDecisionBody(u egress.ModelUsage, bodyRunID uuid.UUID) string {
	return fmt.Sprintf(`{"request":{"run_id":%q,"host":%q,"method":"POST","path":"/v1/messages"},"decision":"allow","rule_source":"llm:usage",`+
		`"usage":{"call_id":%q,"vendor":%q,"host":%q,"model":%q,"input_tokens":%d,"output_tokens":%d,"cache_read_tokens":%d,"cache_write_tokens":%d,"reasoning_tokens":%d,"state":%q,"reason":%q}}`,
		bodyRunID, u.Host, u.CallID, u.Vendor, u.Host, u.Model, u.Input, u.Output, u.CacheRead, u.CacheWrite, u.Reasoning, u.State, u.Reason)
}

func egressRows(h *harness) (n int) {
	for _, ev := range h.audit.snapshot() {
		if strings.HasPrefix(ev.Action, "egress.") {
			n++
		}
	}
	return n
}

// One metered call is one decision row plus one usage row: the allow decision is an egress.allow audit row,
// and the llm:usage record is stored as usage and writes no second egress row.
func TestInternalDecision_MeteredCallIsOneDecisionRowAndOneUsageRow(t *testing.T) {
	srv, us, h := newUsageHarness(t)
	runID := seedUsageRunIn(us, "owner-sub", "", types.RunRunning)
	tok := h.mintRunToken(t, runID)

	allow := `{"request":{"host":"api.anthropic.com","port":443,"method":"POST","path":"/v1/messages"},"decision":"allow","rule_source":"brokered:llm"}`
	if w := do(t, srv, http.MethodPost, decisionsPath, tok, allow); w.Code != http.StatusAccepted {
		t.Fatalf("allow row: %d %s", w.Code, w.Body.String())
	}
	u := egress.ModelUsage{CallID: uuid.New(), Vendor: "anthropic", Host: "api.anthropic.com", Model: "claude-x", Input: 12, Output: 7, State: "counted"}
	// The body names another run: the row belongs to the token's run.
	if w := do(t, srv, http.MethodPost, decisionsPath, tok, usageDecisionBody(u, uuid.New())); w.Code != http.StatusAccepted {
		t.Fatalf("usage row: %d %s", w.Code, w.Body.String())
	}
	if n := egressRows(h); n != 1 {
		t.Fatalf("egress decision rows = %d, want exactly 1 for one metered call", n)
	}
	if got := us.rows[runID]; len(got) != 1 || got[0].CallID != u.CallID || got[0].Input != 12 {
		t.Fatalf("usage rows for the token's run = %+v, want the one record", got)
	}
	if len(us.rows) != 1 {
		t.Fatalf("a usage row was stored under a run id from the body: %v", us.rows)
	}
}

// A usage record is not agent activity: it does not move the idle clock.
func TestInternalDecision_UsageRecordDoesNotTouchTheIdleClock(t *testing.T) {
	srv, us, h := newUsageHarness(t)
	runID := seedUsageRunIn(us, "owner-sub", "", types.RunRunning)
	tok := h.mintRunToken(t, runID)
	u := egress.ModelUsage{CallID: uuid.New(), Vendor: "openai", Host: "api.openai.com", State: "uncounted", Reason: "no_usage"}
	if w := do(t, srv, http.MethodPost, decisionsPath, tok, usageDecisionBody(u, runID)); w.Code != http.StatusAccepted {
		t.Fatalf("usage row: %d", w.Code)
	}
	srv.lastTouchMu.Lock()
	_, touched := srv.lastTouch[runID]
	srv.lastTouchMu.Unlock()
	if touched {
		t.Fatal("a usage record touched the run's idle clock")
	}
}

// A retried POST stores the call once and counts its tokens once.
func TestInternalDecision_RetriedUsageRecordCountsOnce(t *testing.T) {
	srv, us, h := newUsageHarness(t)
	runID := seedUsageRunIn(us, "owner-sub", "", types.RunRunning)
	tok := h.mintRunToken(t, runID)
	u := egress.ModelUsage{CallID: uuid.New(), Vendor: "anthropic", Host: "api.anthropic.com", Model: "claude-x", Input: 100, Output: 50, CacheRead: 9, State: "counted"}
	for range 3 {
		if w := do(t, srv, http.MethodPost, decisionsPath, tok, usageDecisionBody(u, runID)); w.Code != http.StatusAccepted {
			t.Fatalf("usage row: %d", w.Code)
		}
	}
	if len(us.rows[runID]) != 1 {
		t.Fatalf("stored %d times", len(us.rows[runID]))
	}
	m := do(t, srv, http.MethodGet, "/metrics", adminToken, "").Body.String()
	for _, want := range []string{
		`wardyn_model_tokens_total{vendor="anthropic",direction="input"} 100`,
		`wardyn_model_tokens_total{vendor="anthropic",direction="output"} 50`,
		`wardyn_model_tokens_total{vendor="anthropic",direction="cache_read"} 9`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
}

// A vendor outside the closed set counts under other, so the series count stays fixed.
func TestInternalDecision_UnknownVendorCountsUnderOther(t *testing.T) {
	srv, us, h := newUsageHarness(t)
	runID := seedUsageRunIn(us, "owner-sub", "", types.RunRunning)
	tok := h.mintRunToken(t, runID)
	u := egress.ModelUsage{CallID: uuid.New(), Vendor: "made-up-" + uuid.NewString()[:6], Host: "x.example", Input: 4, State: "counted"}
	if w := do(t, srv, http.MethodPost, decisionsPath, tok, usageDecisionBody(u, runID)); w.Code != http.StatusAccepted {
		t.Fatalf("usage row: %d", w.Code)
	}
	if m := do(t, srv, http.MethodGet, "/metrics", adminToken, "").Body.String(); !strings.Contains(m, `wardyn_model_tokens_total{vendor="other",direction="input"} 4`) || strings.Contains(m, "made-up") {
		t.Errorf("an unknown vendor must count under other:\n%s", m)
	}
}

// A record that is not a count (a state outside the closed set, a negative number) is refused.
func TestInternalDecision_InvalidUsageRecordIsRefused(t *testing.T) {
	srv, us, h := newUsageHarness(t)
	runID := seedUsageRunIn(us, "owner-sub", "", types.RunRunning)
	tok := h.mintRunToken(t, runID)
	for _, u := range []egress.ModelUsage{
		{CallID: uuid.New(), Vendor: "anthropic", Host: "h", State: "guessed"},
		{CallID: uuid.New(), Vendor: "anthropic", Host: "h", State: "counted", Input: -1},
		{Vendor: "anthropic", Host: "h", State: "counted"},
	} {
		if w := do(t, srv, http.MethodPost, decisionsPath, tok, usageDecisionBody(u, runID)); w.Code != http.StatusBadRequest {
			t.Errorf("%+v: code %d, want 400", u, w.Code)
		}
	}
	if len(us.rows) != 0 {
		t.Fatalf("an invalid record was stored: %v", us.rows)
	}
}

// WARDYN_MODEL_USAGE=off: the server accepts the record and stores and counts nothing.
func TestInternalDecision_OptOutStoresNothing(t *testing.T) {
	srv, us, h := newUsageHarness(t, func(c *Config) { c.ModelUsageOff = true })
	runID := seedUsageRunIn(us, "owner-sub", "", types.RunRunning)
	tok := h.mintRunToken(t, runID)
	u := egress.ModelUsage{CallID: uuid.New(), Vendor: "anthropic", Host: "api.anthropic.com", Input: 5, State: "counted"}
	if w := do(t, srv, http.MethodPost, decisionsPath, tok, usageDecisionBody(u, runID)); w.Code != http.StatusAccepted {
		t.Fatalf("usage row: %d", w.Code)
	}
	if len(us.rows) != 0 || egressRows(h) != 0 {
		t.Fatalf("with the opt-out on: %d usage rows, %d egress rows", len(us.rows), egressRows(h))
	}
	if m := do(t, srv, http.MethodGet, "/metrics", adminToken, "").Body.String(); strings.Contains(m, "wardyn_model_tokens_total") {
		t.Error("tokens were counted with the opt-out on")
	}
}

// The view sums by model, names the provider row whose address the host is, falls back to the host, keeps
// the five counts and the uncounted reasons apart, and reports the opaque tunnels.
func TestRunModelUsage_AggregatesByModelAndMapsProvider(t *testing.T) {
	srv, us, _ := newUsageHarness(t)
	us.tunnels = 2
	us.authzStore.siteCfg = types.SiteConfig{ModelProviders: &types.ModelProviders{Providers: []types.ModelProvider{
		{ID: "corp-gw", Kind: types.ModelProviderCustomEndpoint, BaseURL: "https://llm.corp.example/v1"},
	}}}
	id := seedUsageRun(us, "owner-sub", "corp-gw")
	us.rows[id] = append(us.rows[id], []egress.ModelUsage{
		{CallID: uuid.New(), Vendor: "anthropic", Host: "llm.corp.example", Model: "claude-x", Input: 10, Output: 5, CacheRead: 3, CacheWrite: 2, State: "counted"},
		{CallID: uuid.New(), Vendor: "anthropic", Host: "llm.corp.example", Model: "claude-x", Input: 1, Output: 1, State: "partial", Reason: "stream_cut"},
		{CallID: uuid.New(), Vendor: "openai", Host: "api.openai.com", Model: "gpt-x", Input: 20, Reasoning: 4, State: "counted"},
		{CallID: uuid.New(), Vendor: "openai", Host: "api.openai.com", Model: "gpt-x", State: "uncounted", Reason: "stream_without_usage"},
	}...)
	owner := ssoSession(t, "owner-sub", "owner@corp.example", oidc.RoleUser)
	w := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+id.String()+"/model-usage", owner, "")
	if w.Code != http.StatusOK {
		t.Fatalf("code %d: %s", w.Code, w.Body.String())
	}
	var v ModelUsageView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.OpaqueTunnels != 2 || v.UpdatedAt == nil {
		t.Errorf("tunnels %d, updated_at %v", v.OpaqueTunnels, v.UpdatedAt)
	}
	if v.Totals.InputTokens != 31 || v.Totals.OutputTokens != 6 || v.Totals.CacheReadTokens != 3 || v.Totals.CacheWriteTokens != 2 ||
		v.Totals.ReasoningTokens != 4 || v.Totals.Calls != 4 || v.Totals.UncountedCalls != 1 || v.Totals.PartialCalls != 1 {
		t.Errorf("totals: %+v", v.Totals)
	}
	if len(v.ByModel) != 2 {
		t.Fatalf("by_model: %+v", v.ByModel)
	}
	claude, gpt := v.ByModel[0], v.ByModel[1]
	if claude.Provider != "corp-gw" || claude.Model != "claude-x" || claude.Calls != 2 || claude.InputTokens != 11 {
		t.Errorf("claude: %+v", claude)
	}
	if gpt.Provider != "api.openai.com" || gpt.UncountedReasons["stream_without_usage"] != 1 || claude.UncountedReasons == nil {
		t.Errorf("gpt: %+v", gpt)
	}
}

// A run that has no usage answers an empty list (not null) and zero totals.
func TestRunModelUsage_EmptyRunIsEmptyNotNull(t *testing.T) {
	srv, us, _ := newUsageHarness(t)
	id := seedUsageRun(us, "owner-sub", "")
	owner := ssoSession(t, "owner-sub", "owner@corp.example", oidc.RoleUser)
	w := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+id.String()+"/model-usage", owner, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"by_model":[]`) || !strings.Contains(w.Body.String(), `"opaque_tunnels":0`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

// providerServesHost: a key kind with no address of its own serves the vendor's host.
func TestProviderServesHost(t *testing.T) {
	for _, tc := range []struct {
		p    types.ModelProvider
		host string
		want bool
	}{
		{types.ModelProvider{Kind: types.ModelProviderAnthropicAPIKey}, "api.anthropic.com", true},
		{types.ModelProvider{Kind: types.ModelProviderAnthropicAPIKey, BaseURL: "https://gw.example"}, "api.anthropic.com", false},
		{types.ModelProvider{Kind: types.ModelProviderAnthropicAPIKey, BaseURL: "https://gw.example"}, "GW.example", true},
		{types.ModelProvider{Kind: types.ModelProviderOpenAIAPIKey}, "api.openai.com", true},
		{types.ModelProvider{Kind: types.ModelProviderBedrockBearer, Bedrock: &types.BedrockSettings{Region: "us-east-1"}}, "bedrock-runtime.us-east-1.amazonaws.com", true},
		{types.ModelProvider{Kind: types.ModelProviderBedrockSSO, Bedrock: &types.BedrockSettings{BaseURL: "https://bedrock.corp.example"}}, "bedrock.corp.example", true},
		{types.ModelProvider{Kind: types.ModelProviderAzureFoundry, Azure: &types.AzureSettings{Endpoint: "https://r.services.ai.azure.com"}}, "r.services.ai.azure.com", true},
		{types.ModelProvider{Kind: types.ModelProviderOpenAIAPIKey}, "api.anthropic.com", false},
	} {
		if got := providerServesHost(tc.p, tc.host); got != tc.want {
			t.Errorf("%s %q: %v, want %v", tc.p.Kind, tc.host, got, tc.want)
		}
	}
}
