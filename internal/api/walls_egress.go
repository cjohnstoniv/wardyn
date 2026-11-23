// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The run item's door rule on the Walls view: how the run's proxy treats a host no rule names.
const (
	wallsEgressSealed    = "sealed"    // the built-in No network preset: the proxy refuses every request
	wallsEgressOpen      = "open"      // allow_all_egress: any public host not denied
	wallsEgressAllowlist = "allowlist" // only the allowed_domains entries
)

// wallsRunEgress is the run's door rule: "sealed", "open" or "allowlist", or "" when the enforced policy cannot be read.
//
// It is runEffectivePolicy's rule over rows already read: No network first, with no row needed; then the
// dispatch envelope (latestPolicyResolve); then, once a revive made generation 2 or later current, the newest
// row of that generation that carries a policy. With no envelope, or no readable row for the current
// generation, the rule is not guessed.
func wallsRunEgress(run types.AgentRun, env store.RunPolicyEnvelope) string {
	if noNetworkPreset(run.WallPreset) {
		return wallsEgressSealed
	}
	envelope, _, ok := latestPolicyResolve(env.Resolve)
	if !ok {
		return ""
	}
	allowAll := envelope.AllowAllEgress
	if env.State.CurrentGeneration() >= 2 {
		policy := wallsEgressGenerationPolicy(env.Generation)
		if policy == nil {
			return ""
		}
		allowAll = policy.AllowAllEgress
	}
	if allowAll {
		return wallsEgressOpen
	}
	return wallsEgressAllowlist
}

// wallsEgressGenerationPolicy is the policy of the newest row among rows (oldest first) whose data decodes
// {"policy": {...}} with a policy, or nil when none does.
func wallsEgressGenerationPolicy(rows []types.AuditEvent) *types.RunPolicySpec {
	for _, ev := range slices.Backward(rows) {
		var g struct {
			Policy *types.RunPolicySpec `json:"policy"`
		}
		if json.Unmarshal(ev.Data, &g) == nil && g.Policy != nil {
			return g.Policy
		}
	}
	return nil
}

// wallsEgressOf reads one page's envelopes in one call. A No network run is sealed without one. A store without
// the reader, or a failed read, leaves every other run out, so its item omits egress; it never fails the request.
func (s *Server) wallsEgressOf(ctx context.Context, runs []types.AgentRun) map[uuid.UUID]string {
	out := map[uuid.UUID]string{}
	ids := make([]uuid.UUID, 0, len(runs))
	seen := make(map[uuid.UUID]bool, len(runs))
	for _, run := range runs {
		if noNetworkPreset(run.WallPreset) {
			out[run.ID] = wallsEgressSealed
		} else if !seen[run.ID] {
			seen[run.ID] = true
			ids = append(ids, run.ID)
		}
	}
	reader, ok := s.cfg.Store.(store.RunPolicyEnvelopeReader)
	if !ok || len(ids) == 0 {
		return out
	}
	envs, err := reader.RunPolicyEnvelopes(ctx, ids, spellingsOf("run.policy.resolve"))
	if err != nil {
		slog.WarnContext(ctx, "wardyn: the Walls view could not read the runs' policy envelopes; no run item carries egress",
			slog.Any("err", err))
		return out
	}
	for _, run := range runs {
		env, found := envs[run.ID]
		if !found || out[run.ID] != "" {
			continue
		}
		if door := wallsRunEgress(run, env); door != "" {
			out[run.ID] = door
		}
	}
	return out
}
