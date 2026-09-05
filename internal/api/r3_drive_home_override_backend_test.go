// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestDriveGrantHomeOverrideIsBackendChecked is F151.
//
// types.ValidateUserDriveGrant holds only the grant ROW, so it cannot see which
// substrate the drive it points at lands on, and it applies the DOCKER segment
// rule as the looser of the two. A k8s backend enforces DNS-1123 — no `_`, no
// trailing `-` or `.` — so an admin could save an allocation that every launch
// then refused: 201 plus a drive.grant.write audit row at write time,
// errDriveUnmountable and a 422 at every launch, user_drive_unavailable
// "unmountable" on /me, and no signal to the admin until a member complained.
//
// The system already knew the backend at write time; it just was not asked.
func TestDriveGrantHomeOverrideIsBackendChecked(t *testing.T) {
	// The three shapes docker accepts and DNS-1123 does not.
	bad := []string{"bob_smith", "bob-", "bob."}

	t.Run("a k8s drive refuses a docker-legal override at write time", func(t *testing.T) {
		for _, backend := range []types.DriveBackend{types.DriveBackendK8sPVC, types.DriveBackendK8sPVCStatic} {
			for _, override := range bad {
				st := newDriveCRUDStore()
				srv, _ := driveAdminServer(st, nil)
				d := *driveFixture(func(d *types.UserDrive) { d.Backend = backend })
				st.drives[d.ID] = d

				body, err := json.Marshal(map[string]any{
					"subject_type": types.CapabilitySubjectUser, "subject": "sub-bob",
					"drive_id": d.ID.String(), "home_override": override,
				})
				if err != nil {
					t.Fatal(err)
				}
				w := driveCall(t, srv.handleUpsertUserDriveGrant, http.MethodPost, "/api/v1/drives/grants", string(body), nil)
				if w.Code != http.StatusBadRequest {
					t.Errorf("backend=%s home_override=%q -> %d, want 400 — this allocation resolves to "+
						"errDriveUnmountable at EVERY launch, and the admin's only signal was a member complaining; body=%s",
						backend, override, w.Code, w.Body.String())
					continue
				}
				// The refusal has to name THIS backend's rule, or the admin
				// retypes the same class of name.
				if got := w.Body.String(); !strings.Contains(got, "DNS-1123") {
					t.Errorf("backend=%s: refusal does not name the backend's rule: %s", backend, got)
				}
				if len(st.grants) != 0 {
					t.Errorf("backend=%s home_override=%q was STORED despite the refusal", backend, override)
				}
			}
		}
	})

	// The control that keeps this from being "reject everything": the same
	// override on a docker backend is legal and still goes through, and a
	// DNS-1123-legal name goes through on k8s.
	t.Run("a legal override still writes", func(t *testing.T) {
		for _, tc := range []struct {
			backend  types.DriveBackend
			override string
		}{
			{types.DriveBackendDockerVolume, "bob_smith"},
			{types.DriveBackendK8sPVC, "bob-smith"},
			{types.DriveBackendK8sPVCStatic, "bob.smith"},
		} {
			st := newDriveCRUDStore()
			srv, _ := driveAdminServer(st, nil)
			d := *driveFixture(func(d *types.UserDrive) { d.Backend = tc.backend })
			st.drives[d.ID] = d

			body, err := json.Marshal(map[string]any{
				"subject_type": types.CapabilitySubjectUser, "subject": "sub-bob",
				"drive_id": d.ID.String(), "home_override": tc.override,
			})
			if err != nil {
				t.Fatal(err)
			}
			w := driveCall(t, srv.handleUpsertUserDriveGrant, http.MethodPost, "/api/v1/drives/grants", string(body), nil)
			if w.Code != http.StatusCreated {
				t.Errorf("backend=%s home_override=%q -> %d, want 201 — this name IS valid for that backend; body=%s",
					tc.backend, tc.override, w.Code, w.Body.String())
			}
		}
	})

	// A grant with NO override must not pay for a drive read it does not need,
	// and must keep working when the drive row is unreadable for any other
	// reason — the check is scoped to the field it is about.
	t.Run("a grant with no override is unaffected", func(t *testing.T) {
		st := newDriveCRUDStore()
		srv, _ := driveAdminServer(st, nil)
		d := *driveFixture(func(d *types.UserDrive) { d.Backend = types.DriveBackendK8sPVC })
		st.drives[d.ID] = d
		w := driveCall(t, srv.handleUpsertUserDriveGrant, http.MethodPost, "/api/v1/drives/grants",
			`{"subject_type":"user","subject":"sub-bob","drive_id":"`+d.ID.String()+`"}`, nil)
		if w.Code != http.StatusCreated {
			t.Fatalf("a grant with no home_override -> %d, want 201; body=%s", w.Code, w.Body.String())
		}
	})
}
