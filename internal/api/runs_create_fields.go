// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Length ceilings for the create-run request's free-text fields. Every one of
// them is a trust boundary: the values land in TEXT columns, in every run row
// the console renders, in the hash-chained audit row, and — for repo — in the
// clone the sandbox performs. Generous enough that no real value is refused.
//
// title/description were already capped; repo, task and agent were bounded only
// by the 1 MiB body, so a 1 MiB "repo" reached the run row, every list payload
// and the audit trail (B1-F6). localPrincipalOverride's doc comment
// (runs_policy.go) claims every other caller-supplied string on this path is
// capped and control-character-checked — these were the exceptions it did not
// know about.
const (
	maxRunRepoLen  = 512
	maxRunTaskLen  = 32000
	maxRunAgentLen = 128
)

// DRAFT (M2 canon pending)
const (
	// runFieldControlCharRefusal is the 400 for a control character in a
	// create-run text field. NUL is the one that mattered: Postgres rejects it
	// outright, so a NUL in a title became a 500 from the driver instead of a
	// 400 naming the field the caller can fix.
	runFieldControlCharRefusal = "%s contains a control character, which is not allowed"
	// runFieldTooLongRefusal is the EXISTING title/description sentence,
	// spelled once so the three fields that never had a cap answer in the
	// caller's own words rather than inventing a second shape.
	runFieldTooLongRefusal = "%s is too long (%d chars, max %d)"
)

// runTextField is one create-run free-text field and the two things a trust
// boundary has to know about it: how long it may be, and whether it is prose
// (newlines and tabs are content) or a single-line identifier (they are not).
type runTextField struct {
	name  string
	value string
	max   int
	// multiline: \n and \t are legitimate content here. A task is what the human
	// typed for the agent and a description is a note — both are pasted from
	// editors and both round-trip through a TEXT column unharmed. Every other
	// field is a single-line identifier where a newline is either a paste
	// accident or an attempt to forge a second line in something that renders
	// the value.
	multiline bool
}

// validateRunTextFields is the ONE loop over every caller-supplied free-text
// field on a create-run request: a rune cap and a control-character check,
// answering the 400 shape title/description already answered. It writes its own
// error and returns false once it has responded.
//
// A METHOD ON Server, although it reads nothing from the receiver: preflight
// parity is enforced structurally over `s.<Gate>(…)` calls
// (TestPreflightMirrorsLaunchGates), so a package-level function would be a gate
// that guard cannot see — which is exactly how B1-F3's gap survived. Both doors
// call this one; neither can drift.
//
// RUNES, NOT BYTES, for the reason the title cap already gives: the message says
// "chars", and a CJK or emoji value well under the limit was refused with a byte
// count the operator could not reconcile with what they typed.
func (s *Server) validateRunTextFields(w http.ResponseWriter, req createRunRequest) bool {
	for _, f := range []runTextField{
		{name: "title", value: req.Title, max: maxRunTitleLen},
		{name: "description", value: req.Description, max: maxRunDescriptionLen, multiline: true},
		{name: "repo", value: req.Repo, max: maxRunRepoLen},
		{name: "devcontainer_repo", value: req.DevcontainerRepo, max: maxRunRepoLen},
		{name: "task", value: req.Task, max: maxRunTaskLen, multiline: true},
		{name: "agent", value: req.Agent, max: maxRunAgentLen},
	} {
		if n := utf8.RuneCountInString(f.value); n > f.max {
			writeError(w, http.StatusBadRequest, fmt.Sprintf(runFieldTooLongRefusal, f.name, n, f.max))
			return false
		}
		if !runFieldCharsAllowed(f.value, f.multiline) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf(runFieldControlCharRefusal, f.name))
			return false
		}
	}
	return true
}

// runFieldCharsAllowed is controlCharFree (permissions.go — C0, DEL and the C1
// range, the same hygiene every capability-grant field gets) with newline and
// tab exempted for the two prose fields.
func runFieldCharsAllowed(v string, multiline bool) bool {
	if multiline {
		return !strings.ContainsFunc(v, func(r rune) bool {
			return r != '\n' && r != '\t' && r != '\r' && unicode.IsControl(r)
		})
	}
	return controlCharFree(v)
}
