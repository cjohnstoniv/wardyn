// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// portalGrantStore is driveCRUDStore with the additive allocation write, which answers addErr when it is set.
type portalGrantStore struct {
	*driveCRUDStore
	addErr error
	added  []types.UserDriveGrant
	// readsAfterWrite counts the drive reads attempted once a portal row was committed.
	readsAfterWrite int
}

func (s *portalGrantStore) AddUserDriveGrant(_ context.Context, g types.UserDriveGrant, _ bool) (types.UserDriveGrant, error) {
	if s.addErr != nil {
		return types.UserDriveGrant{}, s.addErr
	}
	s.added = append(s.added, g)
	return g, nil
}

// GetUserDrive answers like driveCRUDStore's until a portal row has been written; after that it fails, as a database
// that dropped between the commit and the answer would.
func (s *portalGrantStore) GetUserDrive(ctx context.Context, id uuid.UUID) (types.UserDrive, error) {
	if len(s.added) > 0 {
		s.readsAfterWrite++
		return types.UserDrive{}, errors.New("database went away after the commit")
	}
	return s.driveCRUDStore.GetUserDrive(ctx, id)
}

func (s *portalGrantStore) ReplaceUserDriveGrant(context.Context, types.UserDriveGrant, bool) (types.UserDriveGrant, []uuid.UUID, error) {
	return types.UserDriveGrant{}, nil, store.ErrDrivePortalAdditiveOnly
}

func portalAdminFixture(t *testing.T) (*Server, *portalGrantStore, types.UserDrive) {
	t.Helper()
	st := &portalGrantStore{driveCRUDStore: newDriveCRUDStore()}
	st.site = teamDrivesSite(true)
	d, _ := teamDrive()
	st.drives[d.ID] = d
	srv, _ := driveAdminServer(st.driveCRUDStore, nil)
	srv.cfg.Store = st
	return srv, st, d
}

func portalAllocBody(drive types.UserDrive, portal string, extra string) string {
	return `{"subject_type":"portal","subject":"` + portal + `","drive_id":"` + drive.ID.String() + `"` + extra + `}`
}

