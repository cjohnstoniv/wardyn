// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// user_subject_boundary_test.go is the USER half of what
// group_subject_boundary_test.go pins for a group: the ASCII guard runs on the
// RAW identity, BEFORE the Unicode fold, on BOTH sides of the match — the
// caller's own subject list (capabilitySubjects) and every surface that writes
// a `user` subject row.
//
// strings.ToLower does Unicode case mapping: KELVIN SIGN U+212A folds to ASCII
// 'k' and U+0130 to ASCII 'i'. A caller whose email claim is "Kim@Korp.com"
// (crafted K's) therefore resolved to the subject string "kim@korp.com" — the
// exact value another human's capability grants, governance assignment and
// drive allocation are written against, matched by `subject = ANY($1::text[])`
// equality. The premise that let it stand is stated in the tree twice ("A plain
// ToLower is the WHOLE rule for a user subject"), which is why this pin asserts
// the rule rather than one call site.
package api

import (
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	victimEmail  = "kim@korp.com"
	craftedEmail = "Kim@Korp.com" // KELVIN SIGN twice
	victimSub    = "infra-lead"
	craftedSub   = "İnfra-lead" // LATIN CAPITAL I WITH DOT ABOVE
)

// TestCapabilitySubjectsDoesNotFoldOntoAnotherHuman is the read side: the
// escalation the finding executed.
func TestCapabilitySubjectsDoesNotFoldOntoAnotherHuman(t *testing.T) {
	t.Run("a crafted email claim", func(t *testing.T) {
		users, _, _ := capabilitySubjects(operatorCtx("attacker-sub", craftedEmail, string(oidc.RoleMember)))
		for _, got := range users {
			if got == victimEmail {
				t.Fatalf("capabilitySubjects returned %q for a caller whose email claim is %+q — every `user` grant, "+
					"governance assignment and drive allocation written against that human now matches this caller.\nusers=%+q",
					victimEmail, craftedEmail, users)
			}
		}
		// The identity is not DROPPED either: a deny written against the
		// caller's own claim string has to keep biting.
		if !contains(users, craftedEmail) {
			t.Errorf("capabilitySubjects dropped the caller's own email %+q; a subject that cannot escalate must still be "+
				"matchable, or a DENY written against this human evaporates.\nusers=%+q", craftedEmail, users)
		}
	})

	t.Run("a crafted sub claim", func(t *testing.T) {
		users, _, _ := capabilitySubjects(operatorCtx(craftedSub, "attacker@corp.example", string(oidc.RoleMember)))
		for _, got := range users {
			if got == victimSub {
				t.Fatalf("capabilitySubjects returned %q for a caller whose sub is %+q.\nusers=%+q", victimSub, craftedSub, users)
			}
		}
	})

	// The control: ASCII case folding is the intended rule and must survive.
	t.Run("an ASCII identity still folds", func(t *testing.T) {
		users, _, _ := capabilitySubjects(operatorCtx("Alice", "  Alice@Corp.Example  ", string(oidc.RoleMember)))
		for _, want := range []string{"alice", "alice@corp.example"} {
			if !contains(users, want) {
				t.Errorf("capabilitySubjects lost %q — a grant written \"Alice@Corp.Example\" must still hit this caller.\nusers=%+q",
					want, users)
			}
		}
	})
}

// TestUserSubjectWriteBoundariesShareTheReadRule: what a caller can BE is what
// an admin can WRITE. A write surface that folds where the read surface guards
// stores a row against a subject its author never named.
func TestUserSubjectWriteBoundariesShareTheReadRule(t *testing.T) {
	cases := []struct {
		name    string
		subject string
		want    string
	}{
		{"plain ASCII", "kim@korp.com", "kim@korp.com"},
		{"trimmed and lowercased", "  Kim@Korp.com  ", "kim@korp.com"},
		{"an opaque IdP sub", "AAD-9F2C", "aad-9f2c"},
		// The fold-escalating pair: stored as the VICTIM's subject before this
		// rule, so the row bound a deny/profile/drive to a human the author
		// never named.
		{"folds onto an ASCII human (U+212A)", craftedEmail, craftedEmail},
		{"folds onto an ASCII human (U+0130)", craftedSub, craftedSub},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			grant := types.CapabilityGrant{
				SubjectType: types.CapabilitySubjectUser, Subject: tc.subject,
				Capability: capSecret, Value: "prod-db", Effect: types.CapabilityDeny,
			}
			if err := validateCapabilityGrant(&grant); err != nil {
				t.Fatalf("validateCapabilityGrant(%+q) = %v", tc.subject, err)
			}
			if grant.Subject != tc.want {
				t.Errorf("capability grant subject = %+q, want %+q — a user subject is folded only when the RAW value is "+
					"ASCII; folding first lets a crafted spelling land on another human's row", grant.Subject, tc.want)
			}

			assign := types.GovernanceAssignment{
				SubjectType: types.CapabilitySubjectUser, Subject: tc.subject, ProfileID: uuid.New(),
			}
			if err := validateGovernanceAssignment(&assign); err != nil {
				t.Fatalf("validateGovernanceAssignment(%+q) = %v", tc.subject, err)
			}
			if assign.Subject != tc.want {
				t.Errorf("governance assignment subject = %+q, want %+q", assign.Subject, tc.want)
			}

			// ONE RULE, both sides: whatever a caller carrying this identity
			// resolves to is exactly what an admin naming it stores.
			users, _, _ := capabilitySubjects(operatorCtx(tc.subject, "", string(oidc.RoleMember)))
			if len(users) != 1 || users[0] != tc.want {
				t.Errorf("the read side resolves %+q to %+q while the write side stores %+q — the two halves of one match "+
					"disagree", tc.subject, users, tc.want)
			}
		})
	}
}

func contains(list []string, want string) bool {
	return slices.Contains(list, want)
}
