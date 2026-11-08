// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"errors"
	"strings"
	"testing"
)

func TestChangeNoteBoundsAndRequired(t *testing.T) {
	cases := []struct {
		name           string
		reason, ticket string
		required       bool
		want           error
		wantNote       ChangeNote
	}{
		{name: "empty optional", wantNote: ChangeNote{}},
		{name: "empty required", required: true, want: ErrChangeNoteReasonRequired},
		{name: "blank required", reason: " \t ", required: true, want: ErrChangeNoteReasonRequired},
		{name: "trimmed", reason: "  why  ", ticket: " INC-1 ", wantNote: ChangeNote{Reason: "why", Ticket: "INC-1"}},
		{name: "reason at bound", reason: strings.Repeat("é", 500), wantNote: ChangeNote{Reason: strings.Repeat("é", 500)}},
		{name: "reason over bound", reason: strings.Repeat("é", 501), want: ErrChangeNoteReasonTooLong},
		{name: "reason control", reason: "a\x07b", want: ErrChangeNoteReasonUnprintable},
		{name: "reason newline", reason: "a\nb", want: ErrChangeNoteReasonUnprintable},
		{name: "ticket at bound", ticket: strings.Repeat("t", 128), wantNote: ChangeNote{Ticket: strings.Repeat("t", 128)}},
		{name: "ticket over bound", ticket: strings.Repeat("t", 129), want: ErrChangeNoteTicketTooLong},
		{name: "ticket two lines", ticket: "a\nb", want: ErrChangeNoteTicketNotOneLine},
		{name: "ticket NUL", ticket: "a\x00", want: ErrChangeNoteTicketNotOneLine},
		{name: "required and valid", reason: "r", required: true, wantNote: ChangeNote{Reason: "r"}},
	}
	for _, c := range cases {
		got, err := ValidateChangeNote(c.reason, c.ticket, c.required)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
			continue
		}
		if c.want == nil && got != c.wantNote {
			t.Errorf("%s: note = %+v, want %+v", c.name, got, c.wantNote)
		}
	}
}
