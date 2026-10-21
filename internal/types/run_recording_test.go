// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "testing"

// TestRecordingChoiceResolve is the resolution table. Its first row is the golden: nothing set
// anywhere records terminal sessions, exactly what every run did before the level existed.
func TestRecordingChoiceResolve(t *testing.T) {
	org := func(l RecordingLevel, locked bool) *SiteRecording { return &SiteRecording{Level: l, Locked: locked} }
	for _, tc := range []struct {
		name string
		c    RecordingChoice
		want RecordingLevel
	}{
		{"nothing set", RecordingChoice{}, RecordingTerminal},
		{"nothing set, member", RecordingChoice{MemberAuthored: true}, RecordingTerminal},
		{"empty org document", RecordingChoice{Org: &SiteRecording{}}, RecordingTerminal},
		{"policy off, operator", RecordingChoice{Policy: RecordingOff}, RecordingOff},
		{"policy screen", RecordingChoice{Policy: RecordingScreenActions}, RecordingScreenActions},
		{"org default screen", RecordingChoice{Org: org(RecordingScreenActions, false)}, RecordingScreenActions},
		{"unlocked org, policy lower", RecordingChoice{Policy: RecordingOff, Org: org(RecordingScreenActions, false)}, RecordingOff},
		{"locked org, policy lower", RecordingChoice{Policy: RecordingOff, Org: org(RecordingTerminal, true)}, RecordingTerminal},
		{"locked org, ceiling lower", RecordingChoice{Policy: RecordingOff, MemberAuthored: true, Ceiling: RecordingOff, Org: org(RecordingScreenActions, true)}, RecordingScreenActions},
		{"locked org, policy higher", RecordingChoice{Policy: RecordingScreenActions, Org: org(RecordingTerminal, true)}, RecordingScreenActions},
		{"locked org off", RecordingChoice{Org: org(RecordingOff, true)}, RecordingOff},
		{"member inline, ceiling silent, lower", RecordingChoice{Policy: RecordingOff, MemberAuthored: true}, RecordingTerminal},
		{"member picks a stored off policy", RecordingChoice{Policy: RecordingOff}, RecordingOff},
		{"member inline, ceiling chose off", RecordingChoice{Policy: RecordingOff, MemberAuthored: true, Ceiling: RecordingOff}, RecordingOff},
		{"required raises off", RecordingChoice{Policy: RecordingOff, Required: true}, RecordingTerminal},
		{"required keeps screen", RecordingChoice{Policy: RecordingScreenActions, Required: true}, RecordingScreenActions},
		{"required over org off", RecordingChoice{Org: org(RecordingOff, false), Required: true}, RecordingTerminal},
	} {
		if got := tc.c.Resolve(); got != tc.want {
			t.Errorf("%s: Resolve() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestRecordingLevelsNameNoKeystrokes: no level, valid or ranked, records keystrokes.
func TestRecordingLevelsNameNoKeystrokes(t *testing.T) {
	for _, l := range []RecordingLevel{"keystrokes", "keys", "screen_actions_keystrokes", "all"} {
		if l.Valid() || l.Rank() != 0 {
			t.Errorf("%q is accepted as a recording level", l)
		}
	}
}
