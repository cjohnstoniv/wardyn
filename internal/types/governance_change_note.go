// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The bounds of a change's note.
const (
	MaxChangeNoteReasonLen = 500
	MaxChangeNoteTicketLen = 128
)

// ChangeNote is what a person wrote on a governance change: why it is wanted, and an optional ticket
// reference. It is free text, shown as text and never validated against a ticketing system. The
// governance_changes columns note_reason and note_ticket hold it.
type ChangeNote struct {
	Reason string `json:"reason,omitempty"`
	Ticket string `json:"ticket,omitempty"`
}

// The refusals of ValidateChangeNote. A caller maps each to its own wire reason with errors.Is.
var (
	ErrChangeNoteReasonRequired    = errors.New("reason: required")
	ErrChangeNoteReasonTooLong     = errors.New("reason: at most 500 characters")
	ErrChangeNoteReasonUnprintable = errors.New("reason: must not contain control characters")
	ErrChangeNoteTicketTooLong     = errors.New("ticket: at most 128 characters")
	ErrChangeNoteTicketNotOneLine  = errors.New("ticket: must be one line without control characters")
)

// ValidateChangeNote trims reason and ticket and checks them: the reason is at most 500 characters and
// the ticket one line of at most 128, neither with a control character. A reason may be empty unless
// reasonRequired: a note on a held write leaves it optional, and a request for elevated access requires
// it. The note it returns is what to store.
func ValidateChangeNote(reason, ticket string, reasonRequired bool) (ChangeNote, error) {
	n := ChangeNote{Reason: strings.TrimSpace(reason), Ticket: strings.TrimSpace(ticket)}
	switch {
	case n.Reason == "" && reasonRequired:
		return ChangeNote{}, ErrChangeNoteReasonRequired
	case utf8.RuneCountInString(n.Reason) > MaxChangeNoteReasonLen:
		return ChangeNote{}, ErrChangeNoteReasonTooLong
	case strings.ContainsFunc(n.Reason, unicode.IsControl):
		return ChangeNote{}, ErrChangeNoteReasonUnprintable
	case utf8.RuneCountInString(n.Ticket) > MaxChangeNoteTicketLen:
		return ChangeNote{}, ErrChangeNoteTicketTooLong
	case strings.ContainsFunc(n.Ticket, unicode.IsControl):
		return ChangeNote{}, ErrChangeNoteTicketNotOneLine
	}
	return n, nil
}
