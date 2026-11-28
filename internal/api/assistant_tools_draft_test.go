// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/assistant"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A host the person's ceiling does not allow is dropped from the proposal and said why, with the unit whose limits
// refused it; a host it allows stays. An operator's ceiling drops nothing.
func TestAssistantDraftDropsACeilingRefusedHost(t *testing.T) {
	ceiling := governanceCeiling{Spec: types.RunPolicySpec{AllowedDomains: []string{"api.example"}},
		Unit: &ResolvedUnit{ID: uuid.New(), Name: "eng"}}
	draft := types.RunPolicySpec{AllowedDomains: []string{"api.example", "evil.example"}}
	got, refusals := assistantClampSpec(draft, ceiling)
	if !slices.Equal(got.AllowedDomains, []string{"api.example"}) {
		t.Errorf("proposal hosts = %v, want only the allowed one", got.AllowedDomains)
	}
	want := assistant.Refusal{Host: "evil.example", Code: assistant.DropOutsideCeiling, Unit: "eng"}
	if !slices.Contains(refusals, want) {
		t.Errorf("refusals = %+v, want %+v", refusals, want)
	}
	if d := assistant.DroppedOf(refusals, func(string) int { return 2 }); len(d) == 0 || d[0].AdminChangeApprovals != 2 {
		t.Errorf("dropped = %+v, want the host with the unit's approval count", d)
	}
	ceiling.Operator = true
	if got, refusals := assistantClampSpec(draft, ceiling); len(refusals) != 0 || len(got.AllowedDomains) != 2 {
		t.Errorf("operator ceiling: %v, %+v; want nothing dropped", got.AllowedDomains, refusals)
	}
}

// Each part names a field of the target's save body; one that names none is refused as an unknown section, and
// the rest are laid on the draft.
func TestAssistantDraftRefusesAnUnknownSection(t *testing.T) {
	sub := assistantSubject{targetKind: types.AssistantTargetRun}
	draft, refused := (&Server{}).assistantAssembleDraft(sub, []types.AssistantProposalPart{
		{Section: "allowed_domains", Value: []byte(`["api.example"]`)},
		{Section: "no_such_field", Value: []byte(`1`)},
	})
	if len(refused) != 1 || refused[0].Field != "no_such_field" || refused[0].Code != assistant.DropUnknownSection {
		t.Errorf("refused = %+v, want the unknown section only", refused)
	}
	if string(draft) != `{"allowed_domains":["api.example"]}` {
		t.Errorf("draft = %s", draft)
	}
}
