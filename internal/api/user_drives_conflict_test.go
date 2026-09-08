// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// TestWriteUserDriveTellsItsThree409sApart is R1 F177 and F284.
//
// UpsertUserDrive returns three different conflicts and all three WRAP
// store.ErrConflict, so the status was already right and the sentence was not:
// both specific ones fell through to "a user drive named %q already exists" —
// for a name that is FREE. An admin told that goes looking for a row that is not
// there, which is worse than a vague refusal because it is a confident wrong
// direction. store.ErrDriveHomeNamespaceConflict's own doc says it exists so
// "the ONE caller that writes the sentence can tell this refusal apart"; until
// now no caller did.
//
// Each arm asserts the REMEDY, not just the status, because the status was never
// the defect.
func TestWriteUserDriveTellsItsThree409sApart(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		err        error
	}{
		{
			// F284. The name is free; what is taken is types.DriveSlug(name),
			// the fragment every minted object name is built from.
			name: "a name that folds onto another drive's storage object",
			err:  store.ErrDriveSlugConflict,
			want: "folds to the same storage-object name",
		},
		{
			// F177. Two host_path drives over one host_root deriving homes by
			// different rules allocate two members the same directory.
			name: "two shares over one host root with different home rules",
			err:  store.ErrDriveHomeNamespaceConflict,
			want: "derives home directory names by a different rule",
		},
		{
			// THE CONTROL, and it is what makes the other two mean anything: the
			// UNIQUE(name) refusal still says the name is taken, because for it
			// that is true and it is the remedy.
			name: "a name another drive really does hold",
			err:  store.ErrConflict,
			want: `already exists`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newDriveCRUDStore()
			st.upsertErr = tc.err
			srv, _ := driveAdminServer(st, nil)

			w := driveCall(t, srv.handleCreateUserDrive, http.MethodPost, "/api/v1/drives", driveCreateBody, nil)
			if w.Code != http.StatusConflict {
				t.Fatalf("code = %d, want 409 — all three wrap store.ErrConflict, so the status was never the "+
					"defect: %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Errorf("body = %s\nwant it to carry %q — the three conflicts have three different remedies, "+
					"and a refusal that names the wrong one sends an admin to fix something that is not broken",
					w.Body.String(), tc.want)
			}
			// …and the two specific arms must NOT reach the name-taken
			// sentence, which is the exact regression: the name is free.
			if tc.err != store.ErrConflict && strings.Contains(w.Body.String(), "already exists") {
				t.Errorf("body = %s — this refusal is not about the name, and the drive it points at is a row "+
					"the admin will not find: the name is free", w.Body.String())
			}
		})
	}
}
