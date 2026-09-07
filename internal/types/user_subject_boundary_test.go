// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

// THE USER-SUBJECT WRITE BOUNDARY, AND THE FOLD IT MUST NOT PERFORM.
//
// internal/api's group_subject_boundary_test.go pins the same property for GROUP
// subjects across the validators that write them. The user-drive grant is the
// third write boundary of the user-subject set and the one that lives here, in
// internal/types — and it was the one still folding with a bare strings.ToLower.
//
// The escalation is not theoretical and not about aesthetics. strings.ToLower
// performs UNICODE case mapping: KELVIN SIGN U+212A maps to ASCII 'k', and
// U+0130 (LATIN CAPITAL LETTER I WITH DOT ABOVE) maps to a string beginning with
// ASCII 'i'. So an admin who types a look-alike spelling of a real human's
// address had it folded ONTO that human's subject and the allocation stored
// against them — a drive handed to somebody nobody named, with the audit row
// showing the exotic string that was actually typed. Guard the RAW value, fold
// second, and there is nothing to fold onto.
//
// The opposite direction is pinned just as hard: a non-ASCII subject is a real
// person's identity, not an operator-authored label, so it is kept exactly as
// typed. Refusing it would refuse somebody.

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// lookAlikeSubjects are spellings whose Unicode fold lands on an ASCII subject a
// real human could hold. The ASCII form is what the session surface carries.
var lookAlikeSubjects = []struct{ typed, foldsOnto string }{
	{"Kim@corp.example", "kim@corp.example"}, // KELVIN SIGN -> 'k'
	{"Kelly@corp.example", "kelly@corp.example"},
	{"İvan@corp.example", "ivan@corp.example"}, // I WITH DOT ABOVE -> 'i'...
}

func TestCanonicalUserSubjectDoesNotFoldALookAlikeOntoARealPerson(t *testing.T) {
	for _, tc := range lookAlikeSubjects {
		// The premise, asserted rather than assumed: a BARE lowercase really
		// does land on the ASCII subject. Without this the test could pass
		// against an input that was never dangerous.
		if got := strings.ToLower(tc.typed); !strings.HasPrefix(got, tc.foldsOnto[:1]) {
			t.Fatalf("strings.ToLower(%q) = %q, which no longer begins with %q — pick a live look-alike, because "+
				"this one no longer demonstrates the escalation", tc.typed, got, tc.foldsOnto[:1])
		}
		if got := CanonicalUserSubject(tc.typed); got == tc.foldsOnto {
			t.Errorf("CanonicalUserSubject(%q) = %q: the look-alike an admin typed was folded ONTO a real person's "+
				"subject, so the drive is allocated to them and the audit row shows a string that is not what the "+
				"grant now targets", tc.typed, got)
		}
	}
}

func TestCanonicalUserSubjectFoldsASCIIAndKeepsTheRest(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// ASCII still folds, so nothing an admin types in the ordinary case
		// changes: the session surface lowercases too, and a row that did not
		// fold would match nobody.
		{"Kim@Corp.Example", "kim@corp.example"},
		{"  Kim@Corp.Example  ", "kim@corp.example"},
		{"a1b2c3d4-1111-2222-3333-444455556666", "a1b2c3d4-1111-2222-3333-444455556666"},
		{"", ""},
		// A non-ASCII identity is kept EXACTLY as typed: unfolded, because
		// folding is what makes a look-alike dangerous, and unrefused, because
		// a `sub` claim is whatever the identity provider issues.
		{"Kim@corp.example", "Kim@corp.example"},
		{" İvan@corp.example ", "İvan@corp.example"},
		{"Ünïcode@corp.example", "Ünïcode@corp.example"},
	} {
		if got := CanonicalUserSubject(tc.in); got != tc.want {
			t.Errorf("CanonicalUserSubject(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestValidateUserDriveGrantAppliesTheUserRule drives the WRITE BOUNDARY itself,
// not only the helper: the fix is worth nothing if the validator stops calling
// it, and that is the shape a refactor breaks silently.
func TestValidateUserDriveGrantAppliesTheUserRule(t *testing.T) {
	grantFor := func(subject string) UserDriveGrant {
		return UserDriveGrant{
			DriveID:     uuid.New(),
			SubjectType: CapabilitySubjectUser,
			Subject:     subject,
		}
	}

	for _, tc := range lookAlikeSubjects {
		g := grantFor(tc.typed)
		if err := ValidateUserDriveGrant(&g); err != nil {
			t.Fatalf("ValidateUserDriveGrant(%q) = %v; a non-ASCII subject must be KEPT, not refused — refusing one "+
				"refuses a real person whose provider issues it", tc.typed, err)
		}
		if g.Subject == tc.foldsOnto {
			t.Errorf("the grant normalized %q to %q: this allocation is now written against a real human the admin "+
				"never named, and nothing downstream can tell it apart from one they did", tc.typed, g.Subject)
		}
		if g.Subject != tc.typed {
			t.Errorf("the grant normalized %q to %q, want it kept verbatim", tc.typed, g.Subject)
		}
	}

	// The ordinary ASCII case is unchanged, in both directions — this is what a
	// fix that simply stopped normalizing would break.
	g := grantFor("  Kim@Corp.Example  ")
	if err := ValidateUserDriveGrant(&g); err != nil {
		t.Fatalf("ValidateUserDriveGrant on an ASCII subject: %v", err)
	}
	if g.Subject != "kim@corp.example" {
		t.Errorf("an ASCII user subject normalized to %q, want %q — the session surface lowercases, so a row that "+
			"did not fold would match nobody", g.Subject, "kim@corp.example")
	}

	// And the GROUP arm is untouched: it still REFUSES non-ASCII, because a
	// group subject is matched against a snapshot that carries printable ASCII
	// only. The two arms answer differently on purpose.
	gg := grantFor("Kubernetes-admins")
	gg.SubjectType = CapabilitySubjectGroup
	if err := ValidateUserDriveGrant(&gg); err == nil {
		t.Errorf("a non-ASCII GROUP subject was accepted as %q; the group arm's refusal is what stops a crafted "+
			"claim folding onto an operator-authored group", gg.Subject)
	}
}
