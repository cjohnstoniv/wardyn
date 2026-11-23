// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "testing"

func TestAssistantTargetValidFor(t *testing.T) {
	cases := []struct {
		kind    string
		purpose AssistantPurpose
		want    bool
	}{
		{AssistantTargetRun, AssistantCompose, true},
		{AssistantTargetUnit, AssistantCompose, true},
		{AssistantTargetTemplate, AssistantCompose, true},
		{AssistantTargetChange, AssistantCompose, false},
		{AssistantTargetChange, AssistantReview, true},
		{AssistantTargetRun, AssistantReview, false},
		{"", AssistantCompose, false},
		{AssistantTargetRun, "other", false},
	}
	for _, c := range cases {
		if got := (AssistantTarget{Kind: c.kind}).ValidFor(c.purpose); got != c.want {
			t.Errorf("%q with %q = %v, want %v", c.kind, c.purpose, got, c.want)
		}
	}
}

func TestAssistantRunEventKind(t *testing.T) {
	for _, k := range []string{AssistantEventState, AssistantEventText, AssistantEventToolCall, AssistantEventToolResult,
		AssistantEventNotice, AssistantEventTurnEnd, AssistantEventError} {
		if !AssistantRunEventKind(k) {
			t.Errorf("the run may not post %q", k)
		}
	}
	for _, k := range []string{AssistantEventUserMessage, AssistantEventPersonEdit, AssistantEventProposal,
		AssistantEventToolRecord, AssistantEventBuildFiles, AssistantEventSuggestion, "other", ""} {
		if AssistantRunEventKind(k) {
			t.Errorf("the run may post the server-only kind %q", k)
		}
	}
}

func TestAssistantSessionLive(t *testing.T) {
	if !(AssistantSession{State: AssistantSessionStarting}).Live() || (AssistantSession{State: AssistantSessionEnded}).Live() {
		t.Error("Live misreads a state")
	}
}
