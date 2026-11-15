// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ModelUsageTokens are token counts exactly as the provider reported them. Anthropic and Bedrock count
// input without the cache tokens; OpenAI counts cached tokens inside input. The fields are kept apart and
// never added together, so a reader who wants one figure chooses it per vendor.
type ModelUsageTokens struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
	Calls            int64 `json:"calls"`
	UncountedCalls   int64 `json:"uncounted_calls"`
	PartialCalls     int64 `json:"partial_calls"`
}

// ModelUsageModel is one model's line: the provider row the host belongs to (the host itself when none
// matches), the vendor dialect its counts are in, and why its uncounted calls were not counted.
type ModelUsageModel struct {
	Provider         string           `json:"provider"`
	Vendor           string           `json:"vendor"`
	Host             string           `json:"host"`
	Model            string           `json:"model"`
	UncountedReasons map[string]int64 `json:"uncounted_reasons"`
	ModelUsageTokens
}

// ModelUsageView is GET /api/v1/runs/{id}/model-usage. OpaqueTunnels counts tunnels, not calls: a tunnel to
// a model host that Wardyn could not open can carry many requests, none counted.
type ModelUsageView struct {
	Totals        ModelUsageTokens  `json:"totals"`
	ByModel       []ModelUsageModel `json:"by_model"`
	OpaqueTunnels int64             `json:"opaque_tunnels"`
	UpdatedAt     *time.Time        `json:"updated_at,omitempty"`
}

// handleRunModelUsage serves GET /api/v1/runs/{id}/model-usage to the run's owner or an admin, as
// /resources does: a run the caller may not read answers the same 404 a missing run does. It reads
// stored rows, so it answers for a finished run too.
func (s *Server) handleRunModelUsage(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, ok := s.getRunAuthorized(w, r, id)
	if !ok {
		return
	}
	us, ok := s.cfg.Store.(store.ModelUsageStore)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonModelUsageUnsupported, "this store does not keep model usage")
		return
	}
	sum, err := us.RunModelUsage(r.Context(), run.ID)
	if err != nil {
		writeErrorReason(w, http.StatusInternalServerError, reasonModelUsageReadFailed, "could not read model usage")
		return
	}
	tunnels, err := us.RunOpaqueTunnels(r.Context(), run.ID)
	if err != nil {
		writeErrorReason(w, http.StatusInternalServerError, reasonModelUsageReadFailed, "could not read model usage")
		return
	}
	var provider *types.ModelProvider
	if site, ok := s.siteConfigSnapshot(r.Context()); ok {
		if p, found := modelProviderByID(site.ModelProviders, run.ModelProviderID); found {
			provider = &p
		}
	}
	writeJSON(w, http.StatusOK, buildModelUsageView(sum, tunnels, provider))
}

func buildModelUsageView(sum store.ModelUsageSummary, tunnels int64, provider *types.ModelProvider) ModelUsageView {
	v := ModelUsageView{ByModel: []ModelUsageModel{}, OpaqueTunnels: tunnels, UpdatedAt: sum.UpdatedAt}
	for _, g := range sum.Groups {
		if !fitsTotals(v.Totals, g) {
			g = g.Uncounted()
		}
		t := ModelUsageTokens{
			InputTokens: g.Input, OutputTokens: g.Output, CacheReadTokens: g.CacheRead, CacheWriteTokens: g.CacheWrite,
			ReasoningTokens: g.Reasoning, Calls: g.Calls, UncountedCalls: g.UncountedCalls, PartialCalls: g.PartialCalls,
		}
		reasons := g.UncountedReasons
		if reasons == nil {
			reasons = map[string]int64{}
		}
		name := g.Host
		if provider != nil && providerServesHost(*provider, g.Host) {
			name = provider.ID
		}
		v.ByModel = append(v.ByModel, ModelUsageModel{Provider: name, Vendor: g.Vendor, Host: g.Host, Model: g.Model, UncountedReasons: reasons, ModelUsageTokens: t})
		v.Totals.InputTokens += t.InputTokens
		v.Totals.OutputTokens += t.OutputTokens
		v.Totals.CacheReadTokens += t.CacheReadTokens
		v.Totals.CacheWriteTokens += t.CacheWriteTokens
		v.Totals.ReasoningTokens += t.ReasoningTokens
		v.Totals.Calls += t.Calls
		v.Totals.UncountedCalls += t.UncountedCalls
		v.Totals.PartialCalls += t.PartialCalls
	}
	return v
}

// fitsTotals reports whether each of g's token counts can be added to tot's without passing an int64. Both are
// never negative. A group that does not fit is shown Uncounted, in its line and in the totals, so a total is
// never wrapped negative and never a cut figure.
func fitsTotals(tot ModelUsageTokens, g store.ModelUsageGroup) bool {
	return g.Input <= math.MaxInt64-tot.InputTokens && g.Output <= math.MaxInt64-tot.OutputTokens &&
		g.CacheRead <= math.MaxInt64-tot.CacheReadTokens && g.CacheWrite <= math.MaxInt64-tot.CacheWriteTokens &&
		g.Reasoning <= math.MaxInt64-tot.ReasoningTokens
}

// providerServesHost reports whether host is an address p's calls go to: its base URL, its Bedrock or
// Azure address, or the vendor's own host for a kind that has no address of its own.
func providerServesHost(p types.ModelProvider, host string) bool {
	host = strings.ToLower(host)
	addrs := []string{gatewayHost(p.BaseURL)}
	if p.Bedrock != nil {
		addrs = append(addrs, gatewayHost(p.Bedrock.BaseURL))
	}
	if p.Azure != nil {
		addrs = append(addrs, gatewayHost(p.Azure.Endpoint))
	}
	for _, a := range addrs {
		if a != "" && strings.EqualFold(a, host) {
			return true
		}
	}
	switch p.Kind {
	case types.ModelProviderAnthropicAPIKey, types.ModelProviderAnthropicSubscription:
		return p.BaseURL == "" && host == "api.anthropic.com"
	case types.ModelProviderOpenAIAPIKey:
		return p.BaseURL == "" && host == "api.openai.com"
	case types.ModelProviderBedrockSSO, types.ModelProviderBedrockBearer:
		return strings.HasPrefix(host, "bedrock-runtime.") && strings.HasSuffix(host, ".amazonaws.com")
	}
	return false
}

// ingestModelUsage handles a decision log that carries a model call's token counts (rule source
// llm:usage). It is a count and not an egress decision, so it writes no egress row and does not touch the
// idle clock. The run id is the token's, never the body's. With the opt-out on, or a store that keeps no
// usage, the record is accepted and dropped: the proxy has nothing to retry.
func (s *Server) ingestModelUsage(w http.ResponseWriter, r *http.Request, runID uuid.UUID, u *egress.ModelUsage) {
	us, ok := s.cfg.Store.(store.ModelUsageStore)
	if s.cfg.ModelUsageOff || !ok {
		writeJSON(w, http.StatusAccepted, nil)
		return
	}
	if !store.ValidModelUsage(*u) {
		writeErrorReason(w, http.StatusBadRequest, reasonInternalDecisionLogInvalid, "invalid decision log")
		return
	}
	stored, err := us.InsertModelUsage(r.Context(), runID, *u, time.Now(), "")
	if err != nil {
		writeErrorReason(w, http.StatusInternalServerError, reasonModelUsageWriteFailed, "could not store model usage")
		return
	}
	if stored {
		s.metrics.modelTokens(*u)
	}
	writeJSON(w, http.StatusAccepted, nil)
}
