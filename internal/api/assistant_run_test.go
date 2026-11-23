// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The written policy sets every inspection field, the validator accepts it, and it carries nothing a person wrote.
func TestAssistantRunPolicy(t *testing.T) {
	spec := assistantRunPolicy(types.CC2)
	li := spec.LLMInspection
	if li == nil {
		t.Fatal("no inspection block")
	}
	want := types.LLMInspectionSpec{Mode: "block", DetectSecrets: true, DetectSecretPatterns: true, OnScannerError: "block",
		RequireInspectableLLM: true, InterceptTLS: true, BlockMinSeverity: "low", MaxScanBytes: 1048576}
	gotJSON, _ := json.Marshal(li)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("LLMInspection = %s, want %s", gotJSON, wantJSON)
	}
	if err := validateLLMInspection(spec); err != nil {
		t.Errorf("validateLLMInspection = %v, want nil", err)
	}
	if spec.MinConfinementClass != types.CC2 || spec.AutoStopAfterSec != 7200 || len(spec.AllowedDomains) != 0 || spec.AllowedDomains == nil {
		t.Errorf("policy = %+v, want CC2, 7200 s and an empty allow-list", spec)
	}
	if len(spec.WorkspaceMounts) != 0 || len(spec.EligibleGrants) != 0 || len(spec.WorkspaceRepos) != 0 {
		t.Errorf("policy carries mounts, repositories or grants: %+v", spec)
	}
}

func TestAssistantRunPayloadOK(t *testing.T) {
	ev := func(kind, payload string) assistantRunEvent {
		return assistantRunEvent{Kind: kind, Payload: json.RawMessage(payload)}
	}
	cases := []struct {
		name string
		ev   assistantRunEvent
		want bool
	}{
		{"no payload", ev(types.AssistantEventText, ""), true},
		{"object", ev(types.AssistantEventText, `{"text":"hi"}`), true},
		{"not an object", ev(types.AssistantEventText, `"hi"`), false},
		{"a state naming a turn", ev(types.AssistantEventState, `{"turn":"reset"}`), false},
		{"a state of its own", ev(types.AssistantEventState, `{"state":"ready"}`), true},
	}
	for _, c := range cases {
		if got := assistantRunPayloadOK(c.ev); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAssistantHostCountsOf(t *testing.T) {
	r1, r2 := uuid.New(), uuid.New()
	rows := []store.RunHostDecision{
		{RunID: r1, Host: "a.example"}, {RunID: r1, Host: "a.example"}, {RunID: r2, Host: "a.example"}, {RunID: r2, Host: "b.example"},
	}
	got := assistantHostCountsOf(rows)
	if len(got) != 2 || got[0].Host != "a.example" || got[0].Count != 2 || got[1].Host != "b.example" || got[1].Count != 1 {
		t.Errorf("counts = %+v, want a.example 2 then b.example 1: runs, not rows", got)
	}
}

func TestAssistantTargetPairing(t *testing.T) {
	if assistantStaticKind(types.AssistantTarget{Kind: types.AssistantTargetUnit}) != govKindUnit ||
		assistantStaticKind(types.AssistantTarget{Kind: types.AssistantTargetChange}) != "" {
		t.Error("assistantStaticKind reads the wrong kinds")
	}
	if got := assistantToolNames(types.AssistantReview); len(got) != 8 || got[len(got)-1] == "draft" {
		t.Errorf("review tools = %v, want eight with no draft", got)
	}
	if got := assistantToolNames(types.AssistantCompose); len(got) != 10 {
		t.Errorf("compose tools = %v, want ten", got)
	}
	set := &types.AssistantSettings{NeverSentKinds: []string{govKindUnit}}
	if assistantNeverSent(set, govKindUnit) == nil || assistantNeverSent(set, "other") != nil || assistantNeverSent(nil, govKindUnit) != nil ||
		assistantNeverSent(set, "") != nil {
		t.Error("assistantNeverSent refuses the wrong kinds")
	}
}
