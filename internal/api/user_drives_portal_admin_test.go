// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
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

// rehomeFixture is the portal admin fixture holding portals portal allocations and people group allocations of its
// team drive, with a PUT caller that answers the drive's own body with a new name and home template.
func rehomeFixture(t *testing.T, portals, people int) (*portalGrantStore, *recRecorder, types.UserDrive, func(name, query string) *httptest.ResponseRecorder) {
	t.Helper()
	srv, st, d := portalAdminFixture(t)
	// A static claim, which accepts both the hash and the sub home template, on a deployment that dispatches to Kubernetes.
	d.Backend = types.DriveBackendK8sPVCStatic
	st.drives[d.ID] = d
	srv.cfg.RunnerTarget = d.Backend.RunnerTarget()
	rec := &recRecorder{}
	srv.cfg.Audit = rec
	for range portals {
		g := types.UserDriveGrant{ID: uuid.New(), SubjectType: types.DriveSubjectPortal, Subject: uuid.NewString(), DriveID: d.ID, Enabled: true}
		st.grants[g.ID] = g
	}
	for i := range people {
		g := types.UserDriveGrant{ID: uuid.New(), SubjectType: types.CapabilitySubjectGroup, Subject: fmt.Sprintf("grp-%d", i), DriveID: d.ID, Enabled: true}
		st.grants[g.ID] = g
	}
	return st, rec, d, func(name, query string) *httptest.ResponseRecorder {
		return driveCall(t, srv.handleUpdateUserDrive, http.MethodPut, "/api/v1/drives/"+d.ID.String()+query,
			`{"name":"`+name+`","backend":"k8s_pvc_static","team":true,"home_template":"sub"}`, map[string]string{"id": d.ID.String()})
	}
}

// A drive held by portals alone is edited without a re-home confirmation: its folders are named by the portal and
// drive ids, so a rename and a home template change move none of them, and the audit row claims no move.
func TestPortalDriveRehome_PortalOnlyEditIsAcknowledged(t *testing.T) {
	st, rec, d, put := rehomeFixture(t, 2, 0)
	w := put("Team Data Renamed", "")
	if w.Code != http.StatusOK {
		t.Fatalf("a rename and template change of a portal-only drive = %d %s, want 200 with no confirmation", w.Code, w.Body)
	}
	if got := st.drives[d.ID]; got.Name != "Team Data Renamed" || got.HomeTemplate != types.HomeTemplateSub {
		t.Errorf("stored drive = %+v, want the edit written", got)
	}
	data := driveAuditData(t, rec, "drive.write")
	if data["rehomed"] != false || data["rehomed_subjects"] != nil || data["rehomed_fields"] != nil {
		t.Errorf("drive.write = %v, want rehomed false with no subjects or fields", data)
	}
}

// With people and portals on one drive, only the people's allocations are re-homed: the refusal and the confirmed
// audit row count them and name the fields, never the portals.
func TestPortalDriveRehome_MixedGrantsCountOnlyWhatMoves(t *testing.T) {
	st, rec, d, put := rehomeFixture(t, 2, 1)
	w := put("Team Data Renamed", "")
	if w.Code != http.StatusConflict || errorReason(w) != reasonUserDriveRehomeInvalid ||
		!strings.Contains(w.Body.String(), "allocated to 1 subject and this change re-homes it") {
		t.Fatalf("mixed edit = %d %s, want 409 naming 1 subject", w.Code, w.Body)
	}
	if st.drives[d.ID].Name != d.Name {
		t.Errorf("the refused edit wrote %q", st.drives[d.ID].Name)
	}
	if w = put("Team Data Renamed", "?confirm=rehome"); w.Code != http.StatusOK {
		t.Fatalf("confirmed mixed edit = %d %s, want 200", w.Code, w.Body)
	}
	data := driveAuditData(t, rec, "drive.write")
	if data["rehomed"] != true || data["rehomed_subjects"] != float64(1) {
		t.Errorf("drive.write = %v, want rehomed true for 1 subject", data)
	}
}

// What moves, by allocation kind: a person or group's folder is named by all five identity fields, a portal's by the
// backend alone, so a backend change moves every allocation and the rest move the people only.
func TestPortalDriveRehome_AllocationsThatMove(t *testing.T) {
	name, backend := `name "a" → "b"`, `backend "docker_volume" → "k8s_pvc_static"`
	for _, c := range []struct {
		label          string
		people, portal int
		changes        []string
		wantMoved      int
		wantChanges    []string
	}{
		{"portals only, a rename", 0, 2, []string{name}, 0, nil},
		{"portals only, a backend change", 0, 2, []string{name, backend}, 2, []string{backend}},
		{"people and portals, a rename", 1, 2, []string{name}, 1, []string{name}},
		{"people and portals, a backend change", 1, 2, []string{name, backend}, 3, []string{name, backend}},
		{"people only, a rename", 2, 0, []string{name}, 2, []string{name}},
	} {
		item := types.UserDriveListItem{GrantCount: c.people + c.portal, PortalGrantCount: c.portal}
		moved, changes := driveRehomedAllocations(item, c.changes)
		if moved != c.wantMoved || !reflect.DeepEqual(changes, c.wantChanges) {
			t.Errorf("%s: moved %d %v, want %d %v", c.label, moved, changes, c.wantMoved, c.wantChanges)
		}
	}
}
