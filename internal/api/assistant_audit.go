// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "github.com/cjohnstoniv/wardyn/internal/types"

// assistantSettingsWriteAction is the row of a change to the organisation's assistant block, written by
// PUT /assistant/settings when the change only narrows and by the approval when it was held.
const assistantSettingsWriteAction = "assistant.settings.write"

// The audit rows of an assistant session: its start, its end, and one suggestion for each completed turn. No row
// carries prompt or output text.
const (
	assistantSessionStartAction = "assistant.session.start"
	assistantSessionEndAction   = "assistant.session.end"
	assistantSuggestionAction   = "assistant.suggestion"
)

// assistantTargetKey is the audit target of the assistant block and the target key of its held change: the
// block is a singleton.
const assistantTargetKey = "assistant"

// assistantMarkView is one provider mark in an audit row: the purposes it serves, never a credential.
type assistantMarkView struct {
	ProviderID string   `json:"provider_id"`
	Purposes   []string `json:"purposes"`
	Models     []string `json:"models"`
}

// assistantAuditView is the block as an audit row or a change's diff shows it.
type assistantAuditView struct {
	Compose          bool                `json:"compose"`
	Review           bool                `json:"review"`
	RunHistoryCounts bool                `json:"run_history_counts"`
	NeverSentKinds   []string            `json:"never_sent_kinds"`
	Providers        []assistantMarkView `json:"providers"`
}

func assistantViewOf(a types.AssistantSettings) assistantAuditView {
	a = a.Normalized()
	v := assistantAuditView{Compose: a.Compose, Review: a.Review, RunHistoryCounts: a.RunHistoryCounts,
		NeverSentKinds: a.NeverSentKinds, Providers: make([]assistantMarkView, len(a.Providers))}
	for i, m := range a.Providers {
		purposes := []string{}
		if m.Compose {
			purposes = append(purposes, string(types.AssistantCompose))
		}
		if m.Review {
			purposes = append(purposes, string(types.AssistantReview))
		}
		v.Providers[i] = assistantMarkView{ProviderID: m.ProviderID, Purposes: purposes, Models: m.Models}
	}
	return v
}

// assistantAuditData is the row's data: the block before and after.
func assistantAuditData(before, after types.AssistantSettings) map[string]any {
	return map[string]any{"before": assistantViewOf(before), "after": assistantViewOf(after)}
}
