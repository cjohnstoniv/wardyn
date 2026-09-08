// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// asciiOnlyPredicates are the homes of the "is this string pure ASCII" rule.
//
// ONE LINE PER HOME, deliberately: the rule is duplicated, and the point of this
// guard is that a new copy costs one line to enrol and cannot then answer
// differently from the others. types.ASCIIOnlySubject is the second home and is
// commented out because it does not exist on this branch — it lands with lane
// core's internal/types/group_subject.go, whose body is byte-identical to
// oidc.ASCIIOnly's (`strings.IndexFunc(s, func(r rune) bool { return r >
// unicode.MaxASCII }) < 0`). Uncomment it at assembly; nothing else changes.
var asciiOnlyPredicates = map[string]func(string) bool{
	"oidc.ASCIIOnly":         oidc.ASCIIOnly,
	"types.ASCIIOnlySubject": types.ASCIIOnlySubject,
}

// TestASCIIOnlyPredicatesHaveOneAnswer is R1 F342.
//
// The ASCII guard is the ORDER half of a security property, not a formatting
// nicety. canonicalUserSubject (capabilities.go) runs it BEFORE strings.ToLower
// because ToLower does UNICODE case mapping: U+212A KELVIN SIGN folds to ASCII
// 'k' and U+0130 folds to ASCII 'i', so folding first let a crafted claim
// "Kim@Korp.com" resolve to "kim@korp.com" — the exact string another human's
// capability grants, governance assignment and drive allocation are written
// against, since every one of those columns is matched by `subject =
// ANY($1::text[])` exact equality.
//
// That property is only as good as the predicate, and the predicate has two
// homes for the same reason CanonicalGroupSubject does: internal/types is
// imported by pkg/client and the CLI, which must not acquire the server's
// OIDC/JOSE dependency stack, while internal/auth/oidc is the match surface.
// TestCanonicalGroupSubjectHasOneAnswer already guards the group rule's two
// homes; nothing guarded this one, so a copy could be widened — or narrowed to
// printable ASCII, which is a DIFFERENT rule living two functions away in the
// same file — with no test to notice.
func TestASCIIOnlyPredicatesHaveOneAnswer(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"", true},
		{"bob@corp.example", true},
		{"Kim@Korp.com", true},
		// CONTROL CHARACTERS ARE ASCII, and this predicate says so. It answers
		// exactly one question — "can ToLower move a rune across the ASCII
		// boundary" — and a copy that also refused control characters would be
		// enforcing printableASCII's rule under this one's name, silently
		// rejecting subjects the write boundary accepts.
		{"\x00nul", true},
		{"\x7fdel", true},
		{"eng\tteam", true},
		// THE ESCALATION ITSELF: each of these folds to a pure-ASCII string
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
		var answers []string
		for name, pred := range asciiOnlyPredicates {
			got := pred(tc.in)
			answers = append(answers, name+"="+boolWord(got))
			if got != tc.want {
				t.Errorf("%s(%+q) = %v, want %v", name, tc.in, got, tc.want)
			}
		}
		// …and they agree with EACH OTHER, which is the half a shared `want`
		// cannot state once a second home exists: two predicates can both be
		// wrong in the same direction and still match this table if it is ever
		// edited to follow them.
		for name, pred := range asciiOnlyPredicates {
			for other, otherPred := range asciiOnlyPredicates {
				if pred(tc.in) != otherPred(tc.in) {
					t.Errorf("%+q: %s and %s disagree (%s) — one rule, several hand-written copies, which is "+
						"the defect shape this guards", tc.in, name, other, strings.Join(answers, " "))
				}
			}
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
	got := canonicalUserSubject(crafted)
	if folded := strings.ToLower(crafted); got == folded {
		t.Fatalf("canonicalUserSubject(%+q) = %+q, which is strings.ToLower's answer — the ASCII guard did not "+
			"run first, so a crafted claim folds onto the ASCII subject another human's grants are written "+
			"against", crafted, got)
	}
	if got != strings.TrimSpace(crafted) {
		t.Errorf("canonicalUserSubject(%+q) = %+q, want it kept verbatim: dropping or rewriting a non-ASCII "+
			"identity would silently discard a DENY written against that human", crafted, got)
	}
	// The ASCII path still folds, or the guard would have disabled the rule it
	// is protecting.
	if got := canonicalUserSubject("  Bob@Corp.Example  "); got != "bob@corp.example" {
		t.Errorf("canonicalUserSubject = %q, want the trimmed, lowercased ASCII subject", got)
	}
}

func boolWord(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
