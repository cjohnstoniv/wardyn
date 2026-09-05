// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestDriveGrantGroupSubjectSharesTheSnapshotRule is F111's third boundary.
//
// group_subject_boundary_test.go pins the rule across the two 0.7 capability
// tables (validateCapabilityGrant, validateGovernanceAssignment). The drive
// allocation is the THIRD table written against a `group` subject and it was
// still on the old, looser rule — types.ValidateUserDriveGrant's plain
// strings.ToLower — because internal/types must not import internal/auth/oidc,
// so nothing carried the snapshot rule down there.
//
// TWO FAILURES, and the second is the one that matters. A group name the
// snapshot can never carry was stored VERBATIM and matched nobody: an
// allocation an admin sees on the allocations screen that no member will ever
// mount. And a plain ToLower FOLD-ESCALATES: U+212A KELVIN SIGN folds to ASCII
// 'k', U+0130 to ASCII 'i', so a crafted "Kubernetes-admins" was stored as the
// real ASCII group "kubernetes-admins" — binding a drive, with its size and
// writability, to a group the operator never named. That is a widening.
func TestDriveGrantGroupSubjectSharesTheSnapshotRule(t *testing.T) {
	for _, tc := range []struct {
		name    string
		subject string
		want    string // stored subject; "" means the write must be refused
	}{
		{"plain ASCII", "eng-team", "eng-team"},
		{"trimmed and lowercased", "  Eng-Team  ", "eng-team"},
		{"an Entra App Role value", "Wardyn.Contractors", "wardyn.contractors"},
		{"non-ASCII group name (fr)", "\u00e9quipe-fr", ""},
		{"non-ASCII group name (ru)", "\u0438\u043d\u0436\u0435\u043d\u0435\u0440\u044b", ""},
		{"folds onto an ASCII group (U+212A)", "\u212Aubernetes-admins", ""},
		{"folds onto an ASCII group (U+0130)", "\u0130nfra", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newDriveCRUDStore()
			srv, _ := driveAdminServer(st, nil)
			d := *driveFixture(nil)
			st.drives[d.ID] = d

			body, err := json.Marshal(map[string]any{
				"subject_type": types.CapabilitySubjectGroup,
				"subject":      tc.subject,
				"drive_id":     d.ID.String(),
			})
			if err != nil {
				t.Fatal(err)
			}
			w := driveCall(t, srv.handleUpsertUserDriveGrant, http.MethodPost, "/api/v1/drives/grants", string(body), nil)

			if tc.want == "" {
				if w.Code == http.StatusCreated || w.Code == http.StatusOK {
					var saved types.UserDriveGrant
					_ = json.Unmarshal(w.Body.Bytes(), &saved)
					t.Fatalf("ACCEPTED group subject %+q (stored as %+q) — no login snapshot can carry it, so the "+
						"allocation either matches nobody or, worse, folds onto a real ASCII group the operator "+
						"never named", tc.subject, saved.Subject)
				}
				if w.Code != http.StatusBadRequest {
					t.Fatalf("refused with %d, want 400; body=%s", w.Code, w.Body.String())
				}
				return
			}
			if w.Code != http.StatusCreated {
				t.Fatalf("refused %+q with %d — a group name the snapshot DOES carry must be writable; body=%s",
					tc.subject, w.Code, w.Body.String())
			}
			var saved types.UserDriveGrant
			if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil {
				t.Fatal(err)
			}
			if saved.Subject != tc.want {
				t.Fatalf("stored subject = %q, want %q", saved.Subject, tc.want)
			}
			// BYTE-FOR-BYTE what a login would put in the snapshot — the
			// property that makes the resolver's exact-equality matching sound,
			// and the one a second hand-written normalizer breaks.
			if canon, ok := oidc.CanonicalGroupSubject(tc.subject); !ok || canon != saved.Subject {
				t.Fatalf("stored %q but the snapshot normalizer answers (%q, %v) — the surfaces have drifted",
					saved.Subject, canon, ok)
			}
		})
	}

	// The USER tier is untouched: its subject is a sub or an email, matched
	// case-insensitively by capabilitySubjects, and an IdP sub is opaque — the
	// printable-ASCII rule is the GROUP snapshot's, not a general one.
	t.Run("a user subject keeps its own rule", func(t *testing.T) {
		st := newDriveCRUDStore()
		srv, _ := driveAdminServer(st, nil)
		d := *driveFixture(nil)
		st.drives[d.ID] = d
		w := driveCall(t, srv.handleUpsertUserDriveGrant, http.MethodPost, "/api/v1/drives/grants",
			`{"subject_type":"user","subject":"Jos\u00e9@corp.example","drive_id":"`+d.ID.String()+`"}`, nil)
		if w.Code != http.StatusCreated {
			t.Fatalf("a non-ASCII USER subject was refused with %d — the group snapshot's rule must not leak "+
				"onto the user tier, whose subject is an opaque sub or an email; body=%s", w.Code, w.Body.String())
		}
	})
}
