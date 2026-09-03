// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// group_subject_boundary_test.go pins ONE rule across the two 0.7 tables that
// are written against a `group` subject: a subject the login-time snapshot can
// never carry must be REFUSED at the write boundary, not stored.
//
// Both columns are matched by exact string equality against Session.Groups
// (store_capabilities.go's `subject = ANY($2::text[])`, and the same shape in
// ResolveGovernanceProfile), and that snapshot is strictly narrower than
// "lowercase it" — oidc.sessionGroups carries printable ASCII only, checked
// BEFORE the Unicode fold. A subject outside that set is a row that matches
// nobody: a group DENY that protects nothing while the Permissions screen
// renders it as active, and a group-tier assignment that HasGroupTierAssignments
// still counts as present (so every unanswerable-snapshot caller is refused
// 403 groups_snapshot_stale on account of an assignment that could never have
// applied to them).
//
// The two validators share oidc.CanonicalGroupSubject with the snapshot itself
// so they cannot drift again, and the last case here is what proves it: every
// accepted subject is re-derived through the real snapshot normalizer.
package api

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestGroupSubjectWriteBoundariesShareTheSnapshotRule(t *testing.T) {
	cases := []struct {
		name    string
		subject string
		want    string // the stored subject; "" means the write must be refused
	}{
		{"plain ASCII", "eng-team", "eng-team"},
		{"trimmed and lowercased", "  Eng-Team  ", "eng-team"},
		{"an Entra App Role value", "Wardyn.Contractors", "wardyn.contractors"},
		// A directory that names groups in a non-English locale is ordinary,
		// and these are the rows that used to be stored permanently inert.
		{"non-ASCII group name (de)", "Entwickler-Büro", ""},
		{"non-ASCII group name (fr)", "équipe-fr", ""},
		{"non-ASCII group name (ru)", "инженеры", ""},
		// The fold-escalating pair: strings.ToLower maps U+212A to ASCII 'k'
		// and U+0130 to ASCII 'i', so a guard that ran after the fold would
		// accept these and store a DIFFERENT, ASCII subject — binding a deny or
		// a ceiling to a real group the operator never named.
		{"folds onto an ASCII group (U+212A)", "Kubernetes-admins", ""},
		{"folds onto an ASCII group (U+0130)", "İnfra", ""},
		{"control character", "eng\tteam", ""},
		{"empty", "   ", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			grant := types.CapabilityGrant{
				SubjectType: types.CapabilitySubjectGroup, Subject: tc.subject,
				Capability: capSecret, Value: "prod-db", Effect: types.CapabilityDeny,
			}
			grantErr := validateCapabilityGrant(&grant)

			assign := types.GovernanceAssignment{
				SubjectType: types.CapabilitySubjectGroup, Subject: tc.subject,
				ProfileID: uuid.New(),
			}
			assignErr := validateGovernanceAssignment(&assign)

			// The two tables share a subject vocabulary; a value one accepts
			// and the other refuses is the drift this pin exists to catch.
			if (grantErr == nil) != (assignErr == nil) {
				t.Fatalf("the two write boundaries disagree on %+q: grant err=%v, assignment err=%v",
					tc.subject, grantErr, assignErr)
			}

			if tc.want == "" {
				if grantErr == nil {
					t.Fatalf("both boundaries ACCEPTED %+q (stored as %+q / %+q) — no session snapshot can carry it, so the row matches nobody",
						tc.subject, grant.Subject, assign.Subject)
				}
				return
			}
			if grantErr != nil {
				t.Fatalf("both boundaries refused %+q: %v — a group name the snapshot does carry must be writable", tc.subject, grantErr)
			}
			if grant.Subject != tc.want || assign.Subject != tc.want {
				t.Fatalf("stored subject = %q (grant) / %q (assignment), want %q", grant.Subject, assign.Subject, tc.want)
			}
			// The accepted form must be BYTE-FOR-BYTE what a login would put in
			// the snapshot — the property that makes exact-equality matching
			// sound, and the one a second hand-written normalizer would break.
			// oidc.CanonicalGroupSubject is the snapshot's own normalizer;
			// TestCanonicalGroupSubjectIsTheSnapshotRule (internal/auth/oidc)
			// pins it against sessionGroups itself, which this package cannot
			// reach.
			canon, ok := oidc.CanonicalGroupSubject(tc.subject)
			if !ok || canon != tc.want {
				t.Fatalf("write boundary stored %q but the snapshot normalizer answers (%q, %v) for the same claim — the two surfaces have drifted", tc.want, canon, ok)
			}
		})
	}
}

// TestGroupSubjectRefusalNamesTheReason: the refusal is the operator's only
// signal that the group they typed is unwritable, so it has to say what the
// rule is rather than the bare "subject: invalid" a length/control-char reject
// gives. Both boundaries use the same sentence.
func TestGroupSubjectRefusalNamesTheReason(t *testing.T) {
	grant := types.CapabilityGrant{
		SubjectType: types.CapabilitySubjectGroup, Subject: "Entwickler-Büro",
		Capability: capSecret, Value: "prod-db", Effect: types.CapabilityDeny,
	}
	err := validateCapabilityGrant(&grant)
	if err == nil {
		t.Fatal("accepted a non-ASCII group subject")
	}
	for _, want := range []string{"subject:", "printable ASCII", "group snapshot"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}
