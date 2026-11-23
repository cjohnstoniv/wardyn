// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package assistant

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func labels(p Payload) []string {
	out := make([]string, len(p.Sections))
	for i, s := range p.Sections {
		out[i] = s.Label
	}
	return out
}

func TestFirstPayloadOrderAndUntrusted(t *testing.T) {
	admin := Person{Email: "ada@corp.example", Name: "Ada"}
	proposer := Person{Email: "pat@corp.example"}
	p := FirstPayload(Inputs{
		Purpose: types.AssistantReview, UnitRecord: "unit ada@corp.example", Templates: []string{"t1"},
		UnitAdmins: []Person{admin}, ChangeProposer: &proposer, Grants: []string{"github api.github.com"},
		RunHistoryOn: true, RunHistory: []HostCount{{"a.example", "allow", 3}},
		Change: "note from pat@corp.example", BuildFiles: []BuildFile{{"go.mod", "module x"}},
	})
	want := []string{LabelUnit, LabelTemplates, LabelAdmins, LabelGrants, LabelHistory, LabelChange, LabelBuild}
	if got := labels(p); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("labels = %v, want %v", got, want)
	}
	// Every record holding text another party can write is untrusted; the admins line is letters alone.
	untrusted := map[string]bool{LabelUnit: true, LabelRun: true, LabelTemplates: true, LabelAdmins: false,
		LabelGrants: true, LabelHistory: true, LabelChange: true, LabelBuild: true}
	for _, s := range p.Sections {
		wantUntrusted, known := untrusted[s.Label]
		if !known || s.Untrusted != wantUntrusted {
			t.Errorf("%s untrusted = %v, want %v (a label this test does not know: %v)", s.Label, s.Untrusted, wantUntrusted, !known)
		}
		if strings.Contains(s.Text, "@corp.example") {
			t.Errorf("%s still names a person: %q", s.Label, s.Text)
		}
	}
	if p.Letters["Person A"] != "ada@corp.example" || p.Letters["Person B"] != "pat@corp.example" {
		t.Errorf("letters = %v", p.Letters)
	}
}

func TestFirstPayloadLeavesOutWhatIsEmptyOrOff(t *testing.T) {
	p := FirstPayload(Inputs{Purpose: types.AssistantCompose, UnitRecord: "u", RunHistory: []HostCount{{"h", "allow", 1}}, Change: "ignored"})
	if got := labels(p); len(got) != 1 || got[0] != LabelUnit {
		t.Errorf("labels = %v, want only the unit record: history is off and a compose session has no change", got)
	}
}

func TestFirstPayloadCaps(t *testing.T) {
	big := strings.Repeat("é", MaxSectionBytes) // 2 bytes a rune: forces a rune-boundary cut
	p := FirstPayload(Inputs{Purpose: types.AssistantCompose, UnitRecord: big, Templates: []string{big}, Grants: []string{big},
		BuildFiles: []BuildFile{{"f", big}}})
	total := 0
	for _, s := range p.Sections {
		if len(s.Text) > MaxSectionBytes {
			t.Errorf("%s is %d bytes, over the section cap", s.Label, len(s.Text))
		}
		if !strings.HasSuffix(s.Text, truncatedMark) && len(s.Text) < len(big) {
			t.Errorf("%s was cut without the mark", s.Label)
		}
		total += len(s.Text)
	}
	if total > MaxPayloadBytes {
		t.Errorf("payload is %d bytes, over the cap %d", total, MaxPayloadBytes)
	}
	if len(p.Sections) < 3 {
		t.Errorf("got %d sections", len(p.Sections))
	}
	for _, s := range p.Sections {
		if strings.ToValidUTF8(s.Text, "") != s.Text {
			t.Errorf("%s was cut inside a rune", s.Label)
		}
	}
}

// A run target's policy goes under its own label, never the unit's, and is untrusted like every stored record; a change's
// assignee and any address in it become letters like its proposer.
func TestFirstPayloadRunRecordAndChangePeople(t *testing.T) {
	p := FirstPayload(Inputs{Purpose: types.AssistantCompose, RunRecord: `{"allowed_domains":["a.example"]}`})
	if got := labels(p); len(got) != 1 || got[0] != LabelRun || !p.Sections[0].Untrusted {
		t.Errorf("sections = %+v, want one untrusted run record", p.Sections)
	}
	sam := Person{Name: "sub-sam"}
	p = FirstPayload(Inputs{Purpose: types.AssistantReview, ChangeProposer: &Person{Email: "pat@corp.example"},
		ChangePeople: []Person{sam, {Email: "kim@corp.example"}}, Change: `{"subject":"sub-sam","note":"ask kim@corp.example"}`})
	text := p.Sections[len(p.Sections)-1].Text
	if strings.Contains(text, "sub-sam") || strings.Contains(text, "kim@corp.example") ||
		!strings.Contains(text, "Person B") || !strings.Contains(text, "Person C") {
		t.Errorf("change record = %q, want the assignee and the address as letters", text)
	}
}

// Each history line names its host, its decision and its count, and a refused host reads as refused.
func TestHistoryLinesCarryTheDecision(t *testing.T) {
	p := FirstPayload(Inputs{Purpose: types.AssistantCompose, RunHistoryOn: true,
		RunHistory: []HostCount{{"a.example", "allow", 412}, {"b.example", "deny", 9}}})
	if got, want := p.Sections[0].Text, "a.example · allowed · 412\nb.example · refused · 9"; got != want {
		t.Errorf("history = %q, want %q", got, want)
	}
}
