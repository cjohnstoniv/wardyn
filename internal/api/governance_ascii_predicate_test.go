// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestASCIIOnlySubjectRefusesWhatFoldsOntoASCII is R1, asserted against the one
// home of the predicate.
//
// The ASCII guard is the ORDER half of a security property, not a formatting
// nicety. types.CanonicalUserSubject runs it BEFORE strings.ToLower because
// ToLower does UNICODE case mapping: U+212A KELVIN SIGN folds to ASCII 'k' and
// U+0130 folds to ASCII 'i', so folding first let a crafted claim
// "Kim@Korp.com" resolve to "kim@korp.com" — the exact string another human's
// capability grants, governance assignment and drive allocation are written
// against, since every one of those columns is matched by `subject =
// ANY($1::text[])` exact equality.
//
// The predicate had two homes — oidc.ASCIIOnly and types.ASCIIOnlySubject,
// byte-identical bodies — and a parity test across them. There is one home now,
// so what this pins is the predicate's own answers, which is the half the
// parity loop could never state: a copy narrowed to printable ASCII (a DIFFERENT
// rule, one function away in the same file) flips the control-character rows.
func TestASCIIOnlySubjectRefusesWhatFoldsOntoASCII(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"", true},
		{"bob@corp.example", true},
		{"Kim@Korp.com", true},
		// Control characters are ascii, and this predicate says so. It answers
		// exactly one question — "can ToLower move a rune across the ASCII
		// boundary" — and a copy that also refused control characters would be
		// enforcing PrintableASCII's rule under this one's name, silently
		// rejecting subjects the write boundary accepts.
		{"\x00nul", true},
		{"\x7fdel", true},
		{"eng\tteam", true},
		// The escalation itself: each of these folds to a pure-ASCII string
		// under strings.ToLower, which is why the guard has to run first.
		// Written as escapes rather than literals so the bytes under test are
		// not at the mercy of an editor's normalization.
		{"\u212Aernel-team", false}, // U+212A KELVIN SIGN -> 'k'
		{"\u0130nfra", false},       // U+0130 LATIN CAPITAL I WITH DOT ABOVE -> 'i'
		{"caf\u00e9", false},
		{"\u0080c1", false},       // a C1 control: non-ASCII despite being a control
		{"bob\u00a0smith", false}, // NBSP
		{"\u0438\u043d\u0436", false},
	} {
		if got := types.ASCIIOnlySubject(tc.in); got != tc.want {
			t.Errorf("ASCIIOnlySubject(%+q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestCanonicalUserSubjectGuardsBeforeItFolds is the consequence, asserted at
// the site that depends on it: a non-ASCII subject is kept VERBATIM rather than
// folded, so it can only ever equal itself and inherits nobody's grants.
func TestCanonicalUserSubjectGuardsBeforeItFolds(t *testing.T) {
	// Folding this FIRST produces "kim@korp.com" — a real human's subject.
	// U+212A KELVIN SIGN, as an escape so the byte under test is unambiguous.
	const crafted = "\u212Aim@Korp.com"
	got := types.CanonicalUserSubject(crafted)
	if folded := strings.ToLower(crafted); got == folded {
		t.Fatalf("CanonicalUserSubject(%+q) = %+q, which is strings.ToLower's answer — the ASCII guard did not "+
			"run first, so a crafted claim folds onto the ASCII subject another human's grants are written "+
			"against", crafted, got)
	}
	if got != strings.TrimSpace(crafted) {
		t.Errorf("CanonicalUserSubject(%+q) = %+q, want it kept verbatim: dropping or rewriting a non-ASCII "+
			"identity would silently discard a DENY written against that human", crafted, got)
	}
	// The ASCII path still folds, or the guard would have disabled the rule it
	// is protecting.
	if got := types.CanonicalUserSubject("  Bob@Corp.Example  "); got != "bob@corp.example" {
		t.Errorf("CanonicalUserSubject = %q, want the trimmed, lowercased ASCII subject", got)
	}
}
