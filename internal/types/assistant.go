// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"cmp"
	"fmt"
	"slices"
)

// AssistantHarness is the one harness that serves the assistant. A provider serves it only when it is
// paired with this harness.
const AssistantHarness = "claude-code"

// AssistantPurpose is what a person asks the assistant for: drafting a change, or explaining one that
// waits for approval.
type AssistantPurpose string

const (
	AssistantCompose AssistantPurpose = "compose"
	AssistantReview  AssistantPurpose = "review"
)

// Valid reports whether p is one of the two purposes.
func (p AssistantPurpose) Valid() bool { return p == AssistantCompose || p == AssistantReview }

// The availability states of the assistant for one person and one purpose.
const (
	AssistantReady        = "ready"
	AssistantOff          = "off"
	AssistantNoProvider   = "no_provider"
	AssistantUnitDenies   = "unit_denies"
	AssistantNotConnected = "not_connected"
	AssistantAtRunLimit   = "at_run_limit"
)

// The wire reasons Validate refuses with. The API declares the same values with the rest of its reasons;
// a test keeps the two in step.
const (
	AssistantReasonCannotServe      = "assistant_provider_cannot_serve"
	AssistantReasonDefersInspection = "assistant_provider_defers_inspection"
	AssistantReasonSettingsInvalid  = "assistant_settings_invalid"
)

// PROPOSED sentences.
const (
	assistantCannotServeMsg = "This provider can't serve the assistant yet."
	assistantBedrockMsg     = "Bedrock providers can't serve the assistant, because Wardyn can't inspect their prompts."
	assistantDefersMsg      = "This provider leaves prompt inspection to its gateway, so it can't serve the assistant."
)

// AssistantProviderMark says which purposes one configured model provider may serve, and with which
// models. No models means the provider's own harness model.
type AssistantProviderMark struct {
	ProviderID string   `json:"provider_id"`
	Compose    bool     `json:"compose"`
	Review     bool     `json:"review"`
	Models     []string `json:"models"`
}

// Serves reports whether the mark lets its provider serve purpose.
func (m AssistantProviderMark) Serves(purpose AssistantPurpose) bool {
	if purpose == AssistantCompose {
		return m.Compose
	}
	return purpose == AssistantReview && m.Review
}

// AssistantSettings is the organisation's assistant block in the site config. The zero value, and a
// missing block, is every purpose off.
type AssistantSettings struct {
	Compose bool `json:"compose"`
	Review  bool `json:"review"`
	// RunHistoryCounts lets the assistant's first message carry run history as host counts.
	RunHistoryCounts bool `json:"run_history_counts"`
	// NeverSentKinds are record kinds that are never sent to a model provider.
	NeverSentKinds []string                `json:"never_sent_kinds"`
	Providers      []AssistantProviderMark `json:"providers"`
}

// On reports whether purpose is turned on.
func (a AssistantSettings) On(purpose AssistantPurpose) bool {
	if purpose == AssistantCompose {
		return a.Compose
	}
	return purpose == AssistantReview && a.Review
}

// IsZero reports whether a says nothing: every switch off and both lists empty.
func (a AssistantSettings) IsZero() bool {
	return !a.Compose && !a.Review && !a.RunHistoryCounts && len(a.NeverSentKinds) == 0 && len(a.Providers) == 0
}

// Normalized is a with its lists sorted by kind and provider id and never nil, so equal settings hash
// and compare equal. A mark's models keep their order: the first one the person's limits allow is used.
func (a AssistantSettings) Normalized() AssistantSettings {
	a.NeverSentKinds = slices.Sorted(slices.Values(a.NeverSentKinds))
	if a.NeverSentKinds == nil {
		a.NeverSentKinds = []string{}
	}
	marks := make([]AssistantProviderMark, len(a.Providers))
	for i, m := range a.Providers {
		m.Models = slices.Clone(m.Models)
		if m.Models == nil {
			m.Models = []string{}
		}
		marks[i] = m
	}
	slices.SortFunc(marks, func(x, y AssistantProviderMark) int { return cmp.Compare(x.ProviderID, y.ProviderID) })
	a.Providers = marks
	return a
}

// AssistantSettingsError is a refusal of the assistant block, with the wire reason it is answered under.
type AssistantSettingsError struct {
	Reason string
	Msg    string
}

func (e *AssistantSettingsError) Error() string { return e.Msg }

func invalidAssistant(format string, args ...any) *AssistantSettingsError {
	return &AssistantSettingsError{Reason: AssistantReasonSettingsInvalid, Msg: fmt.Sprintf(format, args...)}
}

// Validate refuses a block that names a provider that does not exist or is named twice; then, mark by
// mark, a provider that cannot serve the assistant (it does not pair with the claude-code harness, it is
// a Bedrock kind, or defers reports it leaves prompt inspection to its gateway); a model list that
// ValidAllowedModels refuses; and last a never-sent kind that knownKind does not know, or a repeated one.
// The error is an *AssistantSettingsError.
func (a AssistantSettings) Validate(providers []ModelProvider, knownKind func(string) bool, defers func(ModelProvider) bool) error {
	byID := make(map[string]ModelProvider, len(providers))
	for _, p := range providers {
		byID[p.ID] = p
	}
	seen := map[string]bool{}
	for _, m := range a.Providers {
		if _, ok := byID[m.ProviderID]; !ok {
			return invalidAssistant("provider %q is not a configured model provider", m.ProviderID) // PROPOSED
		}
		if seen[m.ProviderID] {
			return invalidAssistant("provider %q is marked twice", m.ProviderID) // PROPOSED
		}
		seen[m.ProviderID] = true
	}
	for _, m := range a.Providers {
		p := byID[m.ProviderID]
		switch {
		case !p.Serves(AssistantHarness):
			return &AssistantSettingsError{Reason: AssistantReasonCannotServe, Msg: assistantCannotServeMsg}
		case p.Kind.IsBedrock():
			return &AssistantSettingsError{Reason: AssistantReasonCannotServe, Msg: assistantBedrockMsg}
		case defers(p):
			return &AssistantSettingsError{Reason: AssistantReasonDefersInspection, Msg: assistantDefersMsg}
		}
		if err := ValidAllowedModels(m.Models); err != nil {
			return invalidAssistant("models for provider %q: %s", m.ProviderID, err) // PROPOSED
		}
	}
	kinds := map[string]bool{}
	for _, k := range a.NeverSentKinds {
		switch {
		case !knownKind(k):
			return invalidAssistant("%q is not a record kind the assistant can be kept from", k) // PROPOSED
		case kinds[k]:
			return invalidAssistant("record kind %q is listed twice", k) // PROPOSED
		}
		kinds[k] = true
	}
	return nil
}