// Only a super admin allocates a drive to a portal, the allocation is additive, and the answer carries the object
// name the folder will have.
func TestPortalDriveAllocation_SuperAdminOnly(t *testing.T) {
	srv, st, d := portalAdminFixture(t)
	portal := uuid.New()
	body := portalAllocBody(d, portal.String(), `,"additional":true`)

	w := driveGrantAs(t, srv, driveSecurityAdminCtx(), body)
	if w.Code != http.StatusForbidden || errorReason(w) != string(authz.ReasonAdminSurface) || len(st.added) != 0 {
		t.Fatalf("security admin = %d %s (%d rows), want 403 admin_surface and no row", w.Code, w.Body, len(st.added))
	}
	w = driveGrantAs(t, srv, driveSuperAdminCtx(), portalAllocBody(d, portal.String(), ""))
	if w.Code != http.StatusBadRequest || len(st.added) != 0 {
		t.Fatalf("super admin without additional = %d %s, want 400 and no row", w.Code, w.Body)
	}
	w = driveGrantAs(t, srv, driveSuperAdminCtx(), body)
	if w.Code != http.StatusCreated {
		t.Fatalf("super admin = %d %s, want 201", w.Code, w.Body)
	}
	var out struct {
		ObjectName string `json:"object_name"`
		Subject    string `json:"subject"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if want := types.DrivePortalObjectName(d, types.DrivePortalHome(d, portal)); out.ObjectName != want || out.Subject != portal.String() {
		t.Errorf("answer = %+v, want object_name %q for portal %s", out, want, portal)
	}
	if len(st.added) != 1 || st.added[0].SubjectType != types.DriveSubjectPortal {
		t.Errorf("rows = %+v, want one portal row", st.added)
	}
}

// A committed allocation is answered and audited with no further database read: the object name is a function of the
// portal and the drive's id, so a store that fails every read after the commit still gets its 201 and its
// drive.grant.write row.
func TestPortalDriveAllocation_CommittedGrantNeedsNoSecondRead(t *testing.T) {
	srv, st, d := portalAdminFixture(t)
	audit := &recRecorder{}
	srv.cfg.Audit = audit
	portal := uuid.New()
	w := driveGrantAs(t, srv, driveSuperAdminCtx(), portalAllocBody(d, portal.String(), `,"additional":true`))
	if w.Code != http.StatusCreated {
		t.Fatalf("allocation with a failing post-commit read = %d %s, want 201", w.Code, w.Body)
	}
	if st.readsAfterWrite != 0 {
		t.Errorf("%d drive reads after the commit, want 0", st.readsAfterWrite)
	}
	var out struct {
		ObjectName string `json:"object_name"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if want := types.DrivePortalObjectName(d, types.DrivePortalHome(d, portal)); out.ObjectName != want {
		t.Errorf("object_name = %q, want %q", out.ObjectName, want)
	}
	var writes int
	for _, ev := range audit.events {
		if ev.Action == "drive.grant.write" {
			writes++
		}
	}
	if writes != 1 {
		t.Errorf("drive.grant.write rows = %d, want 1 (events %+v)", writes, audit.events)
	}
}

// Each refusal of a portal allocation is its own status and reason, writes no row, and a subject that is not a
// canonical lower-case uuid is refused at the shape check.
func TestPortalDriveAllocation_TeamDriveAndIssuerPortalOnly(t *testing.T) {
	srv, st, d := portalAdminFixture(t)
	super := driveSuperAdminCtx()
	for _, c := range []struct {
		name   string
		err    error
		status int
		reason string
	}{
		{"not a team drive", store.ErrDrivePortalNotTeam, http.StatusBadRequest, reasonUserDriveGrantInvalid},
		{"a host path drive", store.ErrDrivePortalBackend, http.StatusUnprocessableEntity, reasonDrivePortalBackend},
		{"a drive with a second place", store.ErrDrivePortalSecondPlace, http.StatusUnprocessableEntity, reasonDrivePortalSecondPlace},
		{"a console portal", store.ErrDrivePortalNotIssuer, http.StatusUnprocessableEntity, reasonDrivePortalNotIssuerPortal},
		{"a revoked or unknown portal", store.ErrDrivePortalNotLive, http.StatusUnprocessableEntity, reasonDrivePortalNotLive},
	} {
		st.addErr = c.err
		w := driveGrantAs(t, srv, super, portalAllocBody(d, uuid.NewString(), `,"additional":true`))
		if w.Code != c.status || errorReason(w) != c.reason {
			t.Errorf("%s = %d %s, want %d %s", c.name, w.Code, w.Body, c.status, c.reason)
		}
	}
	st.addErr = nil
	for name, subject := range map[string]string{
		"an upper-case id": "ABCDEFAB-0000-0000-0000-000000000000", "a name": "platform", "an empty subject": "",
	} {
		if w := driveGrantAs(t, srv, super, portalAllocBody(d, subject, `,"additional":true`)); w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d %s, want 400", name, w.Code, w.Body)
		}
	}
	if len(st.added) != 0 {
		t.Errorf("refused allocations wrote %+v", st.added)
	}
}

// A drive edit the store refuses because a portal holds the drive is a 422 with its own reason.
func TestPortalDriveAllocation_DriveEditRefusedWhileHeld(t *testing.T) {
	for _, c := range []struct {
		err    error
		reason string
	}{{store.ErrDrivePortalBackend, reasonDrivePortalBackend}, {store.ErrDrivePortalSecondPlace, reasonDrivePortalSecondPlace}} {
		srv, st, d := portalAdminFixture(t)
		st.upsertErr = c.err
		w := driveCall(t, srv.handleUpdateUserDrive, http.MethodPut, "/api/v1/drives/"+d.ID.String(),
			`{"name":"Team Data","backend":"docker_volume","team":true}`, map[string]string{"id": d.ID.String()})
		if w.Code != http.StatusUnprocessableEntity || errorReason(w) != c.reason {
			t.Errorf("edit refused with %v = %d %s, want 422 %s", c.err, w.Code, w.Body, c.reason)
		}
	}
}

// An edit that would unmark a drive a portal holds as a team drive is a 409 with the re-home reason and writes nothing.
func TestPortalDriveAllocation_UnmarkTeamRefusedWhileHeld(t *testing.T) {
	srv, st, d := portalAdminFixture(t)
	st.upsertErr = store.ErrDrivePortalNotTeam
	w := driveCall(t, srv.handleUpdateUserDrive, http.MethodPut, "/api/v1/drives/"+d.ID.String(),
		`{"name":"Team Data","backend":"docker_volume","team":false}`, map[string]string{"id": d.ID.String()})
	if w.Code != http.StatusConflict || errorReason(w) != reasonUserDriveRehomeInvalid {
		t.Errorf("unmark refused = %d %s, want 409 %s", w.Code, w.Body, reasonUserDriveRehomeInvalid)
	}
}
